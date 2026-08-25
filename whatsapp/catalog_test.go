package whatsapp

import (
	"encoding/json"
	"testing"
)

func TestCatalogValidate(t *testing.T) {
	valid := Component{ID: "confirm", ContentSid: "HX1", Description: "Ask yes or no."}

	tests := []struct {
		name    string
		catalog Catalog
		wantErr bool
	}{
		{"empty is fine", nil, false},
		{"valid", Catalog{valid}, false},
		{"uppercase id", Catalog{{ID: "Confirm", ContentSid: "HX1", Description: "d"}}, true},
		{"id with dash", Catalog{{ID: "con-firm", ContentSid: "HX1", Description: "d"}}, true},
		{"id starting with digit", Catalog{{ID: "1confirm", ContentSid: "HX1", Description: "d"}}, true},
		{"duplicate id", Catalog{valid, valid}, true},
		{"duplicate content sid", Catalog{
			valid,
			{ID: "other", ContentSid: "HX1", Description: "d"},
		}, true},
		{"missing content sid", Catalog{{ID: "confirm", Description: "d"}}, true},
		{"missing description", Catalog{{ID: "confirm", ContentSid: "HX1"}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.catalog.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestResolvedCatalogFind(t *testing.T) {
	catalog := resolvedCatalog{
		{ID: "confirm", ContentSid: "HX1"},
		{ID: "pick", ContentSid: "HX2"},
	}

	comp, ok := catalog.find("pick")
	if !ok || comp.ContentSid != "HX2" {
		t.Fatalf("find(pick) = %+v, %v", comp, ok)
	}
	if _, ok := catalog.find("absent"); ok {
		t.Fatal("find(absent) reported a match")
	}
}

// TestDecodeCatalog covers the shapes a session backend may hand back for the
// state value the launcher wrote as a JSON string.
func TestDecodeCatalog(t *testing.T) {
	const raw = `[{"id":"confirm","description":"d","contentSid":"HX1","kind":"twilio/quick-reply",
		"fields":[{"name":"text","position":"1","description":"Body.","maxRunes":1024}]}]`

	for name, value := range map[string]any{
		"string": raw,
		"bytes":  []byte(raw),
		"decoded": []any{map[string]any{
			"id": "confirm", "description": "d", "contentSid": "HX1", "kind": "twilio/quick-reply",
			"fields": []any{map[string]any{"name": "text", "position": "1", "maxRunes": 1024}},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			catalog, err := decodeCatalog(value)
			if err != nil {
				t.Fatalf("decodeCatalog: %v", err)
			}
			if len(catalog) != 1 || catalog[0].ID != "confirm" || len(catalog[0].Fields) != 1 {
				t.Fatalf("decodeCatalog = %+v", catalog)
			}
			if catalog[0].Fields[0].Position != "1" {
				t.Fatalf("field position = %q", catalog[0].Fields[0].Position)
			}
		})
	}

	if _, err := decodeCatalog("not json"); err == nil {
		t.Error("expected an error for malformed state")
	}
}

// TestResolvedRoundTrip guards the launcher-to-toolset contract: what the
// launcher marshals into state must decode back into working tools.
func TestResolvedRoundTrip(t *testing.T) {
	original := resolvedCatalog{{
		ID: "confirm", Description: "Ask yes or no.", ContentSid: "HX1", Kind: QuickReply,
		Fields: []Field{{Name: "text", Position: "1", Description: "Body.", MaxRunes: MaxBodyRunes}},
	}}

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded, err := decodeCatalog(string(encoded))
	if err != nil {
		t.Fatalf("decodeCatalog: %v", err)
	}

	vars, err := decoded[0].variables(map[string]any{"text": "Approve?"})
	if err != nil {
		t.Fatalf("variables after round trip: %v", err)
	}
	if vars["1"] != "Approve?" {
		t.Fatalf("variables = %v", vars)
	}
}
