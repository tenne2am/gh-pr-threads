package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

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
	if got := renderDiff(th, 80, false, nil); strings.Contains(got, "⋮") {
		t.Errorf("an 8-line range should keep the whole hunk, got:\n%s", got)
	}
}

func TestNoDiffWithoutHunk(t *testing.T) {
	if got := renderDiff(fixture()[1], 80, false, nil); got != "" {
		t.Errorf("renderDiff = %q, want empty", got)
	}
}

func TestQuoteOpensReplyWithCompactDiff(t *testing.T) {
	m := key(withHunk(t), "Q")
	if m.mode != modeReply {
		t.Fatal("Q should enter reply mode")
	}
	want := "```diff\n- old()\n+ if x == nil {\n+ return nil\n+ }\n```\n\n"
	// The textarea turns tabs into spaces; compare with whitespace collapsed.
	got := strings.Join(strings.Fields(m.reply.Value()), " ")
	if got != strings.Join(strings.Fields(want), " ") {
		t.Errorf("reply = %q, want %q", m.reply.Value(), want)
	}
	if strings.Contains(m.reply.Value(), "@@") || strings.Contains(m.reply.Value(), "a := 1") {
		t.Error("quote should match the compact view: no hunk header or far context")
	}
}

func TestQuoteFollowsFullDiffToggle(t *testing.T) {
	m := key(key(withHunk(t), "d"), "Q")
	if !strings.Contains(m.reply.Value(), "a := 1") {
		t.Errorf("with the full diff shown, the quote should be full too:\n%s", m.reply.Value())
	}
}

func TestCtrlQInsertsQuoteAfterTypedText(t *testing.T) {
	m := key(key(withHunk(t), "r"), "see")
	n, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlQ})
	m = n.(model)
	if m.mode != modeReply {
		t.Fatal("ctrl+q must not leave reply mode")
	}
	if v := m.reply.Value(); !strings.HasPrefix(v, "see\n```diff\n") {
		t.Errorf("quote should start on its own line after the typed text, got %q", v)
	}
}

func TestQuoteWithoutHunkWarns(t *testing.T) {
	m := key(load(t), "Q")
	if m.reply.Value() != "" {
		t.Errorf("reply should stay empty, got %q", m.reply.Value())
	}
	if !strings.Contains(m.View(), "no diff to quote") {
		t.Errorf("expected a warning in the footer, got:\n%s", m.View())
	}
}

func TestQuoteFenceAvoidsBackticksInCode(t *testing.T) {
	th := Thread{}
	th.Comments.Nodes = []Comment{{DiffHunk: "@@ -1 +1 @@\n+s := \"```\""}}
	if got := quoteDiff(th, true); !strings.HasPrefix(got, "````diff\n") || !strings.HasSuffix(got, "\n````\n") {
		t.Errorf("fence should be longer than any backtick run in the code, got %q", got)
	}
}

func enter(m model) model {
	n, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return n.(model)
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// plain drops colour codes so styled spans next to each other can be matched.
func plain(s string) string { return ansiEscape.ReplaceAllString(s, "") }

// collapse makes quotes comparable regardless of how tabs were expanded.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestSelectStartsOnCommentedLine(t *testing.T) {
	m := key(withHunk(t), "v")
	if m.mode != modeDiff {
		t.Fatal("v should enter line selection")
	}
	if got := m.selection(); got != (lineRange{7, 7}) {
		t.Errorf("selection = %+v, want the last hunk line", got)
	}
	v := plain(m.View())
	if !strings.Contains(v, "▌ +    }") {
		t.Errorf("selected line should carry the gutter marker\n---\n%s", v)
	}
	if strings.Contains(v, "▌ +    if") {
		t.Error("only the commented line should be selected at first")
	}
}

func TestSelectMultiLineCommentPreselectsRange(t *testing.T) {
	threads := fixture()
	threads[0].Comments.Nodes[0].DiffHunk = sampleHunk
	threads[0].StartLine, threads[0].Line = line(10), line(12)
	m := newModel(nil, "o", "r", 1)
	n, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	n, _ = n.Update(loadedMsg{threads: threads})
	if got := key(n.(model), "v").selection(); got != (lineRange{5, 7}) {
		t.Errorf("selection = %+v, want the 3 commented lines", got)
	}
}

func TestSelectedLinesAreQuoted(t *testing.T) {
	m := key(key(key(withHunk(t), "v"), "k"), "k")
	m = enter(m)
	if m.mode != modeReply {
		t.Fatal("enter should open the reply")
	}
	want := "```diff\n+\tif x == nil {\n+\t\treturn nil\n+\t}\n```"
	if got := m.reply.Value(); collapse(got) != collapse(want) {
		t.Errorf("reply = %q, want %q", got, want)
	}
	if strings.Contains(m.View(), "▌") {
		t.Error("selection markers should go away once replying")
	}
}

func TestSpaceRestartsSelection(t *testing.T) {
	m := key(key(key(key(withHunk(t), "v"), "k"), " "), "k")
	if got := m.selection(); got != (lineRange{5, 6}) {
		t.Errorf("selection = %+v, want {5 6}", got)
	}
}

func TestSelectionStaysInShownLines(t *testing.T) {
	m := key(withHunk(t), "v")
	for range 10 {
		m = key(m, "k")
	}
	if m.selCursor != 4 {
		t.Errorf("compact view: cursor = %d, want 4 (first shown line)", m.selCursor)
	}
	m = key(m, "d")
	for range 10 {
		m = key(m, "k")
	}
	if m.selCursor != 0 {
		t.Errorf("full hunk: cursor = %d, want 0", m.selCursor)
	}
	// Back to compact: both ends are pulled into the shown lines.
	m = key(m, "d")
	if got := m.selection(); got.from < 4 {
		t.Errorf("selection %+v reaches lines the compact view hides", got)
	}
}

func TestSelectEscReturnsToBrowse(t *testing.T) {
	m := key(withHunk(t), "v")
	n, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = n.(model)
	if m.mode != modeBrowse {
		t.Fatal("esc should leave line selection")
	}
	if strings.Contains(m.View(), "▌") {
		t.Error("gutter should disappear after leaving selection")
	}
	if m.reply.Value() != "" {
		t.Error("esc must not start a reply")
	}
}

func TestSelectWithoutHunkWarns(t *testing.T) {
	m := key(load(t), "v")
	if m.mode != modeBrowse {
		t.Error("v on a thread without a diff should stay in browse mode")
	}
	if !strings.Contains(m.View(), "no diff on this thread") {
		t.Errorf("expected a warning in the footer, got:\n%s", m.View())
	}
}

func TestSelectionCursorScrollsIntoView(t *testing.T) {
	long := "@@ -1,60 +1,60 @@"
	for i := range 60 {
		long += "\n line " + string(rune('A'+i%26))
	}
	threads := fixture()
	threads[0].Comments.Nodes[0].DiffHunk = long
	m := newModel(nil, "o", "r", 1)
	n, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	n, _ = n.Update(loadedMsg{threads: threads})
	m = key(key(n.(model), "d"), "v") // full hunk, cursor on the last line
	if m.detail.YOffset == 0 {
		t.Fatal("the pane should scroll down to the selection cursor")
	}
	m = key(m, "g")
	if m.detail.YOffset > 3 {
		t.Errorf("after g the pane should be back near the top, YOffset = %d", m.detail.YOffset)
	}
}

func TestDiffStart(t *testing.T) {
	single := Thread{Line: line(12)}
	multi := Thread{StartLine: line(10), Line: line(14)}
	cases := []struct {
		name string
		t    Thread
		n    int
		full bool
		want int
	}{
		{"compact keeps commented line + context", single, 8, false, 4},
		{"full shows everything", single, 8, true, 0},
		{"short hunk is not trimmed", single, 3, false, 0},
		{"range widens the compact view", multi, 20, false, 12},
		{"no line numbers falls back to one line", Thread{}, 8, false, 4},
		{"empty hunk", single, 0, false, 0},
	}
	for _, c := range cases {
		if got := diffStart(c.t, c.n, c.full); got != c.want {
			t.Errorf("%s: diffStart = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestRenderDiffExpandsTabsAndTruncates(t *testing.T) {
	th := Thread{}
	th.Comments.Nodes = []Comment{{DiffHunk: "@@ -1 +1 @@\n+\tsomething quite long here"}}
	got := plain(renderDiff(th, 12, true, nil))
	if strings.Contains(got, "\t") {
		t.Error("tabs should be expanded so widths line up")
	}
	for _, l := range strings.Split(got, "\n") {
		if lipgloss.Width(l) > 12 {
			t.Errorf("line %q is wider than the pane", l)
		}
	}
	// The selection gutter takes two columns from the same width.
	for _, l := range strings.Split(plain(renderDiff(th, 12, true, &lineRange{0, 0})), "\n") {
		if lipgloss.Width(l) > 12 {
			t.Errorf("line %q with gutter is wider than the pane", l)
		}
	}
}

func TestRenderDiffWithoutHeader(t *testing.T) {
	th := Thread{Line: line(1)}
	th.Comments.Nodes = []Comment{{DiffHunk: "+only\n+lines"}}
	got := plain(renderDiff(th, 80, false, nil))
	if got != "+only\n+lines" {
		t.Errorf("renderDiff = %q, want the lines unchanged and no elision marker", got)
	}
}

func TestFenceDiffEmpty(t *testing.T) {
	if got := fenceDiff(nil); got != "" {
		t.Errorf("fenceDiff(nil) = %q, want empty", got)
	}
}

func TestQuoteAfterNewlineAddsNoBlankLine(t *testing.T) {
	m := key(withHunk(t), "r")
	m.reply.InsertString("see:\n")
	n, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlQ})
	if v := n.(model).reply.Value(); !strings.HasPrefix(v, "see:\n```diff\n") {
		t.Errorf("quote should follow the existing newline directly, got %q", v)
	}
}

func TestCtrlQWithoutHunkKeepsDraft(t *testing.T) {
	m := key(key(load(t), "r"), "hi")
	n, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlQ})
	m = n.(model)
	if m.reply.Value() != "hi" {
		t.Errorf("draft changed to %q", m.reply.Value())
	}
	if m.mode != modeReply || !strings.Contains(m.View(), "no diff to quote") {
		t.Errorf("should warn and stay in reply mode, got:\n%s", m.View())
	}
}

func TestSelectionJumpKeys(t *testing.T) {
	m := key(withHunk(t), "v")
	if m = key(m, "g"); m.selCursor != 4 {
		t.Errorf("g: cursor = %d, want 4 (first shown line)", m.selCursor)
	}
	if m = key(m, "G"); m.selCursor != 7 {
		t.Errorf("G: cursor = %d, want 7 (last line)", m.selCursor)
	}
	if m = key(m, "j"); m.selCursor != 7 {
		t.Errorf("j past the end: cursor = %d, want 7", m.selCursor)
	}
}

func TestSelectionConfirmKeys(t *testing.T) {
	for _, k := range []string{"Q", "r"} {
		m := key(key(key(withHunk(t), "v"), "k"), k)
		if m.mode != modeReply {
			t.Fatalf("%s should open the reply", k)
		}
		want := "```diff\n+\t\treturn nil\n+\t}\n```"
		if got := m.reply.Value(); collapse(got) != collapse(want) {
			t.Errorf("%s: reply = %q, want %q", k, got, want)
		}
	}
}

func TestSelectFromFullHunk(t *testing.T) {
	m := key(key(key(withHunk(t), "d"), "v"), "g")
	m = enter(m)
	if !strings.Contains(m.reply.Value(), "a := 1") {
		t.Errorf("selection in the full hunk should reach its first line, got %q", m.reply.Value())
	}
}

func TestSelectionExitAndQuitKeys(t *testing.T) {
	for _, k := range []string{"v", "q"} {
		m := key(key(withHunk(t), "v"), k)
		if m.mode != modeBrowse {
			t.Errorf("%s should go back to browsing", k)
		}
	}
	// q must not quit from selection, but ctrl+c must.
	_, cmd := key(withHunk(t), "v").Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		t.Error("q in selection mode should not issue a command")
	}
	_, cmd = key(withHunk(t), "v").Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c should return tea.Quit")
	}
}

func TestSelectionScrollKeysKeepSelection(t *testing.T) {
	m := key(key(withHunk(t), "v"), "k")
	before := m.selection()
	for _, k := range []string{"J", "K"} {
		if m = key(m, k); m.selection() != before || m.mode != modeDiff {
			t.Errorf("%s changed the selection or mode", k)
		}
	}
}

func TestSelectionFooter(t *testing.T) {
	if v := key(withHunk(t), "v").View(); !strings.Contains(v, "enter quote+reply") {
		t.Errorf("footer should list the selection keys, got:\n%s", v)
	}
}

func TestSelectionEndsWhenThreadLosesDiff(t *testing.T) {
	m := key(withHunk(t), "v")
	// Resolving T1 hides it, so the cursor lands on T2, which has no hunk.
	n, _ := m.Update(resolvedMsg{threadID: "T1", resolved: true})
	m = n.(model)
	if m.mode != modeBrowse {
		t.Error("selection mode should end when the thread has no diff")
	}
	n, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if n.(model).mode == modeReply {
		t.Error("enter should not open a quote reply for a thread without a diff")
	}
}

func checksFixture() []Check {
	return []Check{
		{Workflow: "CI", Name: "lint", State: CheckPass, Detail: "success",
			StartedAt: "2026-09-01T10:00:00Z", CompletedAt: "2026-09-01T10:00:42Z", URL: "https://example.test/lint"},
		{Workflow: "PR Triaging", Name: "label", State: CheckSkipped, Detail: "skipped",
			StartedAt: "2026-09-01T10:00:10Z", CompletedAt: "2026-09-01T10:00:05Z"},
		{Workflow: "CI", Name: "test", State: CheckFail, Detail: "failure",
			StartedAt: "2026-09-01T10:00:00Z", CompletedAt: "2026-09-01T10:03:12Z", URL: "https://example.test/test"},
		{Workflow: "CI", Name: "build", State: CheckPending, Detail: "in progress", StartedAt: "2026-09-01T10:00:00Z"},
	}
}

func withChecks(t *testing.T, checks []Check) model {
	t.Helper()
	n, _ := load(t).Update(checksMsg{sha: "abc1234def", checks: checks})
	return n.(model)
}

func TestSortChecksPutsFailuresFirst(t *testing.T) {
	var got []string
	for _, c := range sortChecks(checksFixture()) {
		got = append(got, c.Label())
	}
	want := []string{"CI / test", "CI / build", "CI / lint", "PR Triaging / label"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %q, want %q", got, want)
	}
}

func TestChecksSummary(t *testing.T) {
	pass := Check{State: CheckPass}
	cases := []struct {
		checks []Check
		want   string
	}{
		{nil, ""},
		{[]Check{pass, {State: CheckSkipped}}, "✓ 1 passed"},
		{[]Check{pass, {State: CheckPending}}, "● 1 pending"},
		{[]Check{pass, {State: CheckPending}, {State: CheckFail}, {State: CheckFail}}, "✗ 2 failing"},
		{[]Check{{State: CheckSkipped}, {State: CheckSkipped}}, "- 2 skipped"},
	}
	for _, c := range cases {
		if got := plain(checksSummary(c.checks)); got != c.want {
			t.Errorf("checksSummary(%v) = %q, want %q", c.checks, got, c.want)
		}
	}
}

func TestCheckTiming(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-09-01T10:05:00Z")
	fx := checksFixture()
	cases := []struct {
		c    Check
		want string
	}{
		{fx[0], "42s"},
		{fx[2], "3m12s"},
		{fx[3], "running 5m00s"},
		{fx[1], ""}, // skipped, and its start is after its end
		{Check{State: CheckPending}, ""},
	}
	for _, c := range cases {
		if got := checkTiming(c.c, now); got != c.want {
			t.Errorf("checkTiming(%s) = %q, want %q", c.c.Label(), got, c.want)
		}
	}
	if got := formatDuration(2*time.Hour + 3*time.Minute); got != "2h03m" {
		t.Errorf("formatDuration = %q", got)
	}
}

func TestChecksInHeader(t *testing.T) {
	v := plain(withChecks(t, checksFixture()).View())
	if !strings.Contains(v, "2 open / 3 total · ✗ 1 failing") {
		t.Errorf("header should summarise the checks\n---\n%s", v)
	}
	if v := plain(load(t).View()); strings.Contains(v, "total ·") {
		t.Errorf("no summary before the checks have loaded\n---\n%s", v)
	}
}

func TestChecksViewLifecycle(t *testing.T) {
	m := key(withChecks(t, checksFixture()), "w")
	if m.mode != modeChecks {
		t.Fatal("w should open the checks view")
	}
	v := plain(m.View())
	for _, want := range []string{"✗ CI / test", "failure · 3m12s", "● CI / build", "✓ CI / lint", "- PR Triaging / label", "checks on abc1234"} {
		if !strings.Contains(v, want) {
			t.Errorf("checks view missing %q\n---\n%s", want, v)
		}
	}
	if strings.Contains(v, "this needs a nil check") {
		t.Error("the checks view should replace the thread panes")
	}
	if first := strings.SplitN(plain(m.viewChecks()), "\n", 2)[0]; !strings.Contains(first, "CI / test") {
		t.Errorf("the failing check should be listed first, got %q", first)
	}

	if m = key(m, "j"); m.checkCursor != 1 {
		t.Errorf("j: cursor = %d, want 1", m.checkCursor)
	}
	if m = key(m, "G"); m.checkCursor != 3 {
		t.Errorf("G: cursor = %d, want 3", m.checkCursor)
	}
	if m = key(m, "j"); m.checkCursor != 3 {
		t.Errorf("j past the end: cursor = %d, want 3", m.checkCursor)
	}
	if m = key(m, "g"); m.checkCursor != 0 {
		t.Errorf("g: cursor = %d, want 0", m.checkCursor)
	}

	for _, k := range []string{"w", "q"} {
		if back := key(m, k); back.mode != modeBrowse {
			t.Errorf("%s should go back to the threads", k)
		}
	}
	n, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if n.(model).mode != modeBrowse {
		t.Error("esc should go back to the threads")
	}
	if !strings.Contains(n.(model).View(), "this needs a nil check") {
		t.Error("threads should be back after leaving the checks view")
	}
}

func TestChecksViewQuitAndRefresh(t *testing.T) {
	m := key(withChecks(t, checksFixture()), "w")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c should return tea.Quit")
	}

	n, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	m = n.(model)
	if cmd == nil || m.checksLoaded {
		t.Error("R should refetch the checks")
	}
	if !strings.Contains(m.View(), "loading checks") {
		t.Errorf("expected a loading message, got:\n%s", m.View())
	}
}

func TestChecksViewStates(t *testing.T) {
	if v := key(load(t), "w").View(); !strings.Contains(v, "loading checks") {
		t.Errorf("before the checks arrive, got:\n%s", v)
	}
	if v := key(withChecks(t, nil), "w").View(); !strings.Contains(v, "No checks have run") {
		t.Errorf("with no checks, got:\n%s", v)
	}
	n, _ := load(t).Update(checksMsg{err: errors.New("fetching checks: forbidden")})
	m := n.(model)
	if m.status != "" {
		t.Error("a checks error must not take over the footer of the threads view")
	}
	if v := key(m, "w").View(); !strings.Contains(v, "fetching checks: forbidden") {
		t.Errorf("the checks view should show the error, got:\n%s", v)
	}
}

func TestChecksRowsAreAligned(t *testing.T) {
	long := Check{Workflow: "Unit and Integration Tests", Name: strings.Repeat("integration-tests ", 10), State: CheckPass, Detail: "success"}
	m := key(withChecks(t, append(checksFixture(), long)), "w")
	lines := strings.Split(m.viewChecks(), "\n")
	for _, l := range lines[:5] {
		if w := lipgloss.Width(l); w != m.width-1 {
			t.Errorf("row is %d wide, want %d so the right column lines up: %q", w, m.width-1, plain(l))
		}
	}
	if !strings.Contains(plain(m.viewChecks()), "…") {
		t.Error("an over-long name should be truncated")
	}
}

func TestChecksViewScrolls(t *testing.T) {
	many := make([]Check, 80)
	for i := range many {
		many[i] = Check{Name: fmt.Sprintf("job-%02d", i), State: CheckPass}
	}
	m := key(withChecks(t, many), "w")
	for range 60 {
		m = key(m, "j")
	}
	if m.checkCursor < m.checkOffset || m.checkCursor >= m.checkOffset+m.bodyHeight() {
		t.Fatalf("cursor %d outside window [%d,%d)", m.checkCursor, m.checkOffset, m.checkOffset+m.bodyHeight())
	}
	if !strings.Contains(m.View(), "job-60") {
		t.Error("the selected check should be on screen")
	}
}

// rerunFixture: CI #7 (run 42) has finished with one failure, Deploy #3
// (run 43) is still going, and ci/jenkins is not an Actions check.
func rerunFixture() []Check {
	return []Check{
		{Workflow: "CI", Name: "test", State: CheckFail, JobID: 2, RunID: 42, RunNumber: 7, Detail: "failure"},
		{Workflow: "Deploy", Name: "ship", State: CheckPending, JobID: 5, RunID: 43, RunNumber: 3},
		{Workflow: "CI", Name: "lint", State: CheckPass, JobID: 1, RunID: 42, RunNumber: 7},
		{Workflow: "Deploy", Name: "build", State: CheckPass, JobID: 4, RunID: 43, RunNumber: 3},
		{Name: "ci/jenkins", State: CheckPass},
	}
}

// checksAt opens the checks view with the cursor on the check with label.
func checksAt(t *testing.T, m model, label string) model {
	t.Helper()
	m = key(m, "w")
	for i, c := range m.checks {
		if c.Label() == label {
			m.checkCursor = i
			return m
		}
	}
	t.Fatalf("no check %q", label)
	return m
}

func TestRerunAsksBeforeRunning(t *testing.T) {
	cases := []struct{ key, prompt string }{
		{"r", "Re-run CI / test? (y/n)"},
		{"f", "Re-run failed jobs of CI #7? (y/n)"},
		{"A", "Re-run all jobs of CI #7? (y/n)"},
	}
	for _, c := range cases {
		m := checksAt(t, withChecks(t, rerunFixture()), "CI / test")
		n, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(c.key)})
		m = n.(model)
		if cmd != nil {
			t.Errorf("%s must not re-run before confirming", c.key)
		}
		if v := m.View(); !strings.Contains(v, c.prompt) {
			t.Errorf("%s: footer should ask %q\n---\n%s", c.key, c.prompt, v)
		}
	}
}

func TestRerunDeclined(t *testing.T) {
	m := key(checksAt(t, withChecks(t, rerunFixture()), "CI / test"), "r")
	for _, k := range []string{"n", "j"} { // anything but y declines
		n, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		got := n.(model)
		if cmd != nil || got.confirm != nil {
			t.Errorf("%q should cancel without re-running", k)
		}
		if !strings.Contains(got.View(), "re-run cancelled") {
			t.Errorf("%q: expected a cancellation notice", k)
		}
		if got.checkCursor != m.checkCursor {
			t.Errorf("%q answered the prompt; it must not also move the cursor", k)
		}
	}
}

func TestRerunGuards(t *testing.T) {
	cases := []struct{ label, key, warning string }{
		{"ci/jenkins", "r", "ci/jenkins is not a GitHub Actions job"},
		{"Deploy / build", "r", "Deploy #3 is still running"}, // finished job, unfinished run
		{"Deploy / ship", "A", "Deploy #3 is still running"},
		{"CI / lint", "f", ""}, // CI #7 has a failure elsewhere in the run: allowed
	}
	for _, c := range cases {
		m := key(checksAt(t, withChecks(t, rerunFixture()), c.label), c.key)
		if c.warning == "" {
			if m.confirm == nil {
				t.Errorf("%s %s: should ask to confirm", c.key, c.label)
			}
			continue
		}
		if m.confirm != nil {
			t.Errorf("%s %s: should not offer the re-run", c.key, c.label)
		}
		if v := m.View(); !strings.Contains(v, c.warning) {
			t.Errorf("%s %s: want warning %q\n---\n%s", c.key, c.label, c.warning, v)
		}
	}

	allGreen := []Check{{Workflow: "CI", Name: "lint", State: CheckPass, JobID: 1, RunID: 42, RunNumber: 7}}
	m := key(checksAt(t, withChecks(t, allGreen), "CI / lint"), "f")
	if m.confirm != nil || !strings.Contains(m.View(), "CI #7 has no failed jobs") {
		t.Errorf("f on a run without failures should warn, got:\n%s", m.View())
	}
}

func TestRerunConfirmedPostsAndRefreshes(t *testing.T) {
	client, fake := fakeClient(t)
	m := withChecks(t, rerunFixture())
	m.client = client
	m = key(checksAt(t, m, "CI / test"), "f")

	n, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = n.(model)
	if !strings.Contains(m.View(), "re-running failed jobs of CI #7") {
		t.Errorf("expected progress in the footer, got:\n%s", m.View())
	}
	n, follow := m.Update(cmd())
	m = n.(model)
	if follow == nil {
		t.Error("a successful re-run should schedule a refresh of the checks")
	}
	if len(fake.rest) != 1 || fake.rest[0] != "POST /repos/o/r/actions/runs/42/rerun-failed-jobs" {
		t.Errorf("requests = %q", fake.rest)
	}
	if !strings.Contains(m.View(), "re-run requested: failed jobs of CI #7") {
		t.Errorf("expected a confirmation, got:\n%s", m.View())
	}

	// After the delay the checks are refetched, without blanking the view.
	n, cmd = m.Update(refreshChecksMsg{})
	if cmd == nil {
		t.Fatal("refreshChecksMsg should refetch the checks")
	}
	if !n.(model).checksLoaded {
		t.Error("the delayed refresh should keep showing the current checks")
	}
}

func TestRerunFailureShown(t *testing.T) {
	client, fake := fakeClient(t)
	fake.restStatus = 403
	fake.restBody = `{"message":"Must have admin rights to Repository."}`
	m := withChecks(t, rerunFixture())
	m.client = client
	m = key(checksAt(t, m, "CI / test"), "r")
	n, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = n.(model)
	n, follow := m.Update(cmd())
	if follow != nil {
		t.Error("a failed re-run should not schedule a refresh")
	}
	if v := n.(model).View(); !strings.Contains(v, "Must have admin rights to Repository.") {
		t.Errorf("expected GitHub's error in the footer, got:\n%s", v)
	}
}

// failingLog has 40 lines of setup, two errors, and lines after them.
func failingLog() string {
	var b strings.Builder
	b.WriteString("2026-09-01T10:00:00Z ##[group]Set up job\n")
	for i := range 40 {
		fmt.Fprintf(&b, "2026-09-01T10:00:%02dZ setup line %d\n", i%60, i)
	}
	b.WriteString("2026-09-01T10:01:00Z ##[endgroup]\n")
	b.WriteString("2026-09-01T10:01:01Z --- FAIL: TestThing\n")
	b.WriteString("2026-09-01T10:01:02Z ##[error]first error\n")
	for i := range 20 {
		fmt.Fprintf(&b, "2026-09-01T10:01:03Z between %d\n", i)
	}
	b.WriteString("2026-09-01T10:01:30Z ##[error]Process completed with exit code 1.\n")
	for i := range 20 {
		fmt.Fprintf(&b, "2026-09-01T10:01:31Z cleanup %d\n", i)
	}
	return b.String()
}

// logsOf opens the log view on the check with label and delivers text.
func logsOf(t *testing.T, label, text string) model {
	t.Helper()
	m := checksAt(t, withChecks(t, rerunFixture()), label)
	n, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	m = n.(model)
	if m.mode != modeLogs || cmd == nil {
		t.Fatalf("l should open the log view and fetch the log (mode %d)", m.mode)
	}
	n, _ = m.Update(logsMsg{jobID: m.logCheck.JobID, text: text})
	return n.(model)
}

func TestLogsOpenOnFirstErrorOfFailedJob(t *testing.T) {
	m := logsOf(t, "CI / test", failingLog())
	v := plain(m.View())
	for _, want := range []string{"✗ CI / test", "failure · log", "--- FAIL: TestThing", "✗ first error"} {
		if !strings.Contains(v, want) {
			t.Errorf("log view missing %q\n---\n%s", want, v)
		}
	}
	if strings.Contains(v, "setup line 0") {
		t.Error("a failed job's log should open at its first error, not the top")
	}
	if got := m.logView.YOffset; got != m.logErrorRows[0]-logContext {
		t.Errorf("YOffset = %d, want %d (first error with context above)", got, m.logErrorRows[0]-logContext)
	}
}

func TestLogsJumpBetweenErrors(t *testing.T) {
	m := logsOf(t, "CI / test", failingLog())
	// The second error is near the end, so the view stops at the bottom
	// rather than showing three lines of context above it.
	bottom := m.logView.TotalLineCount() - m.logView.Height
	first, second := m.logErrorRows[0]-logContext, min(m.logErrorRows[1]-logContext, bottom)

	if m = key(m, "e"); m.logView.YOffset != second {
		t.Errorf("e: YOffset = %d, want %d", m.logView.YOffset, second)
	}
	if m = key(m, "e"); !strings.Contains(m.View(), "no more errors") || m.logView.YOffset != second {
		t.Error("e past the last error should stay put and say so")
	}
	if m = key(m, "E"); m.logView.YOffset != first {
		t.Errorf("E: YOffset = %d, want %d", m.logView.YOffset, first)
	}
	// Scrolling to the top and pressing e finds the first error again.
	if m = key(key(m, "g"), "e"); m.logView.YOffset != first {
		t.Errorf("g then e: YOffset = %d, want %d", m.logView.YOffset, first)
	}
}

func TestLogsOpenPositions(t *testing.T) {
	ok := logsOf(t, "CI / lint", failingLog())
	if ok.logView.YOffset != 0 {
		t.Errorf("a passing job's log should open at the top, YOffset = %d", ok.logView.YOffset)
	}

	noMarkers := strings.ReplaceAll(failingLog(), "##[error]", "")
	m := logsOf(t, "CI / test", noMarkers)
	if !m.logView.AtBottom() {
		t.Error("a failed job without error markers should open at the end, where failures usually are")
	}
	if m = key(m, "e"); !strings.Contains(m.View(), "no errors in this log") {
		t.Errorf("e without errors should say so, got:\n%s", m.View())
	}
}

func TestLogsTimestampsToggle(t *testing.T) {
	m := logsOf(t, "CI / test", failingLog())
	if strings.Contains(plain(m.View()), "10:01:02 ") {
		t.Error("timestamps should be hidden by default")
	}
	m = key(m, "t")
	if !strings.Contains(plain(m.View()), "10:01:02 ✗ first error") {
		t.Errorf("t should show timestamps\n---\n%s", plain(m.View()))
	}
	if m = key(m, "t"); m.logShowTime {
		t.Error("t should toggle timestamps off again")
	}
}

func TestLogsGuards(t *testing.T) {
	cases := []struct{ label, warning string }{
		{"Deploy / ship", "Deploy / ship is still running; its log is available once it finishes"},
		{"ci/jenkins", "ci/jenkins is not a GitHub Actions job; open it with o"},
	}
	for _, c := range cases {
		n, cmd := checksAt(t, withChecks(t, rerunFixture()), c.label).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
		m := n.(model)
		if cmd != nil || m.mode != modeChecks {
			t.Errorf("%s: should not open a log", c.label)
		}
		if !strings.Contains(m.View(), c.warning) {
			t.Errorf("%s: want %q\n---\n%s", c.label, c.warning, m.View())
		}
	}
}

func TestLogsStates(t *testing.T) {
	m := checksAt(t, withChecks(t, rerunFixture()), "CI / test")
	n, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	m = n.(model)
	v := m.View()
	if !strings.Contains(v, "loading log...") || strings.Contains(v, "%") {
		t.Errorf("while loading: want a loading message and no scroll position\n---\n%s", v)
	}

	// A log for a job the user already left is dropped.
	n, _ = m.Update(logsMsg{jobID: 999, text: "stale"})
	if n.(model).logLoaded {
		t.Error("a log for another job must not be shown")
	}

	n, _ = m.Update(logsMsg{jobID: m.logCheck.JobID, err: errors.New("fetching logs: HTTP 410: Logs have expired")})
	if v := n.(model).View(); !strings.Contains(v, "Logs have expired") {
		t.Errorf("want the error, got:\n%s", v)
	}
	n, _ = m.Update(logsMsg{jobID: m.logCheck.JobID, text: "\ufeff"})
	if v := n.(model).View(); !strings.Contains(v, "The log is empty.") {
		t.Errorf("want an empty-log notice, got:\n%s", v)
	}
}

func TestLogsKeys(t *testing.T) {
	m := logsOf(t, "CI / lint", failingLog())
	if m = key(m, "j"); m.logView.YOffset != 1 {
		t.Errorf("j: YOffset = %d, want 1", m.logView.YOffset)
	}
	if m = key(m, "G"); !m.logView.AtBottom() {
		t.Error("G should go to the end")
	}
	if !strings.Contains(m.View(), "100% ·") {
		t.Errorf("footer should show the scroll position\n---\n%s", m.View())
	}
	for _, k := range []string{"esc", "q", "l"} {
		var n tea.Model
		if k == "esc" {
			n, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		} else {
			n, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		}
		if n.(model).mode != modeChecks {
			t.Errorf("%s should go back to the checks", k)
		}
	}
	n, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	if cmd == nil || n.(model).logLoaded {
		t.Error("R should download the log again")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c should return tea.Quit")
	}
}

func TestLogsRewrapOnResize(t *testing.T) {
	long := "2026-09-01T10:00:00Z " + strings.Repeat("y", 150) + "\n"
	m := logsOf(t, "CI / lint", long)
	if m.logView.TotalLineCount() != 2 {
		t.Fatalf("150 cells at width 120 should wrap onto 2 rows, got %d", m.logView.TotalLineCount())
	}
	n, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 40})
	if got := n.(model).logView.TotalLineCount(); got != 3 {
		t.Errorf("after narrowing to 60 cells: %d rows, want 3", got)
	}
}
