package whatsapp

import (
	"strings"
	"time"
)

// Option configures optional launcher behaviour.
type Option func(*launcher)

// WithSessionResolver replaces the default [IdleWindowResolver], which continues
// the user's most recent session while it is younger than [DefaultIdleWindow].
//
// This is the seam for a smarter policy — for example a resolver that reads the
// recent history from SessionRequest.Sessions and asks a model whether the topic
// has turned over. A nil resolver is ignored.
func WithSessionResolver(resolver SessionResolver) Option {
	return func(l *launcher) {
		if resolver != nil {
			l.resolver = resolver
		}
	}
}

// WithIdleWindow sets the idle period on the default resolver. It has no effect
// once [WithSessionResolver] has replaced that resolver.
func WithIdleWindow(window IdleWindowResolver) Option {
	return func(l *launcher) { l.resolver = window }
}

// WithSender replaces the Twilio sender, for testing a launcher without reaching
// Twilio. If sender also implements [MediaFetcher] it serves inbound media, and
// if it implements [TemplateFetcher] it resolves the catalog; otherwise those
// capabilities are dropped. A nil sender is ignored.
func WithSender(sender Sender) Option {
	return func(l *launcher) {
		if sender == nil {
			return
		}
		l.sender = sender
		l.media, _ = sender.(MediaFetcher)
		l.templates, _ = sender.(TemplateFetcher)
	}
}

// WithGate sets the [Gate] that admits inbound senders, and names the ADK user
// each admitted turn runs as.
//
// Without one every sender reaches the agent as the ADK user [UserID] derives
// from their number, which is the right default for an agent that serves anyone
// who messages it. Supply a gate when the agent must know who it is talking to.
// A nil gate is ignored.
func WithGate(gate Gate) Option {
	return func(l *launcher) {
		if gate != nil {
			l.gate = gate
		}
	}
}

// WithTemplateTimeout caps how long startup waits on Twilio while resolving the
// catalog. Zero means [DefaultTemplateTimeout].
func WithTemplateTimeout(timeout time.Duration) Option {
	return func(l *launcher) { l.templateTimeout = timeout }
}

// WithBaseURL pins the origin the Twilio signature is checked against, e.g.
// "https://my-agent-abc123.a.run.app".
//
// By default the origin is derived from the inbound request, which is correct on
// Cloud Run and behind a proxy that preserves Host. Pin it when it is not — a
// mismatch fails the signature check, since Twilio signs the URL it called.
//
// Pinning it to an origin this service does not itself serve — a BFF that
// proxies the webhook — also moves the Cloud Task callback there, which that
// origin will not serve. Pair it with [WithTaskURL] in that case.
func WithBaseURL(baseURL string) Option {
	return func(l *launcher) { l.pinnedBaseURL = strings.TrimSuffix(baseURL, "/") }
}

// WithTaskURL pins the origin Cloud Tasks calls back on, when it differs from
// the origin Twilio called — e.g. a gateway fronts the webhook. Defaults to the
// signature origin, which is correct whenever this service serves both
// [WebhookPath] and [TaskPath].
func WithTaskURL(taskURL string) Option {
	return func(l *launcher) { l.pinnedTaskURL = strings.TrimSuffix(taskURL, "/") }
}
