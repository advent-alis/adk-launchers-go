package whatsapp

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/adk/v2/session"
)

// DefaultIdleWindow is how long a conversation stays "current" before the next
// inbound message opens a fresh session.
const DefaultIdleWindow = 24 * time.Hour

// ResetCommand starts a new session regardless of what the resolver would decide.
const ResetCommand = "/new"

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
type SessionResolver interface {
	// Resolve returns the session ID to continue, or "" to start a fresh session.
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
func (r IdleWindowResolver) Resolve(ctx context.Context, req *SessionRequest) (string, error) {
	// Get the most recent session for this user. 
	latest, err := latestSession(ctx, req.Sessions, req.AppName, req.UserID)
	if err != nil || latest == nil {
		return "", err
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
	return "", nil
}

// latestSession returns the user's most recently updated session, or nil when
// they have none.
func latestSession(ctx context.Context, svc session.Service, appName, userID string) (session.Session, error) {
	if svc == nil {
		return nil, fmt.Errorf("whatsapp: session service is required to resolve a session")
	}
	resp, err := svc.List(ctx, &session.ListRequest{AppName: appName, UserID: userID})
	if err != nil {
		return nil, fmt.Errorf("whatsapp: list sessions for %q: %w", userID, err)
	}

	var latest session.Session
	for _, s := range resp.Sessions {
		if latest == nil || s.LastUpdateTime().After(latest.LastUpdateTime()) {
			latest = s
		}
	}
	return latest, nil
}
