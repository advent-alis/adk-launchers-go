package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/twilio/twilio-go"
	api "github.com/twilio/twilio-go/rest/api/v2010"
)

// Outbound is one WhatsApp message to deliver. Exactly one of Text or
// ContentSid is set: free text, or a content template with its variables.
type Outbound struct {
	// To is the recipient in E.164, without the "whatsapp:" prefix.
	To string
	// Text is a plain message body, at most [MaxTextRunes].
	Text string
	// ContentSid is a Twilio content template SID.
	ContentSid string
	// ContentVariables are the template's positional variables, keyed "1", "2", …
	ContentVariables map[string]string
}

// Sender delivers one WhatsApp message and returns its Twilio SID. One call is
// one message: chunking long text and ordering a turn's messages are the
// launcher's job, not the sender's.
//
// The launcher builds a Twilio-backed Sender from its [Config]. Replace it with
// [WithSender] to test a launcher without reaching Twilio.
type Sender interface {
	Send(ctx context.Context, msg *Outbound) (sid string, err error)
}

// MediaFetcher retrieves inbound media. The launcher's Twilio sender doubles as
// one, since Twilio media URLs need the account credentials.
type MediaFetcher interface {
	Fetch(ctx context.Context, mediaURL string) ([]byte, error)
}

// twilioSender sends over the Twilio REST API and fetches media from it.
type twilioSender struct {
	client     *twilio.RestClient
	accountSid string
	authToken  string
	from       string // E.164, without the "whatsapp:" prefix
	http       *http.Client
}

var (
	_ Sender          = (*twilioSender)(nil)
	_ MediaFetcher    = (*twilioSender)(nil)
	_ TemplateFetcher = (*twilioSender)(nil)
)

// newTwilioSender builds a sender for one WhatsApp number.
func newTwilioSender(cfg Config) *twilioSender {
	return &twilioSender{
		client: twilio.NewRestClientWithParams(twilio.ClientParams{
			Username:   cfg.AccountSid,
			AccountSid: cfg.AccountSid,
			Password:   cfg.AuthToken,
		}),
		accountSid: cfg.AccountSid,
		authToken:  cfg.AuthToken,
		from:       cfg.PhoneNumber,
		http:       http.DefaultClient,
	}
}

// Send implements [Sender].
func (s *twilioSender) Send(ctx context.Context, msg *Outbound) (string, error) {
	params := &api.CreateMessageParams{}
	params.SetFrom("whatsapp:" + s.from)
	params.SetTo("whatsapp:" + msg.To)

	switch {
	case msg.ContentSid != "":
		params.SetContentSid(msg.ContentSid)
		if len(msg.ContentVariables) > 0 {
			vars, err := marshalContentVariables(msg.ContentVariables)
			if err != nil {
				return "", err
			}
			params.SetContentVariables(vars)
		}
	case msg.Text != "":
		params.SetBody(msg.Text)
	default:
		return "", fmt.Errorf("whatsapp: outbound message has neither text nor a content sid")
	}

	resp, err := s.client.Api.CreateMessage(params)
	if err != nil {
		return "", fmt.Errorf("whatsapp: create message to %s: %w", msg.To, err)
	}
	if resp.Sid == nil {
		return "", fmt.Errorf("whatsapp: twilio returned no message sid")
	}
	return *resp.Sid, nil
}

// Fetch implements [MediaFetcher]. Twilio media URLs are authenticated with the
// account credentials.
func (s *twilioSender) Fetch(ctx context.Context, mediaURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: media request: %w", err)
	}
	req.URL.User = url.UserPassword(s.accountSid, s.authToken)

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: fetch media: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("whatsapp: fetch media: unexpected status %s", resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: read media: %w", err)
	}
	return data, nil
}

// FetchTemplate implements [TemplateFetcher], reading a content template's
// definition so the launcher can derive the arguments the model must supply.
func (s *twilioSender) FetchTemplate(_ context.Context, contentSid string) (*Template, error) {
	content, err := s.client.ContentV1.FetchContent(contentSid)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: fetch content template %s: %w", contentSid, err)
	}
	if content.Types == nil || len(*content.Types) == 0 {
		return nil, fmt.Errorf("whatsapp: content template %s declares no content type", contentSid)
	}

	// Twilio returns one entry per content type: a template may carry a WhatsApp
	// type alongside a plain-text fallback for SMS. Iterate in sorted order and
	// prefer an interactive type, so the choice does not vary run to run.
	var chosen Kind
	var definition any
	for _, key := range slices.Sorted(maps.Keys(*content.Types)) {
		kind := Kind(key)
		if !slices.Contains(supportedKinds, kind) {
			continue
		}
		if chosen == "" || chosen == Text {
			chosen, definition = kind, (*content.Types)[key]
		}
	}
	if chosen == "" {
		return nil, fmt.Errorf("whatsapp: content template %s declares no supported content type (has %v, want one of %v)",
			contentSid, slices.Sorted(maps.Keys(*content.Types)), supportedKinds)
	}

	encoded, err := json.Marshal(definition)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: re-encode template %s definition: %w", contentSid, err)
	}
	return &Template{ContentSid: contentSid, Kind: chosen, Definition: encoded}, nil
}

// marshalContentVariables encodes template variables as the JSON object Twilio
// expects, e.g. {"1":"Hi","2":"Yes"}.
func marshalContentVariables(vars map[string]string) (string, error) {
	encoded, err := json.Marshal(vars)
	if err != nil {
		return "", fmt.Errorf("whatsapp: marshal content variables: %w", err)
	}
	return string(encoded), nil
}

// chunkText splits body into messages no longer than [MaxTextRunes], breaking at
// the last newline or space before the limit so words survive the split.
func chunkText(body string) []string {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	if utf8.RuneCountInString(body) <= MaxTextRunes {
		return []string{body}
	}

	var chunks []string
	runes := []rune(body)
	for len(runes) > 0 {
		if len(runes) <= MaxTextRunes {
			chunks = append(chunks, strings.TrimSpace(string(runes)))
			break
		}
		cut := MaxTextRunes
		if at := lastBreak(runes[:cut]); at > 0 {
			cut = at
		}
		chunks = append(chunks, strings.TrimSpace(string(runes[:cut])))
		runes = runes[cut:]
	}
	return chunks
}

// lastBreak returns the index just after the last newline or space in runes, or
// 0 when there is none to break on.
func lastBreak(runes []rune) int {
	for i := len(runes) - 1; i > 0; i-- {
		if runes[i] == '\n' || runes[i] == ' ' {
			return i + 1
		}
	}
	return 0
}
