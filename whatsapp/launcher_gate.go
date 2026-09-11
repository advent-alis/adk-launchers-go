package whatsapp

import "context"

// Gate decides whether an inbound sender may reach the agent at all.
//
// It is optional and subtractive. [Config].Users already answers who a sender is
// — find the user holding this number, create one if nobody does — so a gate is
// never asked to name anybody. It answers the separate question of whether this
// sender may proceed, and it is the only way to answer "no".
//
// Supply one with [WithGate]. Without one every sender proceeds, which is this
// package's opinion working unimpeded: whoever messages the agent ends up with an
// account.
//
// It runs after the lookup and before the creation, and is told which it is by
// [GateRequest].UserID. That is the seam the useful policies live in — refusing
// numbers nobody holds yet is a closed beta, refusing particular numbers is a
// blocklist, refusing everybody is maintenance. A refused stranger is not
// created, so declining to serve somebody does not leave an account behind.
//
// It runs on the task, not the webhook. The webhook has seconds to acknowledge
// before Twilio abandons it, and a gate usually means a network call and
// sometimes an outbound message.
//
// A gate is the perimeter: it runs before any attachment is fetched, before a
// session is resolved or created, and before the agent is loaded. A refused
// sender therefore leaves no trace in the conversation history — which is the
// point, since the alternative is a stranger's message sitting in the session of
// whoever the number is later bound to.
//
// Leaving no trace also leaves the gate nothing to remember with, so an exchange
// that takes more than one message is carried by the answers rather than by
// stored state: refuse with a quick-reply template, and the payload of the button
// the sender taps arrives on the next call as [GateRequest].ButtonPayload.
type Gate interface {
	// Admit decides on one inbound message.
	//
	// An error fails the task, so return one only for a genuine fault — a check
	// that could not be performed. Deciding that a sender may not talk to the
	// agent is a refusal, not an error: return a decision that does not allow,
	// and the message to send them.
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
	// UserID is the user [Config].Users found holding this number, and is empty
	// when nobody holds it yet.
	//
	// Empty is therefore "this message would create an account", which is the
	// distinction a gate refusing new senders but not returning ones is made of.
	// The creation has not happened yet and does not happen at all if this
	// decision refuses.
	UserID string
	// Text is the message as the agent would read it: the body for typed text, and
	// a sentence naming what was tapped and the payload behind it for a component.
	// Empty for a media-only message.
	//
	// This is the form to match a typed answer against. For a tap, match
	// ButtonPayload — the value is in here too, but wrapped in prose.
	Text string
	// ButtonPayload is the payload the agent attached to a quick-reply button the
	// sender has now tapped, and is empty for typed text.
	//
	// Text carries the same value wrapped in a sentence, which is what the model
	// should read. This is the bare value, for a gate driving a flow of its own
	// across turns: a refused turn creates no session, so a question the gate asked
	// last turn can only be answered by what comes back on this one.
	ButtonPayload string
}

// GateDecision is what a [Gate] returns.
type GateDecision struct {
	// Allow lets the turn proceed: the sender's user is created if the lookup
	// found none, and the agent runs. False refuses, and is the zero value — a
	// decision a gate forgot to fill in refuses rather than admits.
	Allow bool
	// State merges into session state before the run, alongside the resolved
	// component catalog. Somewhere to put what the gate learned about the sender,
	// so the agent is told rather than having to ask.
	//
	// A gate that always allows and only writes state is a legitimate use of one:
	// it is how a product puts the sender's own details in front of the model
	// without the launcher having to know what those are.
	//
	// The launcher's own keys win a collision: [StateKey] holds the catalog the
	// model's component tools are derived from, and overwriting it would leave
	// the model holding tools whose schemas no longer match their templates.
	//
	// Unread when Allow is false: a refused turn has no session to merge into.
	State map[string]any
	// Reply is sent to a refused sender, as free text or a content template.
	//
	// Nil sends nothing, which is right for a sender to be ignored rather than
	// turned away. Its To is filled in by the launcher and ignored here, so a gate
	// cannot direct a message at a number other than the one it just judged.
	//
	// Unread when Allow is true: an admitted turn answers through the agent.
	Reply *Outbound
}
