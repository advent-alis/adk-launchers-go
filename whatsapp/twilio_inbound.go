package whatsapp

import (
	"fmt"
	"strconv"
	"strings"
)

// maxMediaAttachments bounds the MediaUrl{i} scan on an inbound webhook.
const maxMediaAttachments = 10

// Inbound is one WhatsApp message, parsed from the Twilio webhook form. It is
// the Cloud Task payload: the webhook parses and enqueues, the task handler
// decodes and runs the agent.
//
// Media is carried by URL, not by value. The task handler fetches the bytes from
// Twilio when it builds the agent's message, which keeps the webhook fast (the
// ack budget is tight) and the task body small (Cloud Tasks caps it at 1 MB,
// well under WhatsApp's 16 MB media limit).
type Inbound struct {
	// MessageSid identifies this inbound message.
	MessageSid string `json:"messageSid"`
	// From is the sender in E.164, without the "whatsapp:" prefix.
	From string `json:"from"`
	// To is the receiving WhatsApp number in E.164, without the prefix.
	To string `json:"to"`
	// Body is the message text. For a button tap Twilio repeats the button
	// label here; prefer [Inbound.AgentText].
	Body string `json:"body,omitempty"`
	// ProfileName is the sender's WhatsApp display name.
	ProfileName string `json:"profileName,omitempty"`
	// RepliedToMessageSid is set when the user long-press-replied to an earlier
	// message, naming exactly which one.
	RepliedToMessageSid string `json:"repliedToMessageSid,omitempty"`

	// ButtonText and ButtonPayload are set when the user tapped a quick-reply
	// button.
	ButtonText string `json:"buttonText,omitempty"`
	// ButtonPayload is the value the agent supplied when sending the button.
	ButtonPayload string `json:"buttonPayload,omitempty"`
	// ListTitle and ListID are set when the user picked a list-picker row.
	ListTitle string `json:"listTitle,omitempty"`
	// ListID is set when the user picked a list-picker row.
	// It is the value the agent supplied when sending the list.
	ListID string `json:"listId,omitempty"`

	// Media holds the attachments on this message.
	Media []Attachment `json:"media,omitempty"`
}

// Attachment is one inbound media attachment, still hosted by Twilio.
type Attachment struct {
	// URL is the Twilio media URL. Fetching it requires the account credentials.
	URL string `json:"url"`
	// ContentType is the MIME type Twilio reported.
	ContentType string `json:"contentType"`
}

// parseInbound reads an Inbound from a Twilio webhook form. The caller must have
// parsed the form already, so the same values can be reused for signature
// validation.
func parseInbound(form map[string][]string) *Inbound {

	// Function to get the first value of a form key, or "" if missing. Twilio sends
	// empty values as the empty string, so we don't need to distinguish between
	// missing and empty.
	get := func(key string) string {
		if v := form[key]; len(v) > 0 {
			return v[0]
		}
		return ""
	}

	// Build the Inbound struct from the form values
	in := &Inbound{
		MessageSid:          get("MessageSid"),
		From:                strings.TrimPrefix(get("From"), "whatsapp:"),
		To:                  strings.TrimPrefix(get("To"), "whatsapp:"),
		Body:                get("Body"),
		ProfileName:         get("ProfileName"),
		RepliedToMessageSid: get("OriginalRepliedMessageSid"),
		ButtonText:          get("ButtonText"),
		ButtonPayload:       get("ButtonPayload"),
		ListTitle:           get("ListTitle"),
		ListID:              get("ListId"),
	}

	// Scan for media attachments, up to the maximum allowed by Twilio
	// Twilio sends MediaUrl0, MediaUrl1, ..., MediaUrlN, and stops at the first
	// missing one. We also read MediaContentType{i} for each attachment.
	for i := range maxMediaAttachments {
		url := get("MediaUrl" + strconv.Itoa(i))
		if url == "" {
			break
		}
		in.Media = append(in.Media, Attachment{
			URL:         url,
			ContentType: get("MediaContentType" + strconv.Itoa(i)),
		})
	}

	// Return the parsed Inbound
	return in
}

// AgentText returns the message text as the agent should read it.
//
// A tap on a component is reported as a plain sentence rather than the bare
// label, so the agent can tell "the user chose the Yes button" from "the user
// typed the word Yes" — and so the payload the agent originally attached to that
// button comes back to it.
func (in *Inbound) AgentText() string {
	// If the user tapped a button, report it as a sentence with the payload.
	// If the user picked a list row, report it as a sentence with the value.
	// Otherwise, return the raw body text.
	switch {
	case in.ButtonPayload != "":
		return fmt.Sprintf("The user tapped the %q button (value: %s).", in.ButtonText, in.ButtonPayload)
	case in.ListID != "":
		return fmt.Sprintf("The user selected %q from the list (value: %s).", in.ListTitle, in.ListID)
	default:
		return in.Body
	}
}

// IsReset reports whether the user asked to start a fresh session.
func (in *Inbound) IsReset() bool {
	return strings.EqualFold(strings.TrimSpace(in.Body), ResetCommand)
}
