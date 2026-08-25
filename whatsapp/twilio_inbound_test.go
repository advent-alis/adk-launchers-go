package whatsapp

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseInbound(t *testing.T) {
	form := url.Values{
		"MessageSid":                {"SM123"},
		"From":                      {"whatsapp:+27836566942"},
		"To":                        {"whatsapp:+17405307773"},
		"Body":                      {"hello"},
		"ProfileName":               {"Daniel"},
		"OriginalRepliedMessageSid": {"MM999"},
		"MediaUrl0":                 {"https://api.twilio.com/m/0"},
		"MediaContentType0":         {"image/png"},
		"MediaUrl1":                 {"https://api.twilio.com/m/1"},
		"MediaContentType1":         {"application/pdf"},
	}

	in := parseInbound(form)
	if in.From != "+27836566942" || in.To != "+17405307773" {
		t.Errorf("phone numbers not unprefixed: from=%q to=%q", in.From, in.To)
	}
	if in.RepliedToMessageSid != "MM999" {
		t.Errorf("RepliedToMessageSid = %q", in.RepliedToMessageSid)
	}
	if len(in.Media) != 2 {
		t.Fatalf("got %d attachments, want 2", len(in.Media))
	}
	if in.Media[1].ContentType != "application/pdf" {
		t.Errorf("Media[1].ContentType = %q", in.Media[1].ContentType)
	}
}

// TestParseInbound_StopsAtFirstGap guards the MediaUrl{i} scan: Twilio numbers
// attachments contiguously, so a gap means there are no more.
func TestParseInbound_StopsAtFirstGap(t *testing.T) {
	in := parseInbound(url.Values{
		"From":      {"whatsapp:+27836566942"},
		"MediaUrl0": {"https://api.twilio.com/m/0"},
		"MediaUrl2": {"https://api.twilio.com/m/2"},
	})
	if len(in.Media) != 1 {
		t.Fatalf("got %d attachments, want 1", len(in.Media))
	}
}

func TestInboundAgentText(t *testing.T) {
	tests := []struct {
		name  string
		in    Inbound
		want  string
		exact bool
	}{
		{"plain text", Inbound{Body: "what is my balance"}, "what is my balance", true},
		{"button tap", Inbound{Body: "Yes", ButtonText: "Yes", ButtonPayload: "approve"}, "approve", false},
		{"list pick", Inbound{ListTitle: "Cheque", ListID: "acc_1"}, "acc_1", false},
		{"media only", Inbound{}, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.AgentText()
			switch {
			case tt.exact && got != tt.want:
				t.Fatalf("AgentText() = %q, want %q", got, tt.want)
			case !tt.exact && !strings.Contains(got, tt.want):
				t.Fatalf("AgentText() = %q, want it to carry %q", got, tt.want)
			}
		})
	}
}

func TestInboundIsReset(t *testing.T) {
	for body, want := range map[string]bool{
		"/new":            true,
		"  /new  ":        true,
		"/NEW":            true,
		"/news":           false,
		"start a new one": false,
		"":                false,
	} {
		if got := (&Inbound{Body: body}).IsReset(); got != want {
			t.Errorf("IsReset(%q) = %v, want %v", body, got, want)
		}
	}
}
