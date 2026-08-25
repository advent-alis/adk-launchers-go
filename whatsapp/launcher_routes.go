package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"time"

	"github.com/twilio/twilio-go/client"
	"go.alis.build/alog"
	alismux "go.alis.build/mux"
	"go.alis.build/tasks"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// handleWebhook receives a message from Twilio, hands it to Cloud Tasks, and
// acknowledges.
//
// The ack must be fast: Twilio abandons a webhook that does not respond within
// seconds, and an agent turn takes far longer than that. So nothing expensive
// happens here — the message is validated, parsed, and handed off.
func (l *launcher) handleWebhook(w http.ResponseWriter, r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		return alismux.BadRequestErr("parsing form: %v", err)
	}

	// Validate before trusting anything in the form: this endpoint is public,
	// and the signature is what proves Twilio sent it.
	callbackURL := l.baseURL(r)
	validator := client.NewRequestValidator(l.cfg.AuthToken)
	signature := r.Header.Get("X-Twilio-Signature")
	if !validator.Validate(callbackURL+WebhookPath, formValues(r), signature) {
		alog.Warnf(r.Context(), "whatsapp: rejecting webhook with invalid signature for %s", callbackURL+WebhookPath)
		return alismux.UnauthorizedErr("invalid twilio signature")
	}

	in := parseInbound(r.Form)
	if in.From == "" {
		return alismux.BadRequestErr("webhook has no sender")
	}

	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("whatsapp: marshal inbound: %w", err)
	}
	if err := (&tasks.Task{
		URL:    callbackURL + TaskPath,
		Method: http.MethodPost,
		Body:   body,
		Time:   time.Now().UTC(),
	}).Schedule(r.Context(), l.cfg.Queue); err != nil {
		return fmt.Errorf("whatsapp: schedule task on queue %q: %w", l.cfg.Queue, err)
	}

	w.WriteHeader(http.StatusOK)
	return nil
}

// handleTask runs the agent for one inbound message and delivers its reply.
//
// Registered with SystemPost, so only the environment service account can reach
// it: this endpoint runs agents on behalf of a user.
func (l *launcher) handleTask(w http.ResponseWriter, r *http.Request) error {
	in := &Inbound{}
	if err := json.NewDecoder(r.Body).Decode(in); err != nil {
		return alismux.BadRequestErr("decoding task payload: %v", err)
	}

	ctx := r.Context()
	msg, err := l.buildMessage(ctx, in)
	if err != nil {
		return err
	}
	if msg == nil {
		alog.Warnf(ctx, "whatsapp: message %s from %s carried no usable content", in.MessageSid, in.From)
		return nil
	}

	userID := UserID(in.From)
	sessionID, err := l.resolveSession(ctx, in, userID)
	if err != nil {
		return err
	}

	_, events, err := l.runtime.run(ctx, runRequest{
		UserID:     userID,
		SessionID:  sessionID,
		Message:    msg,
		StateDelta: l.stateDelta,
	})
	if err != nil {
		return err
	}
	return l.deliver(ctx, in.From, events)
}

// resolveSession applies the reset command, then the configured resolver.
func (l *launcher) resolveSession(ctx context.Context, in *Inbound, userID string) (string, error) {
	if in.IsReset() {
		return "", nil
	}
	return l.resolver.Resolve(ctx, &SessionRequest{
		AppName:             l.appName,
		UserID:              userID,
		Text:                in.AgentText(),
		RepliedToMessageSid: in.RepliedToMessageSid,
		Sessions:            l.runtime.cfg.SessionService,
	})
}

// buildMessage turns an inbound message into the agent's user turn, fetching any
// media from Twilio as inline bytes. Returns nil when there is nothing to run on.
func (l *launcher) buildMessage(ctx context.Context, in *Inbound) (*genai.Content, error) {
	var parts []*genai.Part
	if text := in.AgentText(); text != "" {
		parts = append(parts, genai.NewPartFromText(text))
	}

	for _, media := range in.Media {
		if l.media == nil {
			alog.Warnf(ctx, "whatsapp: dropping attachment %s: no media fetcher configured", media.URL)
			continue
		}
		data, err := l.media.Fetch(ctx, media.URL)
		if err != nil {
			// One unreadable attachment should not sink the whole turn; the text
			// alongside it is usually the part that matters.
			alog.Errorf(ctx, "whatsapp: fetching attachment %s: %v", media.URL, err)
			continue
		}
		parts = append(parts, genai.NewPartFromBytes(data, media.ContentType))
	}

	if len(parts) == 0 {
		return nil, nil
	}
	return &genai.Content{Role: genai.RoleUser, Parts: parts}, nil
}

// deliver walks the agent's event stream and sends what belongs on WhatsApp:
// model text as plain messages, and calls to catalog components as their
// templates. Events are consumed in order, so the user sees the turn unfold in
// the order the agent produced it.
func (l *launcher) deliver(ctx context.Context, to string, events iter.Seq2[*session.Event, error]) error {
	for event, err := range events {
		if err != nil {
			return fmt.Errorf("whatsapp: agent run: %w", err)
		}
		// Partial events are streamed fragments of text that arrive again in the
		// final event. The user event is our own inbound message echoed back.
		if event.Partial || event.Content == nil || event.Content.Role == genai.RoleUser {
			continue
		}

		for _, part := range event.Content.Parts {
			switch {
			case part.Thought:
				// Reasoning is not for the user.
			case part.FunctionCall != nil:
				if err := l.sendComponent(ctx, to, part.FunctionCall); err != nil {
					return err
				}
			case part.Text != "":
				if err := l.sendText(ctx, to, part.Text); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// sendText delivers model text, split across messages at Twilio's body limit.
func (l *launcher) sendText(ctx context.Context, to, text string) error {
	for _, chunk := range chunkText(text) {
		if _, err := l.sender.Send(ctx, &Outbound{To: to, Text: chunk}); err != nil {
			return err
		}
	}
	return nil
}

// sendComponent renders a catalog component call to its Twilio template.
//
// A call naming something outside the catalog is one of the agent's own tools
// and is ignored. A call the launcher cannot render is logged and skipped: the
// rest of the turn is still worth delivering.
func (l *launcher) sendComponent(ctx context.Context, to string, call *genai.FunctionCall) error {
	comp, ok := l.catalog.find(call.Name)
	if !ok {
		return nil
	}

	vars, err := comp.variables(call.Args)
	if err != nil {
		alog.Errorf(ctx, "whatsapp: rendering component %q: %v", comp.ID, err)
		return nil
	}
	_, err = l.sender.Send(ctx, &Outbound{
		To:               to,
		ContentSid:       comp.ContentSid,
		ContentVariables: vars,
	})
	return err
}
