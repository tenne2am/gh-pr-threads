package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func init() {
	// Keep golden-ish assertions free of ANSI escapes.
	lipgloss.SetColorProfile(0) // termenv.Ascii
}

func line(n int) *int { return &n }

func fixture() []Thread {
	mk := func(id, path string, ln int, resolved bool, bodies ...string) Thread {
		t := Thread{ID: id, Path: path, Line: line(ln), IsResolved: resolved}
		for i, b := range bodies {
			t.Comments.Nodes = append(t.Comments.Nodes, Comment{
				ID:        id + "-c",
				Body:      b,
				CreatedAt: "2026-09-01T10:00:00Z",
				Author:    Author{Login: []string{"alice", "bob"}[i%2]},
				URL:       "https://github.com/o/r/pull/1#discussion_" + id,
			})
		}
		return t
	}
	return []Thread{
		mk("T1", "main.go", 12, false, "this needs a nil check", "good catch, fixing"),
		mk("T2", "ui.go", 40, false, "why two panes here?"),
		mk("T3", "old.go", 7, true, "already handled"),
	}
}

func load(t *testing.T) model {
	t.Helper()
	m := newModel(nil, "o", "r", 1)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	next, _ = next.Update(loadedMsg{title: "Some PR", threads: fixture()})
	return next.(model)
}

func key(m model, s string) model {
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return next.(model)
}

func TestResolvedThreadsHiddenByDefault(t *testing.T) {
	m := load(t)
	if len(m.visible) != 2 {
		t.Fatalf("want 2 unresolved threads visible, got %d", len(m.visible))
	}
	m = key(m, "a")
	if len(m.visible) != 3 {
		t.Fatalf("after toggling, want 3 visible, got %d", len(m.visible))
	}
}

func TestHeaderAndPanes(t *testing.T) {
	v := load(t).View()
	for _, want := range []string{"o/r #1", "Some PR", "2 open / 3 total", "main.go:12", "this needs a nil check", "good catch, fixing"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q\n---\n%s", want, v)
		}
	}
	if strings.Contains(v, "already handled") {
		t.Error("resolved thread should not be shown by default")
	}
}

func TestNavigationSwitchesDetail(t *testing.T) {
	m := key(load(t), "j")
	if m.cursor != 1 {
		t.Fatalf("cursor = %d, want 1", m.cursor)
	}
	v := m.View()
	if !strings.Contains(v, "why two panes here?") {
		t.Errorf("detail pane did not follow the cursor\n---\n%s", v)
	}
	// Cursor must not run past the end of the list.
	for range 10 {
		m = key(m, "j")
	}
	if m.cursor != len(m.visible)-1 {
		t.Fatalf("cursor = %d, want %d", m.cursor, len(m.visible)-1)
	}
}

func TestCursorStaysWithinScrollWindow(t *testing.T) {
	many := make([]Thread, 40)
	for i := range many {
		many[i] = Thread{ID: string(rune('A' + i%26)), Path: "f.go", Line: line(i)}
		many[i].Comments.Nodes = []Comment{{Body: "c", Author: Author{Login: "alice"}}}
	}
	m := newModel(nil, "o", "r", 1)
	n, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	n, _ = n.Update(loadedMsg{threads: many})
	mm := n.(model)
	for range 30 {
		mm = key(mm, "j")
	}
	if mm.cursor < mm.offset || mm.cursor >= mm.offset+mm.listCapacity() {
		t.Fatalf("cursor %d outside window [%d,%d)", mm.cursor, mm.offset, mm.offset+mm.listCapacity())
	}
}

func TestReplyModeLifecycle(t *testing.T) {
	m := key(load(t), "r")
	if m.mode != modeReply {
		t.Fatal("r should enter reply mode")
	}
	if !strings.Contains(m.View(), "ctrl+s send") {
		t.Error("footer should show reply keys")
	}
	// Typing goes into the textarea, not the browse keymap.
	m = key(m, "j")
	if m.cursor != 0 {
		t.Error("keystrokes in reply mode must not move the thread cursor")
	}
	if got := m.reply.Value(); got != "j" {
		t.Errorf("textarea value = %q, want %q", got, "j")
	}
	// Esc cancels without posting.
	n, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if n.(model).mode != modeBrowse {
		t.Error("esc should return to browse mode")
	}
}

func TestEmptyReplyIsRejected(t *testing.T) {
	m := key(load(t), "r")
	n, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = n.(model)
	if cmd != nil {
		t.Error("empty reply should not issue a network command")
	}
	if m.mode != modeReply {
		t.Error("should stay in reply mode so the text can be fixed")
	}
	if !strings.Contains(m.View(), "empty") {
		t.Errorf("expected a warning in the footer, got:\n%s", m.View())
	}
}

func TestRepliedMsgAppendsComment(t *testing.T) {
	m := load(t)
	n, _ := m.Update(repliedMsg{
		threadID: "T1",
		comment:  Comment{Body: "thanks!", Author: Author{Login: "carol"}, CreatedAt: "2026-09-09T10:00:00Z"},
	})
	m = n.(model)
	if got := len(m.threads[0].Comments.Nodes); got != 3 {
		t.Fatalf("thread T1 has %d comments, want 3", got)
	}
	if !strings.Contains(m.View(), "thanks!") {
		t.Error("new reply should appear in the conversation pane")
	}
}

func TestResolveTogglesAndRefilters(t *testing.T) {
	m := load(t)
	n, _ := m.Update(resolvedMsg{threadID: "T1", resolved: true})
	m = n.(model)
	if !m.threads[0].IsResolved {
		t.Error("thread should be marked resolved")
	}
	if len(m.visible) != 1 {
		t.Fatalf("resolved thread should drop out of the list, got %d visible", len(m.visible))
	}
}

func TestLocationLabels(t *testing.T) {
	cases := []struct {
		in   Thread
		want string
	}{
		{Thread{Path: "a.go", Line: line(9)}, "a.go:9"},
		{Thread{Path: "a.go", Line: line(9), StartLine: line(5)}, "a.go:5-9"},
		{Thread{Path: "a.go"}, "a.go (outdated)"},
		{Thread{}, "(no file)"},
	}
	for _, c := range cases {
		if got := c.in.Location(); got != c.want {
			t.Errorf("Location() = %q, want %q", got, c.want)
		}
	}
}

const sampleHunk = `@@ -10,6 +10,8 @@ func run() error {
 	a := 1
 	b := 2
 	c := 3
 	d := 4
-	old()
+	if x == nil {
+		return nil
+	}`

func withHunk(t *testing.T) model {
	t.Helper()
	threads := fixture()
	threads[0].Comments.Nodes[0].DiffHunk = sampleHunk
	m := newModel(nil, "o", "r", 1)
	n, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	n, _ = n.Update(loadedMsg{threads: threads})
	return n.(model)
}

func TestDiffShownCompactByDefault(t *testing.T) {
	v := withHunk(t).View()
	for _, want := range []string{"@@ -10,6 +10,8 @@", "⋮", "+        return nil", "-    old()"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q\n---\n%s", want, v)
		}
	}
	if strings.Contains(v, "a := 1") {
		t.Error("compact diff should drop lines far above the comment")
	}
}

func TestDiffToggleShowsFullHunk(t *testing.T) {
	m := key(withHunk(t), "d")
	v := m.View()
	if !strings.Contains(v, "a := 1") {
		t.Errorf("full diff should include the top of the hunk\n---\n%s", v)
	}
	if strings.Contains(v, "⋮") {
		t.Error("full diff should not show the elision marker")
	}
	if key(m, "d").fullDiff {
		t.Error("d should toggle back to the compact diff")
	}
}

func TestDiffKeepsWholeMultiLineRange(t *testing.T) {
	th := Thread{Path: "a.go", Line: line(17), StartLine: line(10)}
	th.Comments.Nodes = []Comment{{DiffHunk: sampleHunk}}
	if got := renderDiff(th, 80, false); strings.Contains(got, "⋮") {
		t.Errorf("an 8-line range should keep the whole hunk, got:\n%s", got)
	}
}

func TestNoDiffWithoutHunk(t *testing.T) {
	if got := renderDiff(fixture()[1], 80, false); got != "" {
		t.Errorf("renderDiff = %q, want empty", got)
	}
}
