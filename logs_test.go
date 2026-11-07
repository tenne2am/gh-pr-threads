package main

import (
	"strings"
	"testing"
)

func TestParseLog(t *testing.T) {
	raw := "\ufeff2026-09-01T10:01:02.1234567Z ##[group]Run go test ./...\r\n" +
		"2026-09-01T10:01:02.2Z \x1b[36;1mgo test ./...\x1b[0m\n" +
		"2026-09-01T10:01:03Z --- FAIL: TestThing\n" +
		"2026-09-01T10:01:04.5Z ##[endgroup]\n" +
		"2026-09-01T10:01:05.5Z ##[error]Process completed with exit code 1.\n" +
		"no timestamp here\n" +
		"2026-09-01T10:01:06Z ##[warning]Node 16 is deprecated\n\n"
	want := []logLine{
		{"10:01:02", "Run go test ./...", logGroup},
		{"10:01:02", "go test ./...", logPlain},
		{"10:01:03", "--- FAIL: TestThing", logPlain},
		{"10:01:04", "", logEndGroup},
		{"10:01:05", "Process completed with exit code 1.", logError},
		{"", "no timestamp here", logPlain},
		{"10:01:06", "Node 16 is deprecated", logWarning},
	}
	got := parseLog(raw)
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if parseLog("") != nil || parseLog("\ufeff\n") != nil {
		t.Error("an empty log should have no lines")
	}
}

func TestSanitizeLogStripsTerminalControl(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"colours", "\x1b[31;1mred\x1b[0m text", "red text"},
		{"window title (OSC 0)", "a\x1b]0;pwned\x07b", "ab"},
		{"clipboard write (OSC 52)", "a\x1b]52;c;ZXZpbA==\x07b", "ab"},
		{"hyperlink (OSC 8)", "\x1b]8;;https://evil.test\x1b\\click\x1b]8;;\x1b\\", "click"},
		{"unterminated OSC", "a\x1b]0;pwned", "a"},
		{"clear screen and home", "\x1b[2J\x1b[Hafter", "after"},
		{"device control string", "a\x1bPq#0;2;0;0;0\x1b\\b", "ab"},
		{"lone escape: only the ESC goes", "a\x1bb", "ab"},
		{"reset (ESC c) is defused", "a\x1bcb", "acb"},
		{"C1 CSI", "a\u009b31mb", "a31mb"},
		{"bell, backspace, carriage return", "a\x07b\x08c\rd", "abcd"},
		{"tab becomes a space", "a\tb", "a b"},
		{"unicode is kept", "テスト ✓ é", "テスト ✓ é"},
	}
	for _, c := range cases {
		got := sanitizeLog(c.in)
		if got != c.want {
			t.Errorf("%s: sanitizeLog(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				t.Errorf("%s: control character %U survived in %q", c.name, r, got)
			}
		}
	}
}

func TestWrapText(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  []string
	}{
		{"short", 10, []string{"short"}},
		{"abcdefghij", 10, []string{"abcdefghij"}},
		{"abcdefghijk", 5, []string{"abcde", "fghij", "k"}},
		{"テストです", 4, []string{"テス", "トで", "す"}}, // two cells per character
		{"aテ", 2, []string{"a", "テ"}},           // a wide rune never straddles the edge
		{"anything", 0, []string{"anything"}},
	}
	for _, c := range cases {
		got := wrapText(c.in, c.width)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("wrapText(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
	}
}

func TestRenderLog(t *testing.T) {
	lines := []logLine{
		{"10:00:00", "Run tests", logGroup},
		{"10:00:01", strings.Repeat("x", 25), logPlain}, // wraps onto 3 rows at width 12 with the marker-less prefix
		{"10:00:02", "", logEndGroup},
		{"10:00:03", "boom", logError},
		{"10:00:04", "again", logError},
	}
	content, errorRows := renderLog(lines, 12, false)
	rows := strings.Split(plain(content), "\n")
	want := []string{"▸ Run tests", strings.Repeat("x", 12), strings.Repeat("x", 12), "x", "✗ boom", "✗ again"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Errorf("rows = %q, want %q", rows, want)
	}
	if len(errorRows) != 2 || errorRows[0] != 4 || errorRows[1] != 5 {
		t.Errorf("errorRows = %v, want [4 5] (endgroup dropped, wrapped rows counted)", errorRows)
	}

	content, _ = renderLog(lines[:2], 20, true)
	rows = strings.Split(plain(content), "\n")
	if rows[0] != "10:00:00 ▸ Run tests" {
		t.Errorf("timestamp column missing: %q", rows[0])
	}
	// Continuation rows line up under the text, not under the timestamp.
	if rows[2] != strings.Repeat(" ", 9)+strings.Repeat("x", 11) {
		t.Errorf("continuation row = %q", rows[2])
	}
	for _, r := range rows {
		if w := len([]rune(r)); w > 20 {
			t.Errorf("row %q is wider than the pane", r)
		}
	}
}
