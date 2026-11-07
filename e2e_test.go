//go:build e2e

// End-to-end tests: build the real binary, run it in a pseudo-terminal, and
// point it at an in-process mock of the GitHub GraphQL API.
//
//	go test -tags e2e -run E2E ./...
//
// go-gh talks to the development host github.localhost over plain HTTP, so
// GH_HOST=github.localhost plus HTTP_PROXY routes every API call to the mock
// without any test hooks in the extension itself.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gh-pr-threads-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "gh-pr-threads")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building binary:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// --- mock GitHub ------------------------------------------------------------

// mockGitHub is a stateful stand-in for api.github.localhost/graphql: replies
// and resolutions change what the next threads query returns.
type mockGitHub struct {
	t *testing.T

	mu        sync.Mutex
	title     string
	threads   []Thread
	requests  []graphQLRequest
	failReply bool
	nextID    int
}

func newMockGitHub(t *testing.T) *mockGitHub {
	mk := func(id, path string, ln int, resolved bool, hunk, body string) Thread {
		th := Thread{ID: id, Path: path, Line: &ln, IsResolved: resolved}
		th.Comments.Nodes = []Comment{{
			ID: id + "-c1", Body: body, CreatedAt: "2026-09-01T10:00:00Z",
			URL: "https://github.com/octo/demo/pull/7#discussion_" + id, Author: Author{Login: "alice"},
			DiffHunk: hunk,
		}}
		return th
	}
	return &mockGitHub{
		t:     t,
		title: "Add nil checks",
		threads: []Thread{
			mk("T1", "main.go", 12, false, sampleHunk, "this needs a nil check"),
			mk("T2", "ui.go", 40, false, "@@ -38,3 +38,4 @@ func view() {\n \tleft := 1\n+\tpanes := 2", "why two panes here?"),
			mk("T3", "old.go", 7, true, "@@ -7 +7 @@\n+\tdone()", "already handled"),
		},
	}
}

func (g *mockGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Host != "api.github.localhost" || r.URL.Path != "/graphql" {
		g.t.Errorf("unexpected request %s %s%s", r.Method, r.Host, r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if got := r.Header.Get("Authorization"); !strings.Contains(got, "test-token") {
		g.t.Errorf("Authorization = %q, want the GH_TOKEN", got)
	}

	var req graphQLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		g.t.Errorf("decoding request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, req)

	var data interface{}
	switch {
	case strings.Contains(req.Query, "query PRReviewThreads"):
		if req.Variables["owner"] != "octo" || req.Variables["repo"] != "demo" || req.Variables["number"] != float64(7) {
			g.t.Errorf("threads query for the wrong PR: %v", req.Variables)
		}
		var resp threadsResponse
		resp.Repository.PullRequest.Title = g.title
		resp.Repository.PullRequest.ReviewThreads.Nodes = g.threads
		// Like GitHub, only return the hunk if the query selects it.
		if !strings.Contains(req.Query, "diffHunk") {
			resp.Repository.PullRequest.ReviewThreads.Nodes = withoutHunks(g.threads)
		}
		data = resp

	case strings.Contains(req.Query, "addPullRequestReviewThreadReply"):
		if g.failReply {
			writeJSON(w, map[string]interface{}{"errors": []map[string]string{{"message": "reply rejected by mock"}}})
			return
		}
		th := g.thread(req.Variables["threadId"])
		if th == nil {
			return
		}
		g.nextID++
		c := Comment{
			ID: fmt.Sprintf("R%d", g.nextID), Body: req.Variables["body"].(string),
			CreatedAt: time.Now().UTC().Format(time.RFC3339), Author: Author{Login: "me"},
		}
		th.Comments.Nodes = append(th.Comments.Nodes, c)
		data = map[string]interface{}{"addPullRequestReviewThreadReply": map[string]interface{}{"comment": c}}

	case strings.Contains(req.Query, "resolveReviewThread"), strings.Contains(req.Query, "unresolveReviewThread"):
		th := g.thread(req.Variables["threadId"])
		if th == nil {
			return
		}
		th.IsResolved = !strings.Contains(req.Query, "unresolveReviewThread")
		data = map[string]interface{}{"thread": map[string]interface{}{"id": th.ID, "isResolved": th.IsResolved}}

	default:
		g.t.Errorf("unexpected GraphQL operation: %s", req.Query)
		http.Error(w, "unknown operation", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]interface{}{"data": data})
}

func withoutHunks(threads []Thread) []Thread {
	out := make([]Thread, len(threads))
	for i, th := range threads {
		th.Comments.Nodes = append([]Comment(nil), th.Comments.Nodes...)
		for j := range th.Comments.Nodes {
			th.Comments.Nodes[j].DiffHunk = ""
		}
		out[i] = th
	}
	return out
}

// lastComment is the newest comment on the given thread, as the mock stores it.
func (g *mockGitHub) lastComment(id string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	nodes := g.thread(id).Comments.Nodes
	return nodes[len(nodes)-1].Body
}

func (g *mockGitHub) thread(id interface{}) *Thread {
	for i := range g.threads {
		if g.threads[i].ID == id {
			return &g.threads[i]
		}
	}
	g.t.Errorf("no thread %v", id)
	return nil
}

// replies returns the bodies of every reply mutation received so far.
func (g *mockGitHub) replies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, r := range g.requests {
		if strings.Contains(r.Query, "addPullRequestReviewThreadReply") {
			out = append(out, r.Variables["body"].(string))
		}
	}
	return out
}

func (g *mockGitHub) count(op string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, r := range g.requests {
		if strings.Contains(r.Query, op) {
			n++
		}
	}
	return n
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// --- terminal driver --------------------------------------------------------

// ansi matches CSI, OSC and charset escape sequences so the screen output can
// be searched as plain text.
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][A-Z0-9]|\x1b[=>]`)

type terminal struct {
	t       *testing.T
	pty     *os.File
	cmd     *exec.Cmd
	exited  chan struct{} // closed when the process ends
	exitErr error
	stderr  bytes.Buffer // kept apart: the pty can drop output when the process exits

	mu   sync.Mutex
	out  bytes.Buffer
	mark int // start of the output produced since the last send
}

// env is the whole environment the binary sees: nothing leaks in from the
// developer's shell (tokens, proxies, gh config).
func env(t *testing.T, proxy string, extra ...string) []string {
	home := t.TempDir()
	return append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GH_CONFIG_DIR=" + home,
		"GH_HOST=github.localhost",
		"GH_TOKEN=test-token",
		"GH_PROMPT_DISABLED=1",
		"HTTP_PROXY=" + proxy,
		// "screen" stops termenv from querying the terminal's background
		// colour, which would otherwise race with Bubble Tea for the input.
		"TERM=screen-256color",
		"COLORFGBG=15;0",
	}, extra...)
}

func startApp(t *testing.T, gh *mockGitHub, args []string, extraEnv ...string) *terminal {
	t.Helper()
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)

	term := &terminal{t: t, exited: make(chan struct{})}
	cmd := exec.Command(binPath, args...)
	cmd.Env = env(t, srv.URL, extraEnv...)
	cmd.Stderr = &term.stderr
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	term.pty, term.cmd = f, cmd
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			term.mu.Lock()
			term.out.Write(buf[:n])
			term.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		term.exitErr = cmd.Wait()
		close(term.exited)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		f.Close()
	})
	return term
}

// send types keys. Browse-mode keys must go one per call: Bubble Tea reads a
// burst of runes as a single key event.
func (term *terminal) send(keys string) {
	term.t.Helper()
	term.mu.Lock()
	term.mark = term.out.Len()
	term.mu.Unlock()
	if _, err := term.pty.Write([]byte(keys)); err != nil {
		term.t.Fatalf("sending %q: %v", keys, err)
	}
}

// expect waits until text appears in the output drawn since the last send.
// Bubble Tea only redraws changed lines, so expect what a key changes.
func (term *terminal) expect(text string) {
	term.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		term.mu.Lock()
		screen := ansi.ReplaceAllString(term.out.String()[term.mark:], "")
		term.mu.Unlock()
		if strings.Contains(screen, text) {
			return
		}
		select {
		case <-term.exited:
			term.t.Fatalf("process exited (%v) before %q appeared\nstderr: %s\noutput since last key:\n%s",
				term.exitErr, text, term.stderr.String(), screen)
		default:
		}
		if time.Now().After(deadline) {
			term.t.Fatalf("timed out waiting for %q; output since last key:\n%s", text, screen)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitExit waits for the process to end and returns its error.
func (term *terminal) waitExit() error {
	term.t.Helper()
	select {
	case <-term.exited:
		return term.exitErr
	case <-time.After(10 * time.Second):
		term.t.Fatal("process did not exit")
		return nil
	}
}

const (
	keyEnter = "\r"
	keyCtrlS = "\x13"
	keyCtrlQ = "\x11"
)

// --- scenarios --------------------------------------------------------------

func TestE2EDiffView(t *testing.T) {
	term := startApp(t, newMockGitHub(t), []string{"7", "--repo", "octo/demo"})
	term.expect("octo/demo #7")
	term.expect("Add nil checks")
	term.expect("2 open / 3 total")
	// Compact diff of the first thread: header, elision marker, commented code.
	term.expect("@@ -10,6 +10,8 @@ func run() error {")
	term.expect("⋮")
	term.expect("+    if x == nil {")
	term.expect("this needs a nil check")

	term.send("d")
	term.expect("a := 1") // the full hunk reaches its first line

	term.send("j")
	term.expect("+    panes := 2")
	term.expect("why two panes here?")

	term.send("q")
	if err := term.waitExit(); err != nil {
		t.Errorf("exit: %v", err)
	}
}

func TestE2EQuoteSelectedLines(t *testing.T) {
	gh := newMockGitHub(t)
	term := startApp(t, gh, []string{"7", "--repo", "octo/demo"})
	term.expect("this needs a nil check")

	term.send("v")
	term.expect("enter quote+reply")
	term.send("k")
	term.expect("▌")
	term.send(keyEnter)
	term.expect("```diff")
	term.send("LGTM")
	term.expect("LGTM")
	term.send(keyCtrlS)
	term.expect("reply posted")

	want := "```diff\n+        return nil\n+    }\n```\n\nLGTM"
	if got := gh.replies(); len(got) != 1 || got[0] != want {
		t.Errorf("replies = %q, want [%q]", got, want)
	}
	if got := gh.lastComment("T1"); got != want {
		t.Errorf("the reply should land on the commented thread T1, its last comment is %q", got)
	}
}

func TestE2EQuoteShownDiffWithQ(t *testing.T) {
	gh := newMockGitHub(t)
	term := startApp(t, gh, []string{"7", "--repo", "octo/demo"})
	term.expect("this needs a nil check")

	term.send("Q")
	term.expect("```diff")
	term.send(keyCtrlS)
	term.expect("reply posted")

	got := gh.replies()
	if len(got) != 1 || !strings.Contains(got[0], "-    old()") || strings.Contains(got[0], "a := 1") {
		t.Errorf("Q should quote exactly the compact diff, got %q", got)
	}
}

func TestE2ECtrlQQuotesWhileTyping(t *testing.T) {
	gh := newMockGitHub(t)
	term := startApp(t, gh, []string{"7", "--repo", "octo/demo"})
	term.expect("this needs a nil check")

	term.send("r")
	term.expect("ctrl+q quote diff")
	term.send("see:")
	term.expect("see:")
	term.send(keyCtrlQ)
	term.expect("```diff")
	term.send(keyCtrlS)
	term.expect("reply posted")

	if got := gh.replies(); len(got) != 1 || !strings.HasPrefix(got[0], "see:\n```diff\n") {
		t.Errorf("replies = %q, want the quote after the typed text", got)
	}
}

func TestE2EResolveAndReload(t *testing.T) {
	gh := newMockGitHub(t)
	term := startApp(t, gh, []string{"7", "--repo", "octo/demo"})
	term.expect("2 open / 3 total")

	term.send("t")
	term.expect("thread resolved")
	term.expect("1 open / 3 total")

	// Someone else replies on GitHub; R must fetch it. (A reload against the
	// mock finishes within a frame, so "loading..." is never drawn.)
	gh.mu.Lock()
	gh.threads[1].Comments.Nodes = append(gh.threads[1].Comments.Nodes,
		Comment{ID: "X1", Body: "pushed a fix in abc123", CreatedAt: "2026-09-02T10:00:00Z", Author: Author{Login: "bob"}})
	gh.mu.Unlock()

	term.send("R")
	term.expect("pushed a fix in abc123")
	if n := gh.count("PRReviewThreads"); n != 2 {
		t.Errorf("threads queried %d times, want 2", n)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if !gh.threads[0].IsResolved {
		t.Error("the resolve mutation should have reached the mock")
	}
}

func TestE2EReplyErrorIsShown(t *testing.T) {
	gh := newMockGitHub(t)
	gh.failReply = true
	term := startApp(t, gh, []string{"7", "--repo", "octo/demo"})
	term.expect("this needs a nil check")

	term.send("Q")
	term.expect("```diff")
	term.send(keyCtrlS)
	term.expect("reply rejected by mock")
}

func TestE2EPRForCurrentBranch(t *testing.T) {
	// A stand-in for gh that records its arguments and answers `pr view`.
	dir := t.TempDir()
	fakeGh := filepath.Join(dir, "gh")
	script := "#!/bin/sh\necho \"$@\" > " + filepath.Join(dir, "args") + "\necho '{\"number\":7}'\n"
	if err := os.WriteFile(fakeGh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	term := startApp(t, newMockGitHub(t), []string{"--repo", "octo/demo"}, "GH_PATH="+fakeGh)
	term.expect("octo/demo #7")

	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(args)); got != "pr view --json number --repo octo/demo" {
		t.Errorf("gh was called with %q", got)
	}
}

func TestE2EInvalidRepoFlag(t *testing.T) {
	cmd := exec.Command(binPath, "1", "--repo", "not-a-repo")
	cmd.Env = env(t, "http://127.0.0.1:1")
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("want exit code 1, got %v", err)
	}
	if !strings.Contains(string(out), `expected OWNER/REPO, got "not-a-repo"`) {
		t.Errorf("output = %q", out)
	}
}
