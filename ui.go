package main

import (
	"fmt"
	"os"
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
	mode         mode
	loading      bool
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
	return tea.Batch(m.loadCmd(), m.spinner.Tick)
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

	case errMsg:
		m.loading = false
		m.status = errStyle.Render("! " + msg.err.Error())
		return m, nil

	case tea.KeyMsg:
		if m.mode == modeReply {
			return m.updateReply(msg)
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
		m.mode = modeReply
		m.status = ""
		m.reply.Reset()
		m.reply.Placeholder = "Reply to @" + firstAuthor(t) + "..."
		if msg.String() == "Q" {
			m.insertQuote(t)
		}
		m.layout()
		return m, m.reply.Focus()

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
		return m, tea.Batch(m.loadCmd(), m.spinner.Tick)

	case "o":
		t, ok := m.selected()
		if !ok || t.URL() == "" {
			return m, nil
		}
		b := browser.New("", os.Stdout, os.Stderr)
		if err := b.Browse(t.URL()); err != nil {
			m.status = errStyle.Render("! " + err.Error())
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
			m.insertQuote(t)
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

	if diff := renderDiff(t, w, m.fullDiff); diff != "" {
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
	m.detail.GotoTop()
}

// diffContext is how many lines above the commented range the compact diff
// view keeps, matching what github.com shows next to a review comment.
const diffContext = 3

// renderDiff colours the thread's diff hunk. Unless full is set, it keeps only
// the tail of the hunk: the commented lines plus a little context above them.
func renderDiff(t Thread, width int, full bool) string {
	header, lines, trimmed := diffLines(t, full)
	if len(lines) == 0 && header == "" {
		return ""
	}

	var out []string
	if header != "" {
		out = append(out, accentStyle.Render(truncate(header, width)))
	}
	if trimmed {
		out = append(out, dimStyle.Render("⋮"))
	}
	for _, l := range lines {
		l = truncate(strings.ReplaceAll(l, "\t", "    "), width)
		switch {
		case strings.HasPrefix(l, "+"):
			l = okStyle.Render(l)
		case strings.HasPrefix(l, "-"):
			l = errStyle.Render(l)
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// diffLines splits the thread's hunk into its "@@" header and code lines,
// keeping only the tail around the commented range unless full is set.
func diffLines(t Thread, full bool) (header string, lines []string, trimmed bool) {
	hunk := strings.TrimRight(t.DiffHunk(), "\n")
	if hunk == "" {
		return "", nil, false
	}
	lines = strings.Split(hunk, "\n")
	if strings.HasPrefix(lines[0], "@@") {
		header, lines = lines[0], lines[1:]
	}
	if !full {
		keep := diffContext + 1
		if t.StartLine != nil && t.Line != nil && *t.Line > *t.StartLine {
			keep += *t.Line - *t.StartLine
		}
		if len(lines) > keep {
			lines = lines[len(lines)-keep:]
			trimmed = true
		}
	}
	return header, lines, trimmed
}

// quoteDiff renders the same lines the diff view shows as a fenced ```diff
// block, which github.com highlights. The fence grows if the code itself
// contains backticks.
func quoteDiff(t Thread, full bool) string {
	_, lines, _ := diffLines(t, full)
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

// insertQuote puts the selected thread's diff into the reply at the cursor,
// starting on a fresh line and leaving the cursor below it ready to type.
func (m *model) insertQuote(t Thread) {
	quote := quoteDiff(t, m.fullDiff)
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

// --- view -------------------------------------------------------------------

func (m model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	var b strings.Builder
	b.WriteString(m.viewHeader())
	b.WriteString("\n\n")

	if m.loading {
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
	if m.mode == modeReply {
		keys = "ctrl+s send · ctrl+q quote diff · esc cancel"
	} else {
		keys = "j/k move · J/K scroll · d diff · r reply · Q quote+reply · t resolve · a show resolved · o open · R refresh · q quit"
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
