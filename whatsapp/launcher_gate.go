package whatsapp

import "context"

// Gate decides whether an inbound sender may reach the agent, and who the agent
// runs as when they may.
//
// The webhook signature proves a message came from Twilio. It says nothing about
// who sent it: a WhatsApp sender is a phone number, and a phone number is not an
// account. A [Gate] is where that gap is closed, by whatever the product uses for
// identity — this package has no opinion on it and never sees the lookup.
//
// It runs on the task, not the webhook. The webhook has seconds to acknowledge
// before Twilio abandons it, and admitting a sender usually means a network call
// and sometimes an outbound message. Running it here also keeps the resolved
// identity out of the task payload, which would otherwise become a field the task
// handler has to trust.
//
// A gate is the perimeter: it runs before any attachment is fetched, before a
// session is resolved or created, and before the agent is loaded. A refused
// sender therefore leaves no trace in the conversation history — which is the
// point, since the alternative is a stranger's message sitting in the session of
// whoever the number is later bound to.
type Gate interface {
	// Admit decides on one inbound message.
	//
	// An error fails the task, so return one only for a genuine fault — a
	// lookup that could not be performed. Deciding that a sender may not talk to
	// the agent is a refusal, not an error: return a decision with no UserID and
	// the message to send them.
	Admit(ctx context.Context, req *GateRequest) (*GateDecision, error)
}

// GateRequest is one inbound message, presented for admission.
type GateRequest struct {
	// PhoneNumber is the sender in E.164, including the leading '+'.
	//
	// It arrived on a signature-verified webhook, so possession of the phone is
	// proven. Who owns the phone is not: numbers are reassigned and a WhatsApp
	// registration follows the number rather than the person.
	PhoneNumber string
	// Text is the inbound message body, empty for a media-only message.
	//
	// Carries the payload of a tapped button as well as typed text, so a gate can
	// act on an answer to something it asked on a previous turn without holding
	// state between them.
	Text string
}

// GateDecision is what a [Gate] returns.
//
// Admitting and refusing are told apart by UserID alone: a decision naming no
// user is a refusal, whatever else it carries.
type GateDecision struct {
	// UserID is the ADK user to run this turn as. Empty refuses the sender.
	//
	// Set it to the id the rest of the platform already knows this person by, so
	// that a WhatsApp turn and a console turn are one ADK user and share memory.
	// Conversations stay separate regardless — that is [SessionPrefix]'s job, not
	// this field's.
	UserID string
	// State merges into session state before the run, alongside the resolved
	// component catalog. Somewhere to put who the sender turned out to be, so the
	// agent is told rather than having to ask.
	//
	// The launcher's own keys win a collision: [StateKey] holds the catalog the
	// model's component tools are derived from, and overwriting it would leave
	// the model holding tools whose schemas no longer match their templates.
	State map[string]any
	// Reply is sent to a refused sender, as free text or a content template.
	//
	// Nil sends nothing, which is right for a sender to be ignored rather than
	// turned away. Its To is filled in by the launcher and ignored here, so a gate
	// cannot direct a message at a number other than the one it just judged.
	//
	// Unread when UserID is set: an admitted turn answers through the agent.
	Reply *Outbound
}
