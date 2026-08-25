package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"unicode/utf8"

	contentv1 "github.com/twilio/twilio-go/rest/content/v1"
)

// Kind is a Twilio content type, derived from the template rather than declared.
type Kind string

const (
	// Text is a plain body with no interactive elements (twilio/text). Useful
	// for reaching a user outside the 24-hour window, where WhatsApp allows only
	// templates.
	Text Kind = "twilio/text"
	// QuickReply is a body plus tappable reply buttons (twilio/quick-reply).
	QuickReply Kind = "twilio/quick-reply"
	// ListPicker is a body plus a menu of selectable rows (twilio/list-picker).
	ListPicker Kind = "twilio/list-picker"
	// CallToAction is a body plus buttons that open a URL or dial a number
	// (twilio/call-to-action).
	CallToAction Kind = "twilio/call-to-action"
)

// Limits WhatsApp imposes on the values filling a template. They are checked
// before the send because a value Twilio rejects reaches the user as silence:
// the agent believes it replied and nothing arrives.
const (
	// MaxBodyRunes caps a message body.
	MaxBodyRunes = 1024
	// MaxTextRunes is Twilio's hard cap on a single WhatsApp message body.
	// Longer model output is split across messages.
	// See https://www.twilio.com/docs/api/errors/21617.
	MaxTextRunes = 1600
	// MaxButtonLabelRunes caps a reply-button title.
	MaxButtonLabelRunes = 20
	// MaxPayloadRunes caps a value echoed back when the user taps or selects.
	MaxPayloadRunes = 128
	// MaxItemTitleRunes caps a list-picker row title.
	MaxItemTitleRunes = 24
	// MaxItemDescriptionRunes caps a list-picker row description.
	MaxItemDescriptionRunes = 72
)

// Template is the part of a Twilio content template this package reads.
type Template struct {
	// ContentSid identifies the template.
	ContentSid string
	// Kind is the Twilio content type key, e.g. "twilio/quick-reply".
	Kind Kind
	// Definition is that type's body exactly as Twilio returned it.
	Definition json.RawMessage
}

// TemplateFetcher reads a content template's definition from Twilio. The
// launcher's Twilio sender implements it; [WithSender] can replace both.
type TemplateFetcher interface {
	FetchTemplate(ctx context.Context, contentSid string) (*Template, error)
}

// Field is one model-supplied argument bound to a template variable position.
//
// Only positions the template actually declares become fields. A slot the
// template hard-codes — a button whose label is fixed text, say — produces no
// field, so the model is never asked for a value it could not change.
type Field struct {
	// Name is the argument name the model sees.
	Name string `json:"name"`
	// Position is the template variable this fills, e.g. "2" for {{2}}.
	Position string `json:"position"`
	// Description tells the model what belongs here.
	Description string `json:"description"`
	// MaxRunes caps the value, from the role the field plays.
	MaxRunes int `json:"maxRunes"`
}

// Resolved is a catalog [Component] joined with the Twilio template behind it.
// The launcher builds these at startup and publishes them into session state,
// where [Toolset] turns each into a tool.
type Resolved struct {
	ID          string  `json:"id"`
	Description string  `json:"description"`
	ContentSid  string  `json:"contentSid"`
	Kind        Kind    `json:"kind"`
	Fields      []Field `json:"fields"`
}

// schema returns the JSON Schema for this component's tool arguments. Every
// field is required: a template placeholder left unfilled renders as an empty
// button or an error, never as an omitted element.
func (r *Resolved) schema() map[string]any {
	properties := make(map[string]any, len(r.Fields))
	required := make([]string, 0, len(r.Fields))
	for _, f := range r.Fields {
		properties[f.Name] = map[string]any{
			"type":        "string",
			"description": f.Description,
			"maxLength":   f.MaxRunes,
		}
		required = append(required, f.Name)
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
}

// variables converts model-supplied arguments into Twilio content variables. It
// re-checks the limits the schema declares, since the model is asked to respect
// them rather than guaranteed to.
func (r *Resolved) variables(args map[string]any) (map[string]string, error) {
	vars := make(map[string]string, len(r.Fields))
	for _, f := range r.Fields {
		raw, ok := args[f.Name]
		if !ok {
			return nil, fmt.Errorf("%s is required", f.Name)
		}
		value, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be a string, got %T", f.Name, raw)
		}
		if value == "" {
			return nil, fmt.Errorf("%s must not be empty", f.Name)
		}
		if n := utf8.RuneCountInString(value); n > f.MaxRunes {
			return nil, fmt.Errorf("%s is %d characters, limit is %d", f.Name, n, f.MaxRunes)
		}
		vars[f.Position] = value
	}
	return vars, nil
}

// placeholderPattern matches a Twilio content variable, e.g. {{2}}.
var placeholderPattern = regexp.MustCompile(`\{\{(\d+)\}\}`)

// slot is one string in a template that may carry variables, described by the
// role it plays so a field derived from it can be named and capped.
type slot struct {
	value    string
	name     string
	desc     string
	maxRunes int
}

// fields turns a slot into one field per variable it declares, or none when the
// template hard-codes it.
func (s slot) fields() []Field {
	matches := placeholderPattern.FindAllStringSubmatch(s.value, -1)
	out := make([]Field, 0, len(matches))
	for _, match := range matches {
		position := match[1]
		name := s.name
		if len(matches) > 1 {
			// A slot mixing several variables needs distinct argument names.
			name = s.name + "_" + position
		}
		out = append(out, Field{
			Name:        name,
			Position:    position,
			Description: s.desc,
			MaxRunes:    s.maxRunes,
		})
	}
	return out
}

// resolveTemplate joins a catalog entry with its Twilio template, deriving the
// argument schema from what the template actually declares.
func resolveTemplate(comp Component, tmpl *Template) (*Resolved, error) {
	slots, err := templateSlots(tmpl)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: component %q (%s): %w", comp.ID, comp.ContentSid, err)
	}

	resolved := &Resolved{
		ID:          comp.ID,
		Description: comp.Description,
		ContentSid:  comp.ContentSid,
		Kind:        tmpl.Kind,
	}
	seen := make(map[string]bool)
	for _, s := range slots {
		for _, field := range s.fields() {
			if seen[field.Position] {
				// Twilio allows one variable in several slots; the first
				// occurrence names it and the rest reuse that value.
				continue
			}
			seen[field.Position] = true
			resolved.Fields = append(resolved.Fields, field)
		}
	}
	return resolved, nil
}

// templateSlots lists the variable-bearing strings of a template, in the order
// the model should be asked for them.
func templateSlots(tmpl *Template) ([]slot, error) {
	body := func(value string) slot {
		return slot{value: value, name: "text", desc: "Message body shown to the user.", maxRunes: MaxBodyRunes}
	}

	switch tmpl.Kind {
	case Text:
		var def contentv1.TwilioText
		if err := json.Unmarshal(tmpl.Definition, &def); err != nil {
			return nil, fmt.Errorf("decoding %s definition: %w", tmpl.Kind, err)
		}
		return []slot{body(def.Body)}, nil

	case QuickReply:
		var def contentv1.TwilioQuickReply
		if err := json.Unmarshal(tmpl.Definition, &def); err != nil {
			return nil, fmt.Errorf("decoding %s definition: %w", tmpl.Kind, err)
		}
		slots := []slot{body(def.Body)}
		for i, action := range def.Actions {
			n := strconv.Itoa(i + 1)
			slots = append(slots,
				slot{value: action.Title, name: "button_" + n + "_label",
					desc: "Text on button " + n + ".", maxRunes: MaxButtonLabelRunes},
				slot{value: action.Id, name: "button_" + n + "_payload",
					desc: "Value echoed back when the user taps button " + n + ".", maxRunes: MaxPayloadRunes},
			)
		}
		return slots, nil

	case ListPicker:
		var def contentv1.TwilioListPicker
		if err := json.Unmarshal(tmpl.Definition, &def); err != nil {
			return nil, fmt.Errorf("decoding %s definition: %w", tmpl.Kind, err)
		}
		slots := []slot{
			body(def.Body),
			{value: def.Button, name: "menu_label",
				desc: "Label on the button that opens the list.", maxRunes: MaxButtonLabelRunes},
		}
		for i, item := range def.Items {
			n := strconv.Itoa(i + 1)
			slots = append(slots,
				slot{value: item.Item, name: "item_" + n + "_title",
					desc: "Title of row " + n + ".", maxRunes: MaxItemTitleRunes},
				slot{value: item.Id, name: "item_" + n + "_id",
					desc: "Value echoed back when the user selects row " + n + ".", maxRunes: MaxPayloadRunes},
				slot{value: item.Description, name: "item_" + n + "_description",
					desc: "Secondary line under row " + n + ".", maxRunes: MaxItemDescriptionRunes},
			)
		}
		return slots, nil

	case CallToAction:
		var def contentv1.TwilioCallToAction
		if err := json.Unmarshal(tmpl.Definition, &def); err != nil {
			return nil, fmt.Errorf("decoding %s definition: %w", tmpl.Kind, err)
		}
		slots := []slot{body(def.Body)}
		for i, action := range def.Actions {
			n := strconv.Itoa(i + 1)
			slots = append(slots,
				slot{value: action.Title, name: "button_" + n + "_label",
					desc: "Text on button " + n + ".", maxRunes: MaxButtonLabelRunes},
				// A call-to-action button carries exactly one destination; the
				// template fixes which, so only the populated one has a slot.
				slot{value: action.Url, name: "button_" + n + "_url",
					desc: "URL button " + n + " opens.", maxRunes: MaxPayloadRunes},
				slot{value: action.Phone, name: "button_" + n + "_phone",
					desc: "Number button " + n + " dials.", maxRunes: MaxPayloadRunes},
			)
		}
		return slots, nil
	}

	return nil, fmt.Errorf("unsupported content type %q, want one of %v", tmpl.Kind, supportedKinds)
}

// supportedKinds lists the Twilio content types this package can render.
var supportedKinds = []Kind{Text, QuickReply, ListPicker, CallToAction}
