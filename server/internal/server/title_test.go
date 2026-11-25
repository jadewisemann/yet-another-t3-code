package server

import (
	"os"
	"strings"
	"testing"
)

// testdata/title-prompt.txt and title-schema.json are what upstream sent
// the fake codex in the title-generation scenario.
func TestThreadTitlePromptMatchesUpstream(t *testing.T) {
	want, err := os.ReadFile("testdata/title-prompt.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := threadTitlePrompt("name this thread", "workspace", nil); got != string(want) {
		t.Fatalf("prompt differs from upstream:\n%s", got)
	}
	schema, err := os.ReadFile("testdata/title-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if threadTitleSchema != string(schema) {
		t.Fatalf("schema differs: %s", schema)
	}
}

func TestSanitizeThreadTitle(t *testing.T) {
	for in, want := range map[string]string{
		`"Fix login"`:                 "Fix login",
		"  Fix   login \nsecond line": "Fix login",
		`{"title":"From JSON"}`:       "From JSON",
		"":                            "New thread",
		`"a" and "b"`:                 `"a" and "b"`,
		strings.Repeat("x", 130):      strings.Repeat("x", 117) + "...",
	} {
		if got := sanitizeThreadTitle(in); got != want {
			t.Errorf("sanitizeThreadTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLimitTitleMessage(t *testing.T) {
	got := limitTitleMessage(strings.Repeat("a", 50)+strings.Repeat("b", 50), 41)
	want := strings.Repeat("a", 10) + "\n[Content truncated]\n" + strings.Repeat("b", 10)
	if got != want {
		t.Fatalf("got %q", got)
	}
}
