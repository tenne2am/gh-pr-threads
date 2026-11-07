package main

import (
	"regexp"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// logKind is what a log line is, from GitHub Actions' "##[...]" markers.
type logKind int

const (
	logPlain logKind = iota
	logGroup
	logEndGroup
	logError
	logWarning
	logNotice
	logDebug
	logCommand
)

var logMarkers = []struct {
	prefix string
	kind   logKind
}{
	{"##[group]", logGroup},
	{"##[endgroup]", logEndGroup},
	{"##[error]", logError},
	{"##[warning]", logWarning},
	{"##[notice]", logNotice},
	{"##[debug]", logDebug},
	{"##[command]", logCommand},
}

// logLine is one line of a job log with its timestamp and marker removed.
type logLine struct {
	time string // "15:04:05", or "" if the line had no timestamp
	text string
	kind logKind
}

// logTimestamp matches the RFC 3339 stamp the runner puts on every line.
var logTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T(\d{2}:\d{2}:\d{2})(?:\.\d+)?Z ?`)

// parseLog splits a downloaded job log into lines.
func parseLog(raw string) []logLine {
	raw = strings.TrimPrefix(raw, "\ufeff")
	raw = strings.TrimRight(raw, "\r\n")
	if raw == "" {
		return nil
	}
	split := strings.Split(raw, "\n")
	lines := make([]logLine, 0, len(split))
	for _, s := range split {
		var l logLine
		if m := logTimestamp.FindStringSubmatch(s); m != nil {
			l.time = m[1]
			s = s[len(m[0]):]
		}
		for _, mk := range logMarkers {
			if strings.HasPrefix(s, mk.prefix) {
				l.kind = mk.kind
				s = s[len(mk.prefix):]
				break
			}
		}
		l.text = sanitizeLog(s)
		lines = append(lines, l)
	}
	return lines
}

// logEscape matches terminal escape sequences: CSI (colours, cursor moves,
// screen clears), OSC (window titles, hyperlinks, clipboard writes) and the
// other two-byte ESC forms.
var logEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[P^_][^\x1b]*(?:\x1b\\)?|\x1b[@-Z\\-_]?`)

// sanitizeLog makes a log line safe to draw. Logs are written by whatever the
// job ran, including code from the pull request, so every escape sequence and
// control character goes: only the extension decides what the terminal does.
func sanitizeLog(s string) string {
	s = logEscape.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0:
			return -1
		}
		return r
	}, s)
}

// wrapText hard-wraps s into lines at most width cells wide.
func wrapText(s string, width int) []string {
	if width <= 0 || runewidth.StringWidth(s) <= width {
		return []string{s}
	}
	var out []string
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > width && w > 0 {
			out = append(out, b.String())
			b.Reset()
			w = 0
		}
		b.WriteRune(r)
		w += rw
	}
	return append(out, b.String())
}

// renderLog draws the log for a pane width cells wide. It returns the content
// and the row each error starts on, for jumping between errors.
func renderLog(lines []logLine, width int, showTime bool) (string, []int) {
	var rows []string
	var errorRows []int
	for _, l := range lines {
		if l.kind == logEndGroup {
			continue
		}

		prefix := ""
		if showTime {
			stamp := l.time
			if stamp == "" {
				stamp = strings.Repeat(" ", 8)
			}
			prefix = dimStyle.Render(stamp) + " "
		}
		var marker string
		style := lipgloss.NewStyle()
		switch l.kind {
		case logGroup:
			marker, style = "▸ ", headerStyle.Foreground(colAccent)
		case logError:
			marker, style = "✗ ", errStyle
			errorRows = append(errorRows, len(rows))
		case logWarning:
			marker, style = "! ", warnStyle
		case logNotice:
			marker, style = "· ", accentStyle
		case logDebug, logCommand:
			style = dimStyle
		}
		if marker != "" {
			prefix += style.Render(marker)
		}

		indent := lipgloss.Width(prefix)
		for i, part := range wrapText(l.text, width-indent) {
			lead := prefix
			if i > 0 {
				lead = strings.Repeat(" ", indent)
			}
			rows = append(rows, lead+style.Render(part))
		}
	}
	return strings.Join(rows, "\n"), errorRows
}

// --- log view ---------------------------------------------------------------

// logContext is how many lines above an error stay visible when jumping to it.
const logContext = 3

// openLogs switches to the log of c and starts downloading it.
func (m *model) openLogs(c Check) tea.Cmd {
	switch {
	case c.JobID == 0:
		m.status = warnStyle.Render("! " + c.Label() + " is not a GitHub Actions job; open it with o")
		return nil
	case c.State == CheckPending:
		m.status = warnStyle.Render("! " + c.Label() + " is still running; its log is available once it finishes")
		return nil
	}
	m.mode = modeLogs
	m.status = ""
	m.logCheck = c
	m.logLines, m.logErr, m.logLoaded = nil, nil, false
	m.logView.SetContent("")
	return m.logsCmd(c)
}

func (m *model) renderLogView() {
	if !m.logLoaded || m.logErr != nil {
		return
	}
	content, errorRows := renderLog(m.logLines, m.logView.Width, m.logShowTime)
	m.logView.SetContent(content)
	m.logErrorRows = errorRows
}

// initialLogOffset is where a freshly loaded log opens: on the first error of
// a failed job, at the end of a failed job without error annotations (where
// the failure usually is), and at the top otherwise.
func (m model) initialLogOffset() int {
	if m.logCheck.State != CheckFail {
		return 0
	}
	if len(m.logErrorRows) > 0 {
		return max(0, m.logErrorRows[0]-logContext)
	}
	return max(0, m.logView.TotalLineCount()-m.logView.Height)
}

// jumpToError scrolls to the next (dir 1) or previous (dir -1) error.
func (m *model) jumpToError(dir int) {
	if len(m.logErrorRows) == 0 {
		m.status = dimStyle.Render("no errors in this log")
		return
	}
	at := m.logView.YOffset + logContext // the row a jump puts an error on
	maxOffset := max(0, m.logView.TotalLineCount()-m.logView.Height)
	for i := range m.logErrorRows {
		row := m.logErrorRows[i]
		if dir < 0 {
			row = m.logErrorRows[len(m.logErrorRows)-1-i]
		}
		if (dir > 0 && row <= at) || (dir < 0 && row >= at) {
			continue
		}
		// Errors near the end can't get context above them; the view stops at
		// the bottom instead. An error that wouldn't move the view is skipped,
		// so repeated jumps end with "no more errors" rather than sticking.
		offset := clamp(row-logContext, 0, maxOffset)
		if offset == m.logView.YOffset {
			continue
		}
		m.status = ""
		m.logView.SetYOffset(offset)
		return
	}
	m.status = dimStyle.Render("no more errors")
}

func (m model) updateLogs(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.status = ""
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "q", "l":
		m.mode = modeChecks
	case "j", "down":
		m.logView.LineDown(1)
	case "k", "up":
		m.logView.LineUp(1)
	case "ctrl+d", "pgdown", "J", " ":
		m.logView.HalfPageDown()
	case "ctrl+u", "pgup", "K":
		m.logView.HalfPageUp()
	case "g", "home":
		m.logView.GotoTop()
	case "G", "end":
		m.logView.GotoBottom()
	case "e":
		m.jumpToError(1)
	case "E":
		m.jumpToError(-1)
	case "t":
		// Keep the same log line at the top: wrapping can change row counts,
		// so go by the share of the log scrolled past.
		pct := m.logView.ScrollPercent()
		m.logShowTime = !m.logShowTime
		m.renderLogView()
		m.logView.SetYOffset(int(pct * float64(max(0, m.logView.TotalLineCount()-m.logView.Height))))
	case "o":
		m.openURL(m.logCheck.URL)
	case "R", "ctrl+r":
		return m, m.openLogs(m.logCheck)
	}
	return m, nil
}

func (m model) viewLogs() string {
	c := m.logCheck
	detail := "log"
	if c.Detail != "" {
		detail = c.Detail + " · log"
	}
	title := checkIcon(c.State) + " " + headerStyle.Render(c.Label()) + dimStyle.Render("  "+detail)
	title = lipgloss.NewStyle().MaxWidth(m.width).Render(title)

	h := m.bodyHeight() - 1
	box := lipgloss.NewStyle().Width(max(20, m.width-1)).Height(h).MaxHeight(h)
	var body string
	switch {
	case !m.logLoaded:
		body = box.Render(dimStyle.Render("loading log..."))
	case m.logErr != nil:
		body = box.Render(errStyle.Render("! " + m.logErr.Error()))
	case len(m.logLines) == 0:
		body = box.Render(dimStyle.Render("The log is empty."))
	default:
		body = m.logView.View()
	}
	return title + "\n" + body
}
