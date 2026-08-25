package whatsapp

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkText(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   \n ", nil},
		{"short", "hello", []string{"hello"}},
		{"trimmed", "  hello  ", []string{"hello"}},
		{"exactly at the limit", strings.Repeat("a", MaxTextRunes), []string{strings.Repeat("a", MaxTextRunes)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chunkText(tt.body)
			if len(got) != len(tt.want) {
				t.Fatalf("chunkText() returned %d chunks, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("chunk %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestChunkText_SplitsOnWordBoundary checks that a long body is broken at
// whitespace, so no chunk exceeds Twilio's limit and no word is cut in half.
func TestChunkText_SplitsOnWordBoundary(t *testing.T) {
	body := strings.TrimSpace(strings.Repeat("word ", 500)) // 2500 runes
	chunks := chunkText(body)

	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want the body split", len(chunks))
	}
	for i, chunk := range chunks {
		if n := utf8.RuneCountInString(chunk); n > MaxTextRunes {
			t.Errorf("chunk %d is %d runes, over the %d limit", i, n, MaxTextRunes)
		}
		if strings.HasPrefix(chunk, " ") || strings.HasSuffix(chunk, " ") {
			t.Errorf("chunk %d has untrimmed whitespace: %q", i, chunk)
		}
	}
	if rejoined := strings.Join(chunks, " "); rejoined != body {
		t.Errorf("chunks do not rejoin to the original body")
	}
}

// TestChunkText_UnbreakableBody covers a body with nowhere to break: it is cut
// at the limit rather than sent over it.
func TestChunkText_UnbreakableBody(t *testing.T) {
	chunks := chunkText(strings.Repeat("a", MaxTextRunes+50))
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if n := utf8.RuneCountInString(chunks[0]); n != MaxTextRunes {
		t.Errorf("first chunk is %d runes, want %d", n, MaxTextRunes)
	}
	if n := utf8.RuneCountInString(chunks[1]); n != 50 {
		t.Errorf("second chunk is %d runes, want 50", n)
	}
}

// TestChunkText_CountsRunesNotBytes guards against splitting mid-character.
func TestChunkText_CountsRunesNotBytes(t *testing.T) {
	body := strings.Repeat("é", MaxTextRunes) // 2x MaxTextRunes bytes
	chunks := chunkText(body)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1: the body is within the rune limit", len(chunks))
	}
	if !utf8.ValidString(chunks[0]) {
		t.Error("chunk is not valid UTF-8")
	}
}

func TestMarshalContentVariables(t *testing.T) {
	got, err := marshalContentVariables(map[string]string{"1": "Hi \"there\"", "2": "Yes"})
	if err != nil {
		t.Fatalf("marshalContentVariables: %v", err)
	}
	if !strings.Contains(got, `"1":"Hi \"there\""`) || !strings.Contains(got, `"2":"Yes"`) {
		t.Fatalf("marshalContentVariables() = %s", got)
	}
}
