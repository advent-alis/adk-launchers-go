package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordingSender captures what would have been sent to Twilio.
type recordingSender struct {
	sent []*Outbound
}

func (s *recordingSender) Send(_ context.Context, msg *Outbound) (string, error) {
	s.sent = append(s.sent, msg)
	return "SM" + msg.To, nil
}

// gateFunc adapts a function to [Gate].
type gateFunc func(context.Context, *GateRequest) (*GateDecision, error)

func (f gateFunc) Admit(ctx context.Context, req *GateRequest) (*GateDecision, error) {
	return f(ctx, req)
}

// taskRequest builds the Cloud Task request handleTask decodes.
func taskRequest(t *testing.T, in *Inbound) *http.Request {
	t.Helper()

	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal inbound: %v", err)
	}
	return httptest.NewRequest(http.MethodPost, TaskPath, bytes.NewReader(body))
}

// refusingGate refuses every sender with the given reply.
func refusingGate(reply *Outbound) Gate {
	return gateFunc(func(_ context.Context, _ *GateRequest) (*GateDecision, error) {
		return &GateDecision{Reply: reply}, nil
	})
}

// TestHandleTask_RefusalAimsAtTheSender is the safety property behind the
// refusal aiming at in.From: a reply carrying somebody else's number in To
// still goes to the number the gate just judged.
func TestHandleTask_RefusalAimsAtTheSender(t *testing.T) {
	sender := &recordingSender{}
	l := &launcher{sender: sender, users: knownStore("users-1"), gate: refusingGate(&Outbound{
		To:   "+15550009999", // the number the gate asked for, and must not get
		Text: "please sign in",
	})}

	req := taskRequest(t, &Inbound{From: "+27821112222", Body: "hello"})
	if err := l.handleTask(httptest.NewRecorder(), req); err != nil {
		t.Fatalf("handleTask: %v", err)
	}

	if len(sender.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sender.sent))
	}
	if got := sender.sent[0].To; got != "+27821112222" {
		t.Errorf("delivered to %q, want the sender +27821112222", got)
	}
}

// TestHandleTask_RefusalChunksTextButNotTemplates checks a refusal is delivered
// the same way a model reply is: long text split across messages, a template
// sent whole.
func TestHandleTask_RefusalChunksTextButNotTemplates(t *testing.T) {
	t.Run("long text", func(t *testing.T) {
		sender := &recordingSender{}
		body := strings.TrimSpace(strings.Repeat("word ", 500)) // 2500 runes
		l := &launcher{sender: sender, users: knownStore("users-1"), gate: refusingGate(&Outbound{Text: body})}

		req := taskRequest(t, &Inbound{From: "+27821112222", Body: "hello"})
		if err := l.handleTask(httptest.NewRecorder(), req); err != nil {
			t.Fatalf("handleTask: %v", err)
		}
		if len(sender.sent) < 2 {
			t.Fatalf("sent %d messages, want the body split", len(sender.sent))
		}
	})

	t.Run("template", func(t *testing.T) {
		sender := &recordingSender{}
		l := &launcher{sender: sender, users: knownStore("users-1"), gate: refusingGate(&Outbound{
			ContentSid:       "HX123",
			ContentVariables: map[string]string{"1": "I do not recognise this number yet."},
		})}

		req := taskRequest(t, &Inbound{From: "+27821112222", Body: "hello"})
		if err := l.handleTask(httptest.NewRecorder(), req); err != nil {
			t.Fatalf("handleTask: %v", err)
		}
		if len(sender.sent) != 1 {
			t.Fatalf("sent %d messages, want 1", len(sender.sent))
		}
		if sender.sent[0].ContentSid != "HX123" {
			t.Errorf("ContentSid = %q, want HX123", sender.sent[0].ContentSid)
		}
		if got := sender.sent[0].ContentVariables["1"]; got == "" {
			t.Error("ContentVariables were dropped")
		}
	})
}

// TestHandleTask_RefusalWithNoReplySendsNothing covers a sender to be ignored
// rather than turned away.
func TestHandleTask_RefusalWithNoReplySendsNothing(t *testing.T) {
	sender := &recordingSender{}
	l := &launcher{sender: sender, users: knownStore("users-1"), gate: refusingGate(nil)}

	req := taskRequest(t, &Inbound{From: "+27821112222", Body: "hello"})
	if err := l.handleTask(httptest.NewRecorder(), req); err != nil {
		t.Fatalf("handleTask: %v", err)
	}
	if len(sender.sent) != 0 {
		t.Errorf("sent %d messages to an ignored sender, want 0", len(sender.sent))
	}
}

// storeFunc adapts a pair of functions to [UserStore].
type storeFunc struct {
	find   func(context.Context, string) (string, error)
	create func(context.Context, string) (string, error)
}

func (f storeFunc) FindByPhoneNumber(ctx context.Context, phone string) (string, error) {
	return f.find(ctx, phone)
}

func (f storeFunc) CreateByPhoneNumber(ctx context.Context, phone string) (string, error) {
	return f.create(ctx, phone)
}

// knownStore finds every number already held by userID, and fails the test if
// asked to create anybody.
func knownStore(userID string) UserStore {
	return storeFunc{
		find: func(_ context.Context, _ string) (string, error) { return userID, nil },
		create: func(_ context.Context, phone string) (string, error) {
			return "", errors.New("create called for " + phone)
		},
	}
}

// TestNewLauncher_RequiresUsers keeps the perimeter from being optional. Without
// a store there is nobody for a session to belong to, and every sender would be
// admitted as an ADK user derived from their number — which makes the
// conversation history a bearer asset held by whoever holds the number next.
func TestNewLauncher_RequiresUsers(t *testing.T) {
	cfg := Config{
		AccountSid:  "AC00000000000000000000000000000000",
		AuthToken:   "token",
		PhoneNumber: "+17405307773",
		Queue:       "whatsapp-inbound",
	}

	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("NewLauncher with no Users did not panic")
			}
			if msg, _ := r.(string); !strings.Contains(msg, "config.Users is required") {
				t.Errorf("panicked with %v, want a missing-store error", r)
			}
		}()
		NewLauncher("app", cfg)
	}()

	cfg.Users = knownStore("users-1")
	l, ok := NewLauncher("app", cfg).(*launcher)
	if !ok {
		t.Fatal("NewLauncher returned something other than *launcher")
	}
	if l.users == nil {
		t.Error("Config.Users did not reach the launcher")
	}
	// A gate is the optional half: no WithGate, nobody vetoed.
	if l.gate != nil {
		t.Error("a launcher with no WithGate has a gate")
	}
}

// TestHandleTask_RefusedSenderIsNeverCreated is why the gate runs between the
// lookup and the creation. Turning somebody away must not leave an account
// behind under their number.
func TestHandleTask_RefusedSenderIsNeverCreated(t *testing.T) {
	created := false
	sender := &recordingSender{}
	l := &launcher{
		sender: sender,
		users: storeFunc{
			find: func(_ context.Context, _ string) (string, error) { return "", nil },
			create: func(_ context.Context, _ string) (string, error) {
				created = true
				return "users-new", nil
			},
		},
		gate: refusingGate(&Outbound{Text: "we are not open yet"}),
	}

	req := taskRequest(t, &Inbound{From: "+27821112222", Body: "hello"})
	if err := l.handleTask(httptest.NewRecorder(), req); err != nil {
		t.Fatalf("handleTask: %v", err)
	}

	if created {
		t.Error("a refused sender was created anyway")
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d messages, want the refusal", len(sender.sent))
	}
}

// TestHandleTask_GateSeesWhetherTheSenderIsNew covers the field a closed beta is
// made of: GateRequest.UserID is empty exactly when this message would open an
// account.
func TestHandleTask_GateSeesWhetherTheSenderIsNew(t *testing.T) {
	tests := []struct {
		name  string
		found string
		want  string
	}{
		{name: "returning sender", found: "users-1", want: "users-1"},
		{name: "new sender", found: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen *GateRequest
			l := &launcher{
				sender: &recordingSender{},
				users: storeFunc{
					find:   func(_ context.Context, _ string) (string, error) { return tt.found, nil },
					create: func(_ context.Context, _ string) (string, error) { return "users-new", nil },
				},
				// Refuses, so the turn stops before the runtime the test has not built.
				gate: gateFunc(func(_ context.Context, req *GateRequest) (*GateDecision, error) {
					seen = req
					return &GateDecision{}, nil
				}),
			}

			req := taskRequest(t, &Inbound{From: "+27821112222", Body: "hello"})
			if err := l.handleTask(httptest.NewRecorder(), req); err != nil {
				t.Fatalf("handleTask: %v", err)
			}
			if seen == nil {
				t.Fatal("the gate was not consulted")
			}
			if seen.UserID != tt.want {
				t.Errorf("gate saw UserID %q, want %q", seen.UserID, tt.want)
			}
			if seen.PhoneNumber != "+27821112222" {
				t.Errorf("gate saw PhoneNumber %q", seen.PhoneNumber)
			}
		})
	}
}

// TestHandleTask_NoGateAdmitsEverybody is the default: with no WithGate the
// lookup and the creation run back to back and nobody is turned away.
func TestHandleTask_NoGateAdmitsEverybody(t *testing.T) {
	sender := &recordingSender{}
	l := &launcher{
		sender: sender,
		users: storeFunc{
			find:   func(_ context.Context, _ string) (string, error) { return "users-1", nil },
			create: func(_ context.Context, _ string) (string, error) { return "", errors.New("create called") },
		},
	}

	// No runtime is wired, so an admitted turn panics rather than running. That it
	// gets that far is the assertion: nothing refused it and nothing was sent.
	defer func() {
		_ = recover()
		if len(sender.sent) != 0 {
			t.Errorf("sent %d messages without a gate to refuse anybody", len(sender.sent))
		}
	}()
	req := taskRequest(t, &Inbound{From: "+27821112222", Body: "hello"})
	_ = l.handleTask(httptest.NewRecorder(), req)
}
