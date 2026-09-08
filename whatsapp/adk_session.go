package whatsapp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/session"
)

// DefaultIdleWindow is how long a conversation stays "current" before the next
// inbound message opens a fresh session.
const DefaultIdleWindow = 24 * time.Hour

// ResetCommand starts a new session regardless of what the resolver would decide.
const ResetCommand = "/new"

// SessionPrefix marks a session as belonging to this channel. Sessions minted
// here carry it, and [IdleWindowResolver] continues only sessions that have it.
//
// The same agent is usually reachable from more than one place — a web console,
// a cron — and those runs share the ADK app and, once a phone number is resolved
// to a platform identity, the user id too. Without a marker, a WhatsApp message
// would resume whatever the person was last doing on the web, which reads as the
// agent leaking one conversation into another.
//
// The prefix is safe to test for: the rest of a minted id is hex, and "w" is not
// a hex digit, so an id from any other source can never begin with it.
const SessionPrefix = "wa"

// UserID returns the ADK user ID for a WhatsApp sender in E.164.
//
// The mapping is deterministic and prefixed, so a WhatsApp participant is a
// distinct ADK user from the same human signed into the console. Sessions and
// memory therefore do not cross between the two surfaces.
//
// This is the identity of an unidentified sender, and it is what a launcher with
// no [Gate] runs every turn as. A gate is the deliberate extra step: it resolves
// the number to a platform identity and returns that as the ADK user instead, at
// which point the two surfaces are one user and share memory. Conversations stay
// apart either way — see [SessionPrefix].
func UserID(phoneE164 string) string {
	return "whatsapp:" + phoneE164
}

// SessionRequest is what a [SessionResolver] gets to decide with. It carries the
// inbound message plus the session service, so a resolver may read prior
// sessions and their events.
type SessionRequest struct {
	// AppName is the ADK app the message will run against.
	AppName string
	// UserID is the ADK user this WhatsApp sender maps to.
	UserID string
	// Text is the inbound message body (empty for a media-only message).
	Text string
	// RepliedToMessageSid is the Twilio SID of the message the user
	// long-press-replied to, or empty. WhatsApp reports this only for an
	// explicit quote-reply, which makes it an exact signal that the user meant
	// to pick up an earlier point in the conversation.
	RepliedToMessageSid string
	// Sessions is the ADK session service. Resolvers may List and Get to read
	// prior sessions; they must not mutate them.
	Sessions session.Service
}

// SessionResolver decides which ADK session an inbound WhatsApp message belongs
// to. WhatsApp has no thread concept, so this mapping is a policy choice rather
// than something the transport tells us.
//
// Resolve returns the session ID to continue, or "" to start a fresh session.
// The launcher's default is [IdleWindowResolver]; supply your own with
// [WithSessionResolver] — for example one that reads the recent history from
// SessionRequest.Sessions and asks a model whether the topic has turned over.
//
// A resolver chooses among sessions that exist; it does not mint them. Returning
// "" is the only way to ask for a new session — the launcher then creates one
// itself, carrying [SessionPrefix]. An ID that does not belong to an existing
// session of this app and user fails the turn as not-found.
type SessionResolver interface {
	// Resolve returns the session ID to continue, or "" to start a fresh session.
	// The ID must be one of this app and user's existing sessions; anything else
	// fails the turn. Return "" to have the launcher create a new session.
	Resolve(ctx context.Context, req *SessionRequest) (string, error)
}

// IdleWindowResolver continues the user's most recent session while it is
// younger than Window, and starts a fresh one otherwise.
//
// This treats a WhatsApp chat the way people use it: a conversation picks up if
// it was recent and feels new after a gap. It deliberately does not try to
// detect a change of subject — two topics in one day is just a conversation, and
// long-horizon recall is what an ADK MemoryService is for.
type IdleWindowResolver struct {
	// Window is the idle period after which a new session starts. Zero means
	// [DefaultIdleWindow].
	Window time.Duration
	// Now overrides the clock in tests. Nil means time.Now.
	Now func() time.Time
}

// Resolve implements [SessionResolver].
// Returns the ID of the session to continue, or "" to start a fresh session.
func (r IdleWindowResolver) Resolve(ctx context.Context, req *SessionRequest) (string, error) {
	// Get the most recent session for this user.
	var latest session.Session
	{
		if req.Sessions == nil {
			return "", fmt.Errorf("whatsapp: session service is required to resolve a session")
		}
		resp, err := req.Sessions.List(ctx, &session.ListRequest{AppName: req.AppName, UserID: req.UserID})
		if err != nil {
			return "", fmt.Errorf("whatsapp: list sessions for %q: %w", req.UserID, err)
		}

		for _, s := range resp.Sessions {
			// Sessions without the prefix belong to another channel; continuing
			// one would resume a conversation held somewhere else.
			if !strings.HasPrefix(s.ID(), SessionPrefix) {
				continue
			}
			if latest == nil || s.LastUpdateTime().After(latest.LastUpdateTime()) {
				latest = s
			}
		}
	}

	// A first-time sender has no session to continue.
	if latest == nil {
		return "", nil
	}

	// Determine the idle window.
	window := r.Window
	if window <= 0 {
		// Use the default if not set.
		window = DefaultIdleWindow
	}

	// Determine the current time.
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}

	// If the latest session is still within the idle window, continue it.
	if now().Sub(latest.LastUpdateTime()) < window {
		// Return the ID of the latest session to continue it.
		return latest.ID(), nil
	}

	// No session is recent enough; start a fresh one.
	return "", nil
}

// createSession opens a new session for this channel and returns its ID.
//
// The session is created here rather than left to the runner's
// AutoCreateSession, which creates whatever session ID it is handed. That would
// make a resolver returning a stale or foreign ID indistinguishable from a first
// message: rather than failing, the run would continue in a brand-new empty
// session under that ID. With creation owned here, the only session IDs the
// runner ever sees are ones that exist, and anything else is a not-found error
// from the session service.
func createSession(ctx context.Context, svc session.Service, appName, userID string) (string, error) {
	if svc == nil {
		return "", fmt.Errorf("whatsapp: session service is required to create a session")
	}

	// Hyphens are stripped because Vertex AI memory bank rejects them in session
	// IDs. (Matches what the upstream ADK launchers do.) The prefix marks the
	// session as this channel's, so a run from the web console or a cron is never
	// resumed here. See [SessionPrefix].
	id := SessionPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")

	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID, SessionID: id})
	if err != nil {
		return "", fmt.Errorf("whatsapp: create session for %q: %w", userID, err)
	}

	// The service is the authority on the ID it minted.
	return resp.Session.ID(), nil
}
