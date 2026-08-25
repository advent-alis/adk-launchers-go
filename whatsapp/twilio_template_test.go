package whatsapp

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// quickReplyTemplate builds a twilio/quick-reply definition. Passing a literal
// string for a title or id models a template that hard-codes that slot.
func quickReplyTemplate(body string, actions ...[2]string) *Template {
	type action struct {
		Title string `json:"title"`
		Id    string `json:"id"`
	}
	def := struct {
		Body    string   `json:"body"`
		Actions []action `json:"actions"`
	}{Body: body}
	for _, a := range actions {
		def.Actions = append(def.Actions, action{Title: a[0], Id: a[1]})
	}
	encoded, _ := json.Marshal(def)
	return &Template{ContentSid: "HX1", Kind: QuickReply, Definition: encoded}
}

func TestResolveTemplate_QuickReply(t *testing.T) {
	comp := Component{ID: "confirm", ContentSid: "HX1", Description: "Ask yes or no."}
	tmpl := quickReplyTemplate("{{1}}", [2]string{"{{2}}", "{{3}}"}, [2]string{"{{4}}", "{{5}}"})

	resolved, err := resolveTemplate(comp, tmpl)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if resolved.Kind != QuickReply {
		t.Errorf("Kind = %q", resolved.Kind)
	}

	want := []Field{
		{Name: "text", Position: "1"},
		{Name: "button_1_label", Position: "2"},
		{Name: "button_1_payload", Position: "3"},
		{Name: "button_2_label", Position: "4"},
		{Name: "button_2_payload", Position: "5"},
	}
	assertFields(t, resolved.Fields, want)
}

// TestResolveTemplate_FixedSlotsProduceNoFields is the behaviour the old
// hand-declared schema could not express: a template whose button labels are
// literal text asks the model only for what it can actually change.
func TestResolveTemplate_FixedSlotsProduceNoFields(t *testing.T) {
	comp := Component{ID: "confirm", ContentSid: "HX1", Description: "Ask yes or no."}
	tmpl := quickReplyTemplate("{{1}}", [2]string{"Yes", "{{2}}"}, [2]string{"No", "{{3}}"})

	resolved, err := resolveTemplate(comp, tmpl)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	assertFields(t, resolved.Fields, []Field{
		{Name: "text", Position: "1"},
		{Name: "button_1_payload", Position: "2"},
		{Name: "button_2_payload", Position: "3"},
	})
}

// TestResolveTemplate_ButtonCountFollowsTemplate is the bug the old design had:
// a two-button template must ask for exactly two buttons, never a range.
func TestResolveTemplate_ButtonCountFollowsTemplate(t *testing.T) {
	comp := Component{ID: "confirm", ContentSid: "HX1", Description: "d"}

	for buttons := 1; buttons <= 3; buttons++ {
		actions := make([][2]string, 0, buttons)
		position := 2
		for range buttons {
			actions = append(actions,
				[2]string{"{{" + strconv.Itoa(position) + "}}", "{{" + strconv.Itoa(position+1) + "}}"})
			position += 2
		}
		resolved, err := resolveTemplate(comp, quickReplyTemplate("{{1}}", actions...))
		if err != nil {
			t.Fatalf("%d buttons: resolveTemplate: %v", buttons, err)
		}
		if got := len(resolved.Fields); got != 1+2*buttons {
			t.Errorf("%d buttons: got %d fields, want %d", buttons, got, 1+2*buttons)
		}
	}
}

func TestResolveTemplate_LiteralBody(t *testing.T) {
	comp := Component{ID: "confirm", ContentSid: "HX1", Description: "d"}
	tmpl := quickReplyTemplate("Approve this payment?", [2]string{"{{1}}", "{{2}}"})

	resolved, err := resolveTemplate(comp, tmpl)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	assertFields(t, resolved.Fields, []Field{
		{Name: "button_1_label", Position: "1"},
		{Name: "button_1_payload", Position: "2"},
	})
}

// TestResolveTemplate_RepeatedVariable covers one variable used in two slots:
// it is asked for once and Twilio substitutes it in both.
func TestResolveTemplate_RepeatedVariable(t *testing.T) {
	comp := Component{ID: "confirm", ContentSid: "HX1", Description: "d"}
	tmpl := quickReplyTemplate("{{1}}", [2]string{"{{2}}", "{{2}}"})

	resolved, err := resolveTemplate(comp, tmpl)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	assertFields(t, resolved.Fields, []Field{
		{Name: "text", Position: "1"},
		{Name: "button_1_label", Position: "2"},
	})
}

func TestResolveTemplate_ListPicker(t *testing.T) {
	def := `{"body":"{{1}}","button":"{{2}}","items":[
		{"item":"{{3}}","id":"{{4}}","description":"{{5}}"},
		{"item":"{{6}}","id":"{{7}}","description":""}]}`
	resolved, err := resolveTemplate(
		Component{ID: "pick", ContentSid: "HX2", Description: "d"},
		&Template{ContentSid: "HX2", Kind: ListPicker, Definition: json.RawMessage(def)})
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	assertFields(t, resolved.Fields, []Field{
		{Name: "text", Position: "1"},
		{Name: "menu_label", Position: "2"},
		{Name: "item_1_title", Position: "3"},
		{Name: "item_1_id", Position: "4"},
		{Name: "item_1_description", Position: "5"},
		{Name: "item_2_title", Position: "6"},
		{Name: "item_2_id", Position: "7"},
	})
}

func TestResolveTemplate_UnsupportedKind(t *testing.T) {
	_, err := resolveTemplate(
		Component{ID: "c", ContentSid: "HX9", Description: "d"},
		&Template{ContentSid: "HX9", Kind: "twilio/carousel", Definition: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("expected an error for an unsupported content type")
	}
	if !strings.Contains(err.Error(), "twilio/carousel") {
		t.Errorf("error should name the offending type: %v", err)
	}
}

func TestResolvedVariables(t *testing.T) {
	resolved := &Resolved{
		ID: "confirm",
		Fields: []Field{
			{Name: "text", Position: "1", MaxRunes: MaxBodyRunes},
			{Name: "button_1_label", Position: "2", MaxRunes: MaxButtonLabelRunes},
		},
	}

	vars, err := resolved.variables(map[string]any{"text": "Approve?", "button_1_label": "Yes"})
	if err != nil {
		t.Fatalf("variables: %v", err)
	}
	if vars["1"] != "Approve?" || vars["2"] != "Yes" {
		t.Fatalf("variables = %v", vars)
	}

	bad := []struct {
		name string
		args map[string]any
	}{
		{"missing field", map[string]any{"text": "Approve?"}},
		{"empty value", map[string]any{"text": "Approve?", "button_1_label": ""}},
		{"wrong type", map[string]any{"text": "Approve?", "button_1_label": 42}},
		{"over the limit", map[string]any{
			"text":           "Approve?",
			"button_1_label": strings.Repeat("x", MaxButtonLabelRunes+1),
		}},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := resolved.variables(tt.args); err == nil {
				t.Fatalf("expected an error for %s", tt.name)
			}
		})
	}
}

func TestResolvedSchema(t *testing.T) {
	resolved := &Resolved{Fields: []Field{
		{Name: "text", Position: "1", Description: "Body.", MaxRunes: MaxBodyRunes},
		{Name: "button_1_label", Position: "2", Description: "Label.", MaxRunes: MaxButtonLabelRunes},
	}}

	schema := resolved.schema()
	properties, ok := schema["properties"].(map[string]any)
	if !ok || len(properties) != 2 {
		t.Fatalf("properties = %#v", schema["properties"])
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 2 {
		t.Fatalf("every field must be required, got %#v", schema["required"])
	}
	label, ok := properties["button_1_label"].(map[string]any)
	if !ok || label["maxLength"] != MaxButtonLabelRunes {
		t.Fatalf("button_1_label = %#v", properties["button_1_label"])
	}
}

func assertFields(t *testing.T, got, want []Field) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d fields, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Position != want[i].Position {
			t.Errorf("field %d = {%s @%s}, want {%s @%s}",
				i, got[i].Name, got[i].Position, want[i].Name, want[i].Position)
		}
		if got[i].MaxRunes == 0 {
			t.Errorf("field %d (%s) has no rune limit", i, got[i].Name)
		}
	}
}
