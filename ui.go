package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/cli/go-gh/v2/pkg/browser"
)

type mode int

const (
	modeBrowse mode = iota
	modeReply
	modeDiff   // picking lines of the diff to quote
	modeChecks // the CI checks on the PR's head commit
)

// --- messages ---------------------------------------------------------------

type loadedMsg struct {
	title   string
	threads []Thread
}

type repliedMsg struct {
	threadID string
	comment  Comment
}

type resolvedMsg struct {
	threadID string
	resolved bool
}

type checksMsg struct {
	sha    string
	checks []Check
	err    error
}

type errMsg struct{ err error }

// --- styles -----------------------------------------------------------------

var (
	colDim    = lipgloss.AdaptiveColor{Light: "#6e7781", Dark: "#8b949e"}
	colAccent = lipgloss.AdaptiveColor{Light: "#0969da", Dark: "#58a6ff"}
	colOK     = lipgloss.AdaptiveColor{Light: "#1a7f37", Dark: "#3fb950"}
	colWarn   = lipgloss.AdaptiveColor{Light: "#9a6700", Dark: "#d29922"}
	colErr    = lipgloss.AdaptiveColor{Light: "#cf222e", Dark: "#f85149"}
	colBorder = lipgloss.AdaptiveColor{Light: "#d0d7de", Dark: "#30363d"}

	headerStyle   = lipgloss.NewStyle().Bold(true)
	dimStyle      = lipgloss.NewStyle().Foreground(colDim)
	accentStyle   = lipgloss.NewStyle().Foreground(colAccent)
	okStyle       = lipgloss.NewStyle().Foreground(colOK)
	warnStyle     = lipgloss.NewStyle().Foreground(colWarn)
	errStyle      = lipgloss.NewStyle().Foreground(colErr)
	selectedStyle = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	paneStyle     = lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).
			BorderForeground(colBorder).PaddingLeft(1)
)

// --- model ------------------------------------------------------------------

type model struct {
	client *Client
	owner  string
	repo   string
	number int

	prTitle string
	threads []Thread
	visible []int // indices into threads, after filtering
	cursor  int   // position within visible
	offset  int   // first visible row of the list pane

	showResolved bool
	fullDiff     bool // show the whole diff hunk instead of just the commented lines
	selAnchor    int  // modeDiff: where the selection started, as an index into the hunk lines
	selCursor    int  // modeDiff: the moving end of the selection
	mode         mode
	loading      bool

	checks       []Check // sorted: failing first, then pending, passed, skipped
	checksSHA    string
	checksLoaded bool
	checksErr    error
	checkCursor  int
	checkOffset  int
	status       string
	err          error

	width  int
	height int

	detail  viewport.Model
	reply   textarea.Model
	spinner spinner.Model
}

func newModel(client *Client, owner, repo string, number int) model {
	ta := textarea.New()
	ta.Placeholder = "Write a reply..."
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0 // quoted diffs can be long; never drop lines

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = accentStyle

	return model{
		client:  client,
		owner:   owner,
		repo:    repo,
		number:  number,
		loading: true,
		detail:  viewport.New(0, 0),
		reply:   ta,
		spinner: sp,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(), m.checksCmd(), m.spinner.Tick)
}

// --- commands ---------------------------------------------------------------

func (m model) loadCmd() tea.Cmd {
	return func() tea.Msg {
		title, threads, err := m.client.ReviewThreads(m.owner, m.repo, m.number)
		if err != nil {
			return errMsg{err}
		}
		return loadedMsg{title: title, threads: threads}
	}
}

func (m model) checksCmd() tea.Cmd {
	return func() tea.Msg {
		sha, checks, err := m.client.Checks(m.owner, m.repo, m.number)
		return checksMsg{sha: sha, checks: checks, err: err}
	}
}

func (m model) replyCmd(threadID, body string) tea.Cmd {
	return func() tea.Msg {
		comment, err := m.client.Reply(threadID, body)
		if err != nil {
			return errMsg{err}
		}
		return repliedMsg{threadID: threadID, comment: comment}
	}
}

func (m model) resolveCmd(threadID string, resolved bool) tea.Cmd {
	return func() tea.Msg {
		if err := m.client.SetResolved(threadID, resolved); err != nil {
			return errMsg{err}
		}
		return resolvedMsg{threadID: threadID, resolved: resolved}
	}
}

// --- update -----------------------------------------------------------------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case spinner.TickMsg:
		if !m.loading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case loadedMsg:
		m.loading = false
		m.prTitle = msg.title
		m.threads = msg.threads
		m.refilter()
		m.layout()
		m.renderDetail()
		return m, nil

	case repliedMsg:
		if i, ok := m.indexOf(msg.threadID); ok {
			t := m.threads[i]
			t.Comments.Nodes = append(t.Comments.Nodes, msg.comment)
			m.threads[i] = t
		}
		m.status = okStyle.Render("✓ reply posted")
		m.renderDetail()
		m.detail.GotoBottom()
		return m, nil

	case resolvedMsg:
		if i, ok := m.indexOf(msg.threadID); ok {
			m.threads[i].IsResolved = msg.resolved
		}
		if msg.resolved {
			m.status = okStyle.Render("✓ thread resolved")
		} else {
			m.status = okStyle.Render("✓ thread unresolved")
		}
		m.refilter()
		m.renderDetail()
		return m, nil

	case checksMsg:
		m.checksLoaded = true
		m.checksErr = msg.err
		m.checksSHA = msg.sha
		m.checks = sortChecks(msg.checks)
		m.checkCursor = clamp(m.checkCursor, 0, max(0, len(m.checks)-1))
		m.clampCheckOffset()
		return m, nil

	case errMsg:
		m.loading = false
		m.status = errStyle.Render("! " + msg.err.Error())
		return m, nil

	case tea.KeyMsg:
		switch m.mode {
		case modeReply:
			return m.updateReply(msg)
		case modeDiff:
			return m.updateDiff(msg)
		case modeChecks:
			return m.updateChecks(msg)
		}
		return m.updateBrowse(msg)
	}

	var cmd tea.Cmd
	m.detail, cmd = m.detail.Update(msg)
	return m, cmd
}

func (m model) updateBrowse(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit

	case "j", "down":
		m.move(1)
		return m, nil
	case "k", "up":
		m.move(-1)
		return m, nil
	case "g", "home":
		m.cursor = 0
		m.clampOffset()
		m.renderDetail()
		return m, nil
	case "G", "end":
		m.cursor = max(0, len(m.visible)-1)
		m.clampOffset()
		m.renderDetail()
		return m, nil

	case "ctrl+d", "pgdown", "J":
		m.detail.HalfPageDown()
		return m, nil
	case "ctrl+u", "pgup", "K":
		m.detail.HalfPageUp()
		return m, nil

	case "d":
		m.fullDiff = !m.fullDiff
		m.renderDetail()
		return m, nil

	case "a":
		m.showResolved = !m.showResolved
		m.refilter()
		m.renderDetail()
		return m, nil

	case "r", "Q":
		t, ok := m.selected()
		if !ok {
			return m, nil
		}
		if msg.String() == "r" {
			return m, m.startReply(t, "")
		}
		cmd := m.startReply(t, quoteDiff(t, m.fullDiff))
		if m.reply.Value() == "" {
			m.status = warnStyle.Render("! no diff to quote")
		}
		return m, cmd

	case "v":
		t, ok := m.selected()
		if !ok {
			return m, nil
		}
		_, lines := hunkLines(t)
		if len(lines) == 0 {
			m.status = warnStyle.Render("! no diff on this thread")
			return m, nil
		}
		// Start with the commented lines selected, as github.com marks them.
		m.selCursor = len(lines) - 1
		m.selAnchor = m.selCursor
		if t.StartLine != nil && t.Line != nil && *t.Line > *t.StartLine {
			m.selAnchor -= *t.Line - *t.StartLine
		}
		m.mode = modeDiff
		m.status = ""
		m.renderDetail()
		return m, nil

	case "t":
		t, ok := m.selected()
		if !ok {
			return m, nil
		}
		m.status = dimStyle.Render("updating...")
		return m, m.resolveCmd(t.ID, !t.IsResolved)

	case "R", "ctrl+r":
		m.loading = true
		m.status = ""
		return m, tea.Batch(m.loadCmd(), m.checksCmd(), m.spinner.Tick)

	case "w":
		m.mode = modeChecks
		m.status = ""
		return m, nil

	case "o":
		if t, ok := m.selected(); ok {
			m.openURL(t.URL())
		}
		return m, nil
	}
	return m, nil
}

func (m model) updateReply(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeBrowse
		m.reply.Blur()
		m.layout()
		return m, nil

	case "ctrl+q":
		if t, ok := m.selected(); ok {
			m.insertQuote(quoteDiff(t, m.fullDiff))
		}
		return m, nil

	case "ctrl+s", "alt+enter":
		body := strings.TrimSpace(m.reply.Value())
		if body == "" {
			m.status = warnStyle.Render("! reply is empty")
			return m, nil
		}
		t, ok := m.selected()
		if !ok {
			return m, nil
		}
		m.mode = modeBrowse
		m.reply.Blur()
		m.reply.Reset()
		m.layout()
		m.status = dimStyle.Render("posting reply...")
		return m, m.replyCmd(t.ID, body)
	}

	var cmd tea.Cmd
	m.reply, cmd = m.reply.Update(msg)
	return m, cmd
}

// updateDiff handles keys while picking lines of the diff. The selection runs
// from the anchor to the cursor; j/k move the cursor and space re-anchors.
func (m model) updateDiff(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	t, ok := m.selected()
	if !ok {
		m.mode = modeBrowse
		return m, nil
	}
	_, lines := hunkLines(t)
	if len(lines) == 0 {
		m.mode = modeBrowse
		m.renderDetail()
		return m, nil
	}
	first := diffStart(t, len(lines), m.fullDiff)

	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "v", "q":
		m.mode = modeBrowse
	case "j", "down":
		m.selCursor++
	case "k", "up":
		m.selCursor--
	case "g", "home":
		m.selCursor = first
	case "G", "end":
		m.selCursor = len(lines) - 1
	case " ":
		m.selAnchor = m.selCursor
	case "d":
		m.fullDiff = !m.fullDiff
	case "ctrl+d", "pgdown", "J":
		m.detail.HalfPageDown()
		return m, nil
	case "ctrl+u", "pgup", "K":
		m.detail.HalfPageUp()
		return m, nil
	case "enter", "Q", "r":
		sel := m.selection()
		return m, m.startReply(t, fenceDiff(lines[sel.from:sel.to+1]))
	default:
		return m, nil
	}
	m.renderDetail()
	return m, nil
}

// updateChecks handles keys on the checks view.
func (m model) updateChecks(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "w", "q":
		m.mode = modeBrowse
	case "j", "down":
		m.checkCursor = clamp(m.checkCursor+1, 0, max(0, len(m.checks)-1))
	case "k", "up":
		m.checkCursor = clamp(m.checkCursor-1, 0, max(0, len(m.checks)-1))
	case "g", "home":
		m.checkCursor = 0
	case "G", "end":
		m.checkCursor = max(0, len(m.checks)-1)
	case "o", "enter":
		if m.checkCursor < len(m.checks) {
			m.openURL(m.checks[m.checkCursor].URL)
		}
	case "R", "ctrl+r":
		m.checksLoaded = false
		m.status = ""
		return m, m.checksCmd()
	}
	m.clampCheckOffset()
	return m, nil
}

// openURL opens url in the browser, reporting failures in the footer.
func (m *model) openURL(url string) {
	if url == "" {
		return
	}
	b := browser.New("", os.Stdout, os.Stderr)
	if err := b.Browse(url); err != nil {
		m.status = errStyle.Render("! " + err.Error())
	}
}

// startReply opens the reply box for t, pre-filled with quote if non-empty.
func (m *model) startReply(t Thread, quote string) tea.Cmd {
	m.mode = modeReply
	m.status = ""
	m.reply.Reset()
	m.reply.Placeholder = "Reply to @" + firstAuthor(t) + "..."
	if quote != "" {
		m.insertQuote(quote)
	}
	m.layout()
	return m.reply.Focus()
}

// --- helpers ----------------------------------------------------------------

func (m *model) move(delta int) {
	if len(m.visible) == 0 {
		return
	}
	m.cursor = clamp(m.cursor+delta, 0, len(m.visible)-1)
	m.clampOffset()
	m.renderDetail()
}

func (m *model) refilter() {
	m.visible = m.visible[:0]
	for i, t := range m.threads {
		if t.IsResolved && !m.showResolved {
			continue
		}
		m.visible = append(m.visible, i)
	}
	m.cursor = clamp(m.cursor, 0, max(0, len(m.visible)-1))
	m.clampOffset()
}

func (m *model) clampOffset() {
	h := m.listCapacity()
	if h <= 0 {
		m.offset = 0
		return
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m model) selected() (Thread, bool) {
	if m.cursor < 0 || m.cursor >= len(m.visible) {
		return Thread{}, false
	}
	return m.threads[m.visible[m.cursor]], true
}

func (m model) indexOf(threadID string) (int, bool) {
	for i, t := range m.threads {
		if t.ID == threadID {
			return i, true
		}
	}
	return 0, false
}

func (m model) listWidth() int {
	w := m.width / 3
	return clamp(w, 24, 52)
}

func (m model) bodyHeight() int {
	h := m.height - 2 /*header*/ - 2 /*footer*/
	if m.mode == modeReply {
		h -= replyBoxHeight
	}
	return max(3, h)
}

// listCapacity is how many threads fit in the list pane. Each thread occupies
// two rows: the file location and a preview of the opening comment.
func (m model) listCapacity() int { return max(1, m.bodyHeight()/2) }

const replyBoxHeight = 8

func (m *model) layout() {
	if m.width == 0 {
		return
	}
	detailWidth := max(20, m.width-m.listWidth()-3)
	m.detail.Width = detailWidth
	m.detail.Height = m.bodyHeight()
	m.reply.SetWidth(max(20, m.width-2))
	m.reply.SetHeight(replyBoxHeight - 3)
	m.clampOffset()
	m.renderDetail()
}

// renderDetail rebuilds the conversation pane for the selected thread.
func (m *model) renderDetail() {
	t, ok := m.selected()
	if !ok {
		m.detail.SetContent(dimStyle.Render("No review threads to show."))
		return
	}

	w := max(20, m.detail.Width)
	wrap := lipgloss.NewStyle().Width(w)

	var b strings.Builder
	b.WriteString(headerStyle.Render(t.Location()))
	b.WriteString("  ")
	b.WriteString(statusBadge(t))
	b.WriteString("\n\n")

	var sel *lineRange
	cursorRow := 0
	if header, lines := hunkLines(t); m.mode == modeDiff && len(lines) == 0 {
		m.mode = modeBrowse // the thread under the cursor changed to one without a diff
	} else if m.mode == modeDiff {
		first := diffStart(t, len(lines), m.fullDiff)
		m.selCursor = clamp(m.selCursor, first, len(lines)-1)
		m.selAnchor = clamp(m.selAnchor, first, len(lines)-1)
		r := m.selection()
		sel = &r
		// Location line, blank line, then the diff's own header rows.
		cursorRow = 2 + m.selCursor - first
		if header != "" {
			cursorRow++
		}
		if first > 0 {
			cursorRow++
		}
	}

	if diff := renderDiff(t, w, m.fullDiff, sel); diff != "" {
		b.WriteString(diff)
		b.WriteString("\n\n")
	}

	for i, c := range t.Comments.Nodes {
		if i > 0 {
			b.WriteString("\n")
		}
		author := c.Author.Login
		if author == "" {
			author = "ghost"
		}
		b.WriteString(accentStyle.Render("@" + author))
		b.WriteString(dimStyle.Render("  " + humanTime(c.CreatedAt)))
		b.WriteString("\n")
		b.WriteString(wrap.Render(strings.TrimRight(c.Body, "\n")))
		b.WriteString("\n")
	}

	m.detail.SetContent(b.String())
	if m.mode != modeDiff {
		m.detail.GotoTop()
		return
	}
	// Scroll just enough to keep the selection cursor on screen.
	if cursorRow < m.detail.YOffset {
		m.detail.SetYOffset(cursorRow)
	} else if cursorRow >= m.detail.YOffset+m.detail.Height {
		m.detail.SetYOffset(cursorRow - m.detail.Height + 1)
	}
}

// diffContext is how many lines above the commented range the compact diff
// view keeps, matching what github.com shows next to a review comment.
const diffContext = 3

// renderDiff colours the thread's diff hunk. Unless full is set, it keeps only
// the tail of the hunk: the commented lines plus a little context above them.
//
// When sel is non-nil, a gutter marks the selected lines.
func renderDiff(t Thread, width int, full bool, sel *lineRange) string {
	header, lines := hunkLines(t)
	if len(lines) == 0 && header == "" {
		return ""
	}
	start := diffStart(t, len(lines), full)
	trimmed := start > 0
	lines = lines[start:]
	if sel != nil {
		width -= 2
	}

	var out []string
	if header != "" {
		out = append(out, accentStyle.Render(truncate(header, width)))
	}
	if trimmed {
		out = append(out, dimStyle.Render("⋮"))
	}
	for i, l := range lines {
		l = truncate(strings.ReplaceAll(l, "\t", "    "), width)
		switch {
		case strings.HasPrefix(l, "+"):
			l = okStyle.Render(l)
		case strings.HasPrefix(l, "-"):
			l = errStyle.Render(l)
		}
		if sel != nil {
			gutter := "  "
			if sel.contains(start + i) {
				gutter = selectedStyle.Render("▌ ")
			}
			l = gutter + l
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// diffLines splits the thread's hunk into its "@@" header and code lines,
// keeping only the tail around the commented range unless full is set.
func diffLines(t Thread, full bool) (header string, lines []string, trimmed bool) {
	header, lines = hunkLines(t)
	start := diffStart(t, len(lines), full)
	return header, lines[start:], start > 0
}

// hunkLines splits the thread's hunk into its "@@" header and code lines.
func hunkLines(t Thread) (header string, lines []string) {
	hunk := strings.TrimRight(t.DiffHunk(), "\n")
	if hunk == "" {
		return "", nil
	}
	lines = strings.Split(hunk, "\n")
	if strings.HasPrefix(lines[0], "@@") {
		header, lines = lines[0], lines[1:]
	}
	return header, lines
}

// diffStart is the index of the first of n hunk lines the diff view shows.
func diffStart(t Thread, n int, full bool) int {
	if full {
		return 0
	}
	keep := diffContext + 1
	if t.StartLine != nil && t.Line != nil && *t.Line > *t.StartLine {
		keep += *t.Line - *t.StartLine
	}
	return max(0, n-keep)
}

// lineRange is an inclusive range of indices into a hunk's code lines.
type lineRange struct{ from, to int }

func (r lineRange) contains(i int) bool { return i >= r.from && i <= r.to }

// selection is the lines picked in modeDiff, in top-to-bottom order.
func (m model) selection() lineRange {
	return lineRange{min(m.selAnchor, m.selCursor), max(m.selAnchor, m.selCursor)}
}

// quoteDiff quotes the same lines the diff view shows.
func quoteDiff(t Thread, full bool) string {
	_, lines, _ := diffLines(t, full)
	return fenceDiff(lines)
}

// fenceDiff wraps diff lines in a fenced ```diff block, which github.com
// highlights. The fence grows if the code itself contains backticks.
func fenceDiff(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	code := strings.Join(lines, "\n")
	fence := "```"
	for strings.Contains(code, fence) {
		fence += "`"
	}
	return fence + "diff\n" + code + "\n" + fence + "\n"
}

// insertQuote puts a quote into the reply at the cursor, starting on a fresh
// line and leaving the cursor below it ready to type.
func (m *model) insertQuote(quote string) {
	if quote == "" {
		m.status = warnStyle.Render("! no diff to quote")
		return
	}
	if m.reply.Value() != "" && !strings.HasSuffix(m.reply.Value(), "\n") {
		quote = "\n" + quote
	}
	m.reply.InsertString(quote + "\n")
	m.status = ""
}

func statusBadge(t Thread) string {
	switch {
	case t.IsResolved:
		return okStyle.Render("resolved")
	case t.IsOutdated:
		return warnStyle.Render("outdated")
	default:
		return warnStyle.Render("open")
	}
}

func firstAuthor(t Thread) string {
	if len(t.Comments.Nodes) == 0 {
		return "?"
	}
	if t.Comments.Nodes[0].Author.Login == "" {
		return "ghost"
	}
	return t.Comments.Nodes[0].Author.Login
}

func humanTime(ts string) string {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(parsed)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return parsed.Format("2006-01-02")
	}
}

// --- checks -----------------------------------------------------------------

// sortChecks orders checks the way `gh pr checks` does: what needs attention
// first, then by workflow and job name.
func sortChecks(checks []Check) []Check {
	sorted := append([]Check(nil), checks...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.State != b.State {
			return a.State < b.State
		}
		return a.Label() < b.Label()
	})
	return sorted
}

func (m *model) clampCheckOffset() {
	h := m.bodyHeight()
	if m.checkCursor < m.checkOffset {
		m.checkOffset = m.checkCursor
	}
	if m.checkCursor >= m.checkOffset+h {
		m.checkOffset = m.checkCursor - h + 1
	}
	m.checkOffset = max(0, m.checkOffset)
}

func checkIcon(s CheckState) string {
	switch s {
	case CheckPass:
		return okStyle.Render("✓")
	case CheckFail:
		return errStyle.Render("✗")
	case CheckPending:
		return warnStyle.Render("●")
	default:
		return dimStyle.Render("-")
	}
}

// checksSummary is the one-line CI state for the header, or "" when there is
// nothing to report yet.
func checksSummary(checks []Check) string {
	var fail, pending, pass int
	for _, c := range checks {
		switch c.State {
		case CheckFail:
			fail++
		case CheckPending:
			pending++
		case CheckPass:
			pass++
		}
	}
	switch {
	case fail > 0:
		return errStyle.Render(fmt.Sprintf("✗ %d failing", fail))
	case pending > 0:
		return warnStyle.Render(fmt.Sprintf("● %d pending", pending))
	case pass > 0:
		return okStyle.Render(fmt.Sprintf("✓ %d passed", pass))
	case len(checks) > 0:
		return dimStyle.Render(fmt.Sprintf("- %d skipped", len(checks)))
	}
	return ""
}

// checkTiming is how long a finished check took, or how long a running one has
// been going.
func checkTiming(c Check, now time.Time) string {
	start, err := time.Parse(time.RFC3339, c.StartedAt)
	if err != nil {
		return ""
	}
	if c.State == CheckPending {
		return "running " + formatDuration(now.Sub(start))
	}
	end, err := time.Parse(time.RFC3339, c.CompletedAt)
	// Skipped jobs can report a start after their end.
	if err != nil || end.Before(start) || c.State == CheckSkipped {
		return ""
	}
	return formatDuration(end.Sub(start))
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func (m model) viewChecks() string {
	w, h := max(20, m.width-1), m.bodyHeight()
	box := lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h)

	switch {
	case !m.checksLoaded:
		return box.Render(dimStyle.Render("loading checks..."))
	case m.checksErr != nil:
		return box.Render(errStyle.Render("! " + m.checksErr.Error()))
	case len(m.checks) == 0:
		return box.Render(dimStyle.Render("No checks have run on the head commit."))
	}

	now := time.Now()
	var rows []string
	for i := m.checkOffset; i < len(m.checks) && i < m.checkOffset+h; i++ {
		c := m.checks[i]
		detail := c.Detail
		if timing := checkTiming(c, now); timing != "" {
			detail += " · " + timing
		}
		detail = dimStyle.Render(truncate(detail, w/3))

		label := truncate(c.Label(), w-4-lipgloss.Width(detail))
		if i == m.checkCursor {
			label = selectedStyle.Render(label)
		}
		left := checkIcon(c.State) + " " + label
		gap := max(1, w-lipgloss.Width(left)-lipgloss.Width(detail))
		rows = append(rows, left+strings.Repeat(" ", gap)+detail)
	}
	return box.Render(strings.Join(rows, "\n"))
}

// --- view -------------------------------------------------------------------

func (m model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	var b strings.Builder
	b.WriteString(m.viewHeader())
	b.WriteString("\n\n")

	if m.mode == modeChecks {
		b.WriteString(m.viewChecks())
	} else if m.loading {
		b.WriteString(m.spinner.View() + dimStyle.Render(" loading review threads..."))
		b.WriteString(strings.Repeat("\n", max(0, m.bodyHeight()-1)))
	} else {
		body := lipgloss.JoinHorizontal(
			lipgloss.Top,
			m.viewList(),
			paneStyle.Height(m.bodyHeight()).Render(m.detail.View()),
		)
		b.WriteString(body)
	}

	if m.mode == modeReply {
		b.WriteString("\n")
		b.WriteString(m.viewReply())
	}

	b.WriteString("\n")
	b.WriteString(m.viewFooter())
	return b.String()
}

func (m model) viewHeader() string {
	left := headerStyle.Render(fmt.Sprintf("%s/%s #%d", m.owner, m.repo, m.number))
	if m.prTitle != "" {
		left += dimStyle.Render("  " + m.prTitle)
	}

	open := 0
	for _, t := range m.threads {
		if !t.IsResolved {
			open++
		}
	}
	right := dimStyle.Render(fmt.Sprintf("%d open / %d total", open, len(m.threads)))
	if summary := checksSummary(m.checks); summary != "" {
		right += dimStyle.Render(" · ") + summary
	}

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return lipgloss.NewStyle().MaxWidth(m.width).Render(left)
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m model) viewList() string {
	w := m.listWidth()
	h := m.bodyHeight()

	if len(m.visible) == 0 {
		empty := dimStyle.Render("no threads")
		return lipgloss.NewStyle().Width(w).Height(h).Render(empty)
	}

	var rows []string
	for i := m.offset; i < len(m.visible) && i < m.offset+m.listCapacity(); i++ {
		t := m.threads[m.visible[i]]

		marker := " "
		switch {
		case t.IsResolved:
			marker = okStyle.Render("✓")
		case t.IsOutdated:
			marker = warnStyle.Render("~")
		default:
			marker = warnStyle.Render("●")
		}

		count := ""
		if n := len(t.Comments.Nodes); n > 1 {
			count = dimStyle.Render(fmt.Sprintf(" (%d)", n))
		}

		loc := truncate(t.Location(), w-4-lipgloss.Width(count))
		if i == m.cursor {
			loc = selectedStyle.Render(loc)
		}
		line1 := fmt.Sprintf("%s %s%s", marker, loc, count)
		line2 := "  " + dimStyle.Render(truncate(t.Preview(), w-3))

		rows = append(rows, line1, line2)
	}

	content := strings.Join(rows, "\n")
	return lipgloss.NewStyle().Width(w).MaxHeight(h).Height(h).Render(content)
}

func (m model) viewReply() string {
	t, _ := m.selected()
	title := accentStyle.Render("Reply") + dimStyle.Render("  "+t.Location())
	box := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), true, false, false, false).
		BorderForeground(colBorder).
		Width(max(20, m.width-1))
	return box.Render(title + "\n" + m.reply.View())
}

func (m model) viewFooter() string {
	if m.status != "" {
		return lipgloss.NewStyle().MaxWidth(m.width).Render(m.status)
	}
	var keys string
	switch m.mode {
	case modeReply:
		keys = "ctrl+s send · ctrl+q quote diff · esc cancel"
	case modeDiff:
		keys = "j/k extend selection · space restart here · d full hunk · enter quote+reply · esc back"
	case modeChecks:
		keys = "j/k move · o open run · R refresh checks · esc back"
		if m.checksSHA != "" {
			keys = "checks on " + m.checksSHA[:min(7, len(m.checksSHA))] + " · " + keys
		}
	default:
		keys = "j/k move · J/K scroll · d diff · v select lines · r reply · Q quote+reply · t resolve · a show resolved · w checks · o open · R refresh · q quit"
	}
	return dimStyle.Render(truncate(keys, m.width))
}

// --- small utilities --------------------------------------------------------

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	runes := []rune(s)
	if w <= 1 {
		return string(runes[:1])
	}
	return string(runes[:min(len(runes), w-1)]) + "…"
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
