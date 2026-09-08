package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
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
	l := &launcher{sender: sender, gate: refusingGate(&Outbound{
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
		l := &launcher{sender: sender, gate: refusingGate(&Outbound{Text: body})}

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
		l := &launcher{sender: sender, gate: refusingGate(&Outbound{
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
	l := &launcher{sender: sender, gate: refusingGate(nil)}

	req := taskRequest(t, &Inbound{From: "+27821112222", Body: "hello"})
	if err := l.handleTask(httptest.NewRecorder(), req); err != nil {
		t.Fatalf("handleTask: %v", err)
	}
	if len(sender.sent) != 0 {
		t.Errorf("sent %d messages to an ignored sender, want 0", len(sender.sent))
	}
}

// TestWithGate_NilIsIgnored keeps a nil gate from disabling the default, which
// would silently admit nobody rather than everybody.
func TestWithGate_NilIsIgnored(t *testing.T) {
	l := &launcher{}
	WithGate(nil)(l)
	if l.gate != nil {
		t.Error("WithGate(nil) set a gate")
	}

	admitAll := gateFunc(func(_ context.Context, req *GateRequest) (*GateDecision, error) {
		return &GateDecision{UserID: "users/" + req.PhoneNumber}, nil
	})
	WithGate(admitAll)(l)
	if l.gate == nil {
		t.Fatal("WithGate did not set the gate")
	}

	decision, err := l.gate.Admit(context.Background(), &GateRequest{PhoneNumber: "+27821112222"})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if decision.UserID != "users/+27821112222" {
		t.Errorf("UserID = %q", decision.UserID)
	}
}
