package whatsapp

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/twilio/twilio-go/client"
	"go.alis.build/alog"
	alismux "go.alis.build/mux"
	"go.alis.build/tasks"
	"google.golang.org/genai"
)

// handleWebhook receives a message from Twilio, hands it to Cloud Tasks, and
// acknowledges.
//
// The ack must be fast: Twilio abandons a webhook that does not respond within
// seconds, and an agent turn takes far longer than that. So nothing expensive
// happens here — the message is validated, parsed, and handed off.
func (l *launcher) handleWebhook(w http.ResponseWriter, r *http.Request) error {
	// Parse the form values. Twilio sends them as application/x-www-form-urlencoded.
	if err := r.ParseForm(); err != nil {
		return alismux.BadRequestErr("parsing form: %v", err)
	}

	// Validate before trusting anything in the form: this endpoint is public,
	// and the signature is what proves Twilio sent it.
	// The origin Twilio called, which is what it signed. Cloud Run and ngrok both
	// terminate TLS upstream, so the inbound request is plaintext while the URL
	// Twilio signed is https.
	var signatureURL string
	{
		switch {
		case l.pinnedBaseURL != "":
			signatureURL = l.pinnedBaseURL
		case r.TLS == nil && (strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.0.0.1")):
			signatureURL = "http://" + r.Host
		default:
			signatureURL = "https://" + r.Host
		}
	}

	// Where Cloud Tasks calls back to run the agent. The same origin as the
	// webhook, unless another service fronts it: that origin does not serve
	// TaskPath, so the caller pins this one separately.
	taskURL := l.pinnedTaskURL
	if taskURL == "" {
		taskURL = signatureURL
	}
	validator := client.NewRequestValidator(l.cfg.AuthToken)
	signature := r.Header.Get("X-Twilio-Signature")

	// Flatten the parsed form to the single-valued map the validator expects.
	params := make(map[string]string, len(r.Form))
	{
		for key, values := range r.Form {
			if len(values) > 0 {
				params[key] = values[0]
			}
		}
	}

	// Reject the webhook if the signature is invalid. Twilio does not retry a webhook that returns 401 Unauthorized, so we do not need to log or alert on this.
	// The signature is a hash of the URL and the form values, keyed by the Twilio
	// auth token. It is not a secret: anyone can compute it if they know the
	// auth token. But only Twilio knows the auth token, so only Twilio can produce
	// a valid signature.
	//
	// This check must happen before any other processing, because it proves that
	// Twilio sent the webhook. Otherwise an attacker could send a fake webhook
	// with arbitrary form values and make us run an agent on them.
	if !validator.Validate(signatureURL+WebhookPath, params, signature) {
		alog.Warnf(r.Context(), "whatsapp: rejecting webhook with invalid signature for %s", signatureURL+WebhookPath)
		return alismux.UnauthorizedErr("invalid twilio signature")
	}

	// Parse the inbound message. 
	// This is cheap: the form is small (the ack budget is tight) and the task body small 
	// (Cloud Tasks caps it at 1 MB, well under WhatsApp's 16 MB media limit).
	in := parseInbound(r.Form)
	if in.From == "" {
		return alismux.BadRequestErr("webhook has no sender")
	}

	// Marshal the inbound message to JSON and schedule a task to run the agent on it.
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("whatsapp: marshal inbound: %w", err)
	}
	if err := (&tasks.Task{
		URL:    taskURL + TaskPath,
		Method: http.MethodPost,
		Body:   body,
		Time:   time.Now().UTC(),
	}).Schedule(r.Context(), l.cfg.Queue); err != nil {
		return fmt.Errorf("whatsapp: schedule task on queue %q: %w", l.cfg.Queue, err)
	}

	// Acknowledge the webhook. Twilio does not retry a webhook that returns 200 OK.
	w.WriteHeader(http.StatusOK)

	// Return nil so the mux does not write an error page on top of our 200 OK.
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

	// Admit the sender, and learn who the agent runs as.
	//
	// This is the perimeter, and it is deliberately the first thing after the
	// payload: nothing below has happened yet, so a refused sender costs no media
	// fetch, touches no session, and leaves no message in a conversation history.
	// Without a gate every sender is admitted as the ADK user derived from their
	// number, which is this package's behaviour on its own.
	userID := UserID(in.From)
	stateDelta := l.stateDelta
	if l.gate != nil {
		decision, err := l.gate.Admit(ctx, &GateRequest{PhoneNumber: in.From, Text: in.AgentText()})
		if err != nil {
			return fmt.Errorf("whatsapp: gate %s: %w", in.From, err)
		}

		if decision == nil {
			return fmt.Errorf("whatsapp: gate %s returned neither a decision nor an error", in.From)
		}

		// No user named is a refusal. Say whatever the gate wanted said, and run
		// nothing — a refusal is a decision, so the task is done, not failed.
		if decision.UserID == "" {
			if decision.Reply == nil {
				alog.Infof(ctx, "whatsapp: refusing %s with no reply to send", in.From)
				return nil
			}

			// Aimed at in.From, never decision.Reply.To: a gate handing over a
			// message must not be able to send it to a number other than the one
			// in play. A template goes as the single message it is; free text is
			// split the way a model reply is below.
			if decision.Reply.ContentSid != "" {
				_, err := l.sender.Send(ctx, &Outbound{
					To:               in.From,
					ContentSid:       decision.Reply.ContentSid,
					ContentVariables: decision.Reply.ContentVariables,
				})
				return err
			}
			for _, chunk := range chunkText(decision.Reply.Text) {
				if _, err := l.sender.Send(ctx, &Outbound{To: in.From, Text: chunk}); err != nil {
					return err
				}
			}
			return nil
		}

		userID = decision.UserID

		// Merge the gate's state under the launcher's own, mutating neither:
		// l.stateDelta is built once and reused on every message. The launcher's
		// keys win because [StateKey] holds the resolved catalog the model's
		// component tools are derived from, and a gate overwriting it would leave
		// the model holding tools whose schemas no longer match their templates —
		// sends that fail, which reach the user as silence.
		if len(decision.State) > 0 {
			stateDelta = make(map[string]any, len(l.stateDelta)+len(decision.State))
			maps.Copy(stateDelta, decision.State)
			maps.Copy(stateDelta, l.stateDelta)
		}
	}

	// Turn the inbound message into the agent's user turn, fetching any media
	// from Twilio as inline bytes. A nil message means there is nothing to run on.
	var msg *genai.Content
	{
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
				// One unreadable attachment should not sink the whole turn; the
				// text alongside it is usually the part that matters.
				alog.Errorf(ctx, "whatsapp: fetching attachment %s: %v", media.URL, err)
				continue
			}
			parts = append(parts, genai.NewPartFromBytes(data, media.ContentType))
		}

		if len(parts) > 0 {
			msg = &genai.Content{Role: genai.RoleUser, Parts: parts}
		}
	}
	if msg == nil {
		alog.Warnf(ctx, "whatsapp: message %s from %s carried no usable content", in.MessageSid, in.From)
		return nil
	}

	// Decide which session this message belongs to: the reset command always
	// starts a fresh one, otherwise the configured resolver decides.
	var sessionID string
	{
		if !in.IsReset() {
			var err error
			sessionID, err = l.resolver.Resolve(ctx, &SessionRequest{
				AppName:             l.appName,
				UserID:              userID,
				Text:                in.AgentText(),
				RepliedToMessageSid: in.RepliedToMessageSid,
				Sessions:            l.runtime.cfg.SessionService,
			})
			if err != nil {
				return err
			}
		}
	}

	_, events, err := l.runtime.run(ctx, runRequest{
		UserID:     userID,
		SessionID:  sessionID,
		Message:    msg,
		StateDelta: stateDelta,
	})
	if err != nil {
		return err
	}

	// Walk the agent's event stream and send what belongs on WhatsApp: model text
	// as plain messages, and calls to catalog components as their templates.
	// Events are consumed in order, so the user sees the turn unfold in the order
	// the agent produced it.
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
				// Render a catalog component call to its Twilio template. A call
				// naming something outside the catalog is one of the agent's own
				// tools and is ignored. A call the launcher cannot render is logged
				// and skipped: the rest of the turn is still worth delivering.
				comp, ok := l.catalog.find(part.FunctionCall.Name)
				if !ok {
					continue
				}
				vars, err := comp.variables(part.FunctionCall.Args)
				if err != nil {
					alog.Errorf(ctx, "whatsapp: rendering component %q: %v", comp.ID, err)
					continue
				}
				if _, err := l.sender.Send(ctx, &Outbound{
					To:               in.From,
					ContentSid:       comp.ContentSid,
					ContentVariables: vars,
				}); err != nil {
					return err
				}

			case part.Text != "":
				// Model text, split across messages at Twilio's body limit.
				for _, chunk := range chunkText(part.Text) {
					if _, err := l.sender.Send(ctx, &Outbound{To: in.From, Text: chunk}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
