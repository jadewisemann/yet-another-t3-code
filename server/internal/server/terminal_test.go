package server

import (
	"strings"
	"testing"
)

func TestSanitizeTerminalHistory(t *testing.T) {
	cases := []struct{ in, out string }{
		{"plain", "plain"},
		{"a\x1b[6nb", "ab"},
		{"a\x1b[31mred\x1b[0m", "a\x1b[31mred\x1b[0m"},
		{"x\x1b[>cy", "xy"},
		{"\x1b]11;?\x07z", "z"},
		{"\x1b]0;title\x07z", "\x1b]0;title\x07z"},
	}
	for _, c := range cases {
		got, pending := sanitizeTerminalHistory("", c.in)
		if got != c.out || pending != "" {
			t.Errorf("sanitize(%q) = %q, %q; want %q", c.in, got, pending, c.out)
		}
	}
	// A sequence split across reads waits for its end.
	got, pending := sanitizeTerminalHistory("", "a\x1b[6")
	if got != "a" || pending != "\x1b[6" {
		t.Fatalf("split: %q %q", got, pending)
	}
	got, pending = sanitizeTerminalHistory(pending, "nb")
	if got != "b" || pending != "" {
		t.Fatalf("rest: %q %q", got, pending)
	}
}

func TestTerminalHistoryKeepsNewestLines(t *testing.T) {
	var h terminalHistory
	for i := 0; i < terminalHistoryLines+10; i++ {
		h.append("line\n")
	}
	if n := strings.Count(h.value(), "\n"); n != terminalHistoryLines {
		t.Fatalf("kept %d lines", n)
	}
}

func TestTerminalLabel(t *testing.T) {
	if got := terminalLabel("term-2"); got != "Terminal 2" {
		t.Fatal(got)
	}
	if got := terminalLabel("default"); got != "default" {
		t.Fatal(got)
	}
}
