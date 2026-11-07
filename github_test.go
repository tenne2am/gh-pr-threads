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
// records the requests so tests can inspect what would have been sent. REST
// calls are recorded as "METHOD /path" and answered with restStatus/restBody.
type fakeGitHub struct {
	t         *testing.T
	responses []string
	requests  []graphQLRequest

	rest       []string
	restStatus int
	restBody   string
}

func (f *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path != "/graphql" {
		f.rest = append(f.rest, r.Method+" "+r.URL.Path)
		return &http.Response{
			StatusCode: f.restStatus,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(f.restBody)),
			Request:    r,
		}, nil
	}
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
	fake := &fakeGitHub{t: t, responses: responses, restStatus: http.StatusCreated}
	opts := api.ClientOptions{
		Host:      "github.com",
		AuthToken: "test-token",
		Transport: fake,
	}
	gql, err := api.NewGraphQLClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := api.NewRESTClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{gql: gql, rest: rest}, fake
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

// checksPage builds one page of the PRChecks response around raw context nodes.
func checksPage(nodes string, next bool) string {
	return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"commits":{"nodes":[{"commit":{
	  "oid":"abc1234def",
	  "statusCheckRollup":{"contexts":{
	    "pageInfo":{"hasNextPage":%t,"endCursor":"p1"},
	    "nodes":[%s]}}}}]}}}}}`, next, nodes)
}

func TestChecksNormalizesRunsAndStatuses(t *testing.T) {
	nodes := `
	  {"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS",
	   "detailsUrl":"https://example.test/run/1","startedAt":"2026-09-01T10:00:00Z","completedAt":"2026-09-01T10:01:30Z",
	   "checkSuite":{"workflowRun":{"event":"pull_request","workflow":{"name":"CI"}}}},
	  {"__typename":"CheckRun","name":"test","status":"IN_PROGRESS","conclusion":null,
	   "checkSuite":{"workflowRun":{"event":"push","workflow":{"name":"CI"}}}},
	  {"__typename":"CheckRun","name":"deploy","status":"COMPLETED","conclusion":"TIMED_OUT",
	   "checkSuite":{"workflowRun":{"event":"pull_request","workflow":{"name":"CD"}}}},
	  {"__typename":"CheckRun","name":"label","status":"COMPLETED","conclusion":"SKIPPED","checkSuite":{"workflowRun":null}},
	  {"__typename":"StatusContext","context":"ci/jenkins","state":"ERROR","targetUrl":"https://jenkins.test/1",
	   "description":"Build broke","createdAt":"2026-09-01T10:00:00Z"},
	  {"__typename":"StatusContext","context":"ci/other","state":"EXPECTED"}`
	client, fake := fakeClient(t, checksPage(nodes, false))
	sha, checks, err := client.Checks("o", "r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if vars := fake.requests[0].Variables; vars["owner"] != "o" || vars["repo"] != "r" || vars["number"] != float64(7) {
		t.Errorf("checks query for the wrong PR: %v", vars)
	}
	if sha != "abc1234def" {
		t.Errorf("sha = %q", sha)
	}
	want := []Check{
		{Name: "build", Workflow: "CI", Event: "pull_request", State: CheckPass, Detail: "success",
			URL: "https://example.test/run/1", StartedAt: "2026-09-01T10:00:00Z", CompletedAt: "2026-09-01T10:01:30Z"},
		{Name: "test", Workflow: "CI", Event: "push", State: CheckPending, Detail: "in progress"},
		{Name: "deploy", Workflow: "CD", Event: "pull_request", State: CheckFail, Detail: "timed out"},
		{Name: "label", State: CheckSkipped, Detail: "skipped"},
		{Name: "ci/jenkins", State: CheckFail, Detail: "Build broke", URL: "https://jenkins.test/1", StartedAt: "2026-09-01T10:00:00Z"},
		{Name: "ci/other", State: CheckPending, Detail: "expected"},
	}
	if len(checks) != len(want) {
		t.Fatalf("got %d checks, want %d: %+v", len(checks), len(want), checks)
	}
	for i := range want {
		if checks[i] != want[i] {
			t.Errorf("check %d =\n  %+v\nwant\n  %+v", i, checks[i], want[i])
		}
	}
}

func TestChecksPages(t *testing.T) {
	run := func(name string) string {
		return `{"__typename":"CheckRun","name":"` + name + `","status":"COMPLETED","conclusion":"SUCCESS"}`
	}
	client, fake := fakeClient(t, checksPage(run("one"), true), checksPage(run("two"), false))
	_, checks, err := client.Checks("o", "r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 2 || fake.requests[1].Variables["cursor"] != "p1" {
		t.Fatalf("expected a second request with cursor p1, got %+v", fake.requests)
	}
	if len(checks) != 2 || checks[1].Name != "two" {
		t.Errorf("checks from later pages are lost: %+v", checks)
	}
}

func TestChecksWithoutRollupIsEmpty(t *testing.T) {
	client, _ := fakeClient(t, `{"data":{"repository":{"pullRequest":{"commits":{"nodes":[
	  {"commit":{"oid":"abc","statusCheckRollup":null}}]}}}}}`)
	sha, checks, err := client.Checks("o", "r", 7)
	if err != nil || sha != "abc" || len(checks) != 0 {
		t.Errorf("Checks = %q, %+v, %v; want the sha and no checks", sha, checks, err)
	}
}

func TestChecksError(t *testing.T) {
	client, _ := fakeClient(t, `{"errors":[{"message":"Resource not accessible by integration"}]}`)
	if _, _, err := client.Checks("o", "r", 7); err == nil || !strings.Contains(err.Error(), "fetching checks") {
		t.Errorf("err = %v, want a wrapped GraphQL error", err)
	}
}

func TestCheckLabel(t *testing.T) {
	if got := (Check{Workflow: "CI", Name: "build"}).Label(); got != "CI / build" {
		t.Errorf("Label() = %q", got)
	}
	if got := (Check{Name: "ci/jenkins"}).Label(); got != "ci/jenkins" {
		t.Errorf("Label() = %q", got)
	}
}

func TestChecksCarryActionsIDs(t *testing.T) {
	client, _ := fakeClient(t, checksPage(`
	  {"__typename":"CheckRun","databaseId":2002,"name":"test","status":"COMPLETED","conclusion":"FAILURE",
	   "checkSuite":{"workflowRun":{"databaseId":42,"runNumber":7,"event":"pull_request","workflow":{"name":"CI"}}}},
	  {"__typename":"CheckRun","databaseId":3003,"name":"external","status":"COMPLETED","conclusion":"SUCCESS",
	   "checkSuite":{"workflowRun":null}}`, false))
	_, checks, err := client.Checks("o", "r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if c := checks[0]; c.JobID != 2002 || c.RunID != 42 || c.RunNumber != 7 {
		t.Errorf("Actions job IDs = job %d run %d #%d, want job 2002 run 42 #7", c.JobID, c.RunID, c.RunNumber)
	}
	// A check run from another app has no workflow run to re-run.
	if c := checks[1]; c.JobID != 0 || c.RunID != 0 {
		t.Errorf("non-Actions check got IDs job %d run %d; Rerun must not be offered", c.JobID, c.RunID)
	}
}

func TestRerunEndpoints(t *testing.T) {
	check := Check{JobID: 2002, RunID: 42}
	cases := []struct {
		scope RerunScope
		want  string
	}{
		{RerunJob, "POST /repos/o/r/actions/jobs/2002/rerun"},
		{RerunFailed, "POST /repos/o/r/actions/runs/42/rerun-failed-jobs"},
		{RerunAll, "POST /repos/o/r/actions/runs/42/rerun"},
	}
	for _, c := range cases {
		client, fake := fakeClient(t)
		// GitHub answers 201 with an empty body; that must count as success.
		if err := client.Rerun("o", "r", check, c.scope); err != nil {
			t.Errorf("scope %d: %v", c.scope, err)
		}
		if len(fake.rest) != 1 || fake.rest[0] != c.want {
			t.Errorf("scope %d: requests = %q, want [%q]", c.scope, fake.rest, c.want)
		}
	}
}

func TestRerunError(t *testing.T) {
	client, fake := fakeClient(t)
	fake.restStatus = http.StatusForbidden
	fake.restBody = `{"message":"Resource not accessible by integration"}`
	err := client.Rerun("o", "r", Check{JobID: 1, RunID: 2}, RerunJob)
	if err == nil || !strings.Contains(err.Error(), "re-running") || !strings.Contains(err.Error(), "Resource not accessible by integration") {
		t.Errorf("err = %v, want GitHub's message wrapped", err)
	}
}
