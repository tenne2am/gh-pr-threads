package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cli/go-gh/v2/pkg/api"
)

// graphQLRequest is what go-gh posts to /graphql.
type graphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

// fakeGitHub answers every GraphQL request with the next canned response and
// records the requests so tests can inspect what would have been sent.
type fakeGitHub struct {
	t         *testing.T
	responses []string
	requests  []graphQLRequest
}

func (f *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	var req graphQLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Fatalf("decoding request: %v", err)
	}
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		f.t.Fatalf("unexpected request: %s", req.Query)
	}
	body := f.responses[0]
	f.responses = f.responses[1:]
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    r,
	}, nil
}

func fakeClient(t *testing.T, responses ...string) (*Client, *fakeGitHub) {
	t.Helper()
	fake := &fakeGitHub{t: t, responses: responses}
	gql, err := api.NewGraphQLClient(api.ClientOptions{
		Host:      "github.com",
		AuthToken: "test-token",
		Transport: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Client{gql: gql}, fake
}

const threadsJSON = `{"data":{"repository":{"pullRequest":{
  "title": "Some PR",
  "reviewThreads": {
    "pageInfo": {"hasNextPage": false, "endCursor": ""},
    "nodes": [{
      "id": "T1", "isResolved": false, "isOutdated": false,
      "path": "main.go", "line": 12, "startLine": null, "diffSide": "RIGHT",
      "comments": {"nodes": [{
        "id": "C1", "body": "this needs a nil check",
        "createdAt": "2026-09-01T10:00:00Z", "url": "https://example.test/c1",
        "author": {"login": "alice"},
        "diffHunk": "@@ -10,6 +10,8 @@ func run() error {\n \ta := 1\n-\told()\n+\tif x == nil {"
      }]}
    }]
  }
}}}}`

func TestReviewThreadsRequestsAndDecodesDiffHunk(t *testing.T) {
	client, fake := fakeClient(t, threadsJSON)
	_, threads, err := client.ReviewThreads("o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fake.requests[0].Query, "diffHunk") {
		t.Error("the threads query must ask for diffHunk, or the diff view stays empty")
	}
	want := "@@ -10,6 +10,8 @@ func run() error {\n \ta := 1\n-\told()\n+\tif x == nil {"
	if got := threads[0].DiffHunk(); got != want {
		t.Errorf("DiffHunk() = %q, want %q", got, want)
	}
}

func TestReviewThreadsPagesKeepDiffHunks(t *testing.T) {
	page := func(id, hunk string, next bool) string {
		h, _ := json.Marshal(hunk)
		return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"title":"PR","reviewThreads":{
		  "pageInfo":{"hasNextPage":%t,"endCursor":"c1"},
		  "nodes":[{"id":%q,"path":"a.go","line":1,
		    "comments":{"nodes":[{"id":"x","body":"b","diffHunk":%s}]}}]}}}}}`, next, id, h)
	}
	client, fake := fakeClient(t, page("T1", "@@ -1 +1 @@\n+one", true), page("T2", "@@ -2 +2 @@\n+two", false))
	_, threads, err := client.ReviewThreads("o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 2 || fake.requests[1].Variables["cursor"] != "c1" {
		t.Fatalf("expected a second request with cursor c1, got %+v", fake.requests)
	}
	if len(threads) != 2 || threads[1].DiffHunk() != "@@ -2 +2 @@\n+two" {
		t.Errorf("hunks from later pages are lost: %+v", threads)
	}
}

func TestDiffHunkWithoutComments(t *testing.T) {
	if got := (Thread{}).DiffHunk(); got != "" {
		t.Errorf("DiffHunk() = %q, want empty", got)
	}
}

// deliver feeds a command's message back into the model, the way Bubble Tea would.
func deliver(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	n, _ := m.Update(cmd())
	return n.(model)
}

func TestQuotedReplyIsPostedVerbatim(t *testing.T) {
	client, fake := fakeClient(t, threadsJSON,
		`{"data":{"addPullRequestReviewThreadReply":{"comment":{
		   "id":"C2","body":"posted","createdAt":"2026-09-02T10:00:00Z","author":{"login":"me"}}}}}`)

	m := newModel(client, "o", "r", 1)
	n, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = deliver(t, n.(model), m.loadCmd())

	// Select the last two lines of the hunk and reply with them quoted.
	m = key(key(m, "v"), "k")
	m = enter(m)
	m = key(m, "LGTM")
	n, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = deliver(t, n.(model), cmd)

	if len(fake.requests) != 2 {
		t.Fatalf("want a load and a reply request, got %d", len(fake.requests))
	}
	vars := fake.requests[1].Variables
	if vars["threadId"] != "T1" {
		t.Errorf("reply went to thread %v, want T1", vars["threadId"])
	}
	want := "```diff\n-    old()\n+    if x == nil {\n```\n\nLGTM"
	if vars["body"] != want {
		t.Errorf("posted body = %q, want %q", vars["body"], want)
	}
	if !strings.Contains(m.View(), "reply posted") {
		t.Errorf("expected a success message, got:\n%s", m.View())
	}
}
