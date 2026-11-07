package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// Client wraps the authenticated GraphQL and REST clients that go-gh builds
// from whatever credentials `gh auth` already stored, so the extension never
// deals with tokens itself. REST is only needed for re-running Actions jobs,
// which GraphQL can't do.
type Client struct {
	gql  *api.GraphQLClient
	rest *api.RESTClient
}

func NewClient() (*Client, error) {
	gql, err := api.DefaultGraphQLClient()
	if err != nil {
		return nil, fmt.Errorf("creating GraphQL client: %w", err)
	}
	rest, err := api.DefaultRESTClient()
	if err != nil {
		return nil, fmt.Errorf("creating REST client: %w", err)
	}
	return &Client{gql: gql, rest: rest}, nil
}

type Author struct {
	Login string `json:"login"`
}

type Comment struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	URL       string `json:"url"`
	Author    Author `json:"author"`
	// DiffHunk is the slice of the diff the comment was left on, ending at
	// the commented line.
	DiffHunk string `json:"diffHunk"`
}

type Thread struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	Line       *int   `json:"line"`
	StartLine  *int   `json:"startLine"`
	DiffSide   string `json:"diffSide"`
	Comments   struct {
		Nodes []Comment `json:"nodes"`
	} `json:"comments"`
}

// Location renders the "file.go:42" label for the thread list.
func (t Thread) Location() string {
	if t.Path == "" {
		return "(no file)"
	}
	if t.Line == nil {
		return t.Path + " (outdated)"
	}
	if t.StartLine != nil && *t.StartLine != *t.Line {
		return fmt.Sprintf("%s:%d-%d", t.Path, *t.StartLine, *t.Line)
	}
	return fmt.Sprintf("%s:%d", t.Path, *t.Line)
}

// Preview is the first line of the opening comment, for the list pane.
func (t Thread) Preview() string {
	if len(t.Comments.Nodes) == 0 {
		return ""
	}
	body := strings.TrimSpace(t.Comments.Nodes[0].Body)
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[:i]
	}
	return body
}

// DiffHunk is the code the thread was opened on. Replies carry the same hunk,
// so the opening comment's is enough.
func (t Thread) DiffHunk() string {
	if len(t.Comments.Nodes) == 0 {
		return ""
	}
	return t.Comments.Nodes[0].DiffHunk
}

func (t Thread) URL() string {
	if len(t.Comments.Nodes) == 0 {
		return ""
	}
	return t.Comments.Nodes[0].URL
}

const threadsQuery = `
query PRReviewThreads($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      title
      reviewThreads(first: 50, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          startLine
          diffSide
          comments(first: 100) {
            nodes {
              id
              body
              createdAt
              url
              author { login }
              diffHunk
            }
          }
        }
      }
    }
  }
}`

type threadsResponse struct {
	Repository struct {
		PullRequest struct {
			Title         string `json:"title"`
			ReviewThreads struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []Thread `json:"nodes"`
			} `json:"reviewThreads"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

// ReviewThreads pages through every review thread on the pull request.
func (c *Client) ReviewThreads(owner, repo string, number int) (string, []Thread, error) {
	vars := map[string]interface{}{
		"owner":  owner,
		"repo":   repo,
		"number": number,
		"cursor": (*string)(nil),
	}

	var title string
	var all []Thread
	for {
		var resp threadsResponse
		if err := c.gql.Do(threadsQuery, vars, &resp); err != nil {
			return "", nil, fmt.Errorf("fetching review threads: %w", err)
		}
		pr := resp.Repository.PullRequest
		title = pr.Title
		all = append(all, pr.ReviewThreads.Nodes...)
		if !pr.ReviewThreads.PageInfo.HasNextPage {
			break
		}
		cursor := pr.ReviewThreads.PageInfo.EndCursor
		vars["cursor"] = &cursor
	}
	return title, all, nil
}

const replyMutation = `
mutation Reply($threadId: ID!, $body: String!) {
  addPullRequestReviewThreadReply(input: {pullRequestReviewThreadId: $threadId, body: $body}) {
    comment {
      id
      body
      createdAt
      url
      author { login }
    }
  }
}`

// Reply posts a reply into an existing review thread and returns the new comment.
func (c *Client) Reply(threadID, body string) (Comment, error) {
	var resp struct {
		AddPullRequestReviewThreadReply struct {
			Comment Comment `json:"comment"`
		} `json:"addPullRequestReviewThreadReply"`
	}
	vars := map[string]interface{}{"threadId": threadID, "body": body}
	if err := c.gql.Do(replyMutation, vars, &resp); err != nil {
		return Comment{}, fmt.Errorf("posting reply: %w", err)
	}
	return resp.AddPullRequestReviewThreadReply.Comment, nil
}

const resolveMutation = `
mutation Resolve($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}`

const unresolveMutation = `
mutation Unresolve($threadId: ID!) {
  unresolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}`

// SetResolved resolves or unresolves a review thread.
func (c *Client) SetResolved(threadID string, resolved bool) error {
	mutation := unresolveMutation
	if resolved {
		mutation = resolveMutation
	}
	var resp struct{}
	vars := map[string]interface{}{"threadId": threadID}
	if err := c.gql.Do(mutation, vars, &resp); err != nil {
		return fmt.Errorf("updating thread: %w", err)
	}
	return nil
}

// Check is one CI result on the pull request's head commit: a GitHub Actions
// job (or another app's check run), or a commit status from an external CI.
type Check struct {
	Name     string // job name, or the status context
	Workflow string // Actions workflow name; empty for non-Actions checks
	Event    string // what triggered the workflow run, e.g. "pull_request"
	State    CheckState
	// Detail is GitHub's own word for the state, e.g. "cancelled" or
	// "in progress", or a commit status's description.
	Detail      string
	URL         string
	StartedAt   string
	CompletedAt string

	// Set only for GitHub Actions jobs; they are what Rerun needs.
	JobID     int64
	RunID     int64
	RunNumber int
}

// CheckState buckets the many check conclusions the way `gh pr checks` does.
type CheckState int

const (
	CheckFail CheckState = iota
	CheckPending
	CheckPass
	CheckSkipped
)

// Label is "Workflow / job" for Actions jobs and just the name otherwise.
func (c Check) Label() string {
	if c.Workflow == "" {
		return c.Name
	}
	return c.Workflow + " / " + c.Name
}

const checksQuery = `
query PRChecks($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      commits(last: 1) {
        nodes {
          commit {
            oid
            statusCheckRollup {
              contexts(first: 100, after: $cursor) {
                pageInfo { hasNextPage endCursor }
                nodes {
                  __typename
                  ... on CheckRun {
                    databaseId
                    name
                    status
                    conclusion
                    detailsUrl
                    startedAt
                    completedAt
                    checkSuite { workflowRun { databaseId runNumber event workflow { name } } }
                  }
                  ... on StatusContext {
                    context
                    state
                    targetUrl
                    description
                    createdAt
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`

// checkContext holds either member of the CheckRun | StatusContext union.
type checkContext struct {
	Typename string `json:"__typename"`

	// CheckRun
	DatabaseID  int64  `json:"databaseId"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	DetailsURL  string `json:"detailsUrl"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt"`
	CheckSuite  struct {
		WorkflowRun *struct {
			DatabaseID int64  `json:"databaseId"`
			RunNumber  int    `json:"runNumber"`
			Event      string `json:"event"`
			Workflow   struct {
				Name string `json:"name"`
			} `json:"workflow"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`

	// StatusContext
	Context     string `json:"context"`
	State       string `json:"state"`
	TargetURL   string `json:"targetUrl"`
	Description string `json:"description"`
	CreatedAt   string `json:"createdAt"`
}

type checksResponse struct {
	Repository struct {
		PullRequest struct {
			Commits struct {
				Nodes []struct {
					Commit struct {
						Oid               string `json:"oid"`
						StatusCheckRollup *struct {
							Contexts struct {
								PageInfo struct {
									HasNextPage bool   `json:"hasNextPage"`
									EndCursor   string `json:"endCursor"`
								} `json:"pageInfo"`
								Nodes []checkContext `json:"nodes"`
							} `json:"contexts"`
						} `json:"statusCheckRollup"`
					} `json:"commit"`
				} `json:"nodes"`
			} `json:"commits"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

// Checks pages through the CI results on the pull request's head commit and
// returns that commit's SHA with them. A commit nothing has run on yet has no
// status rollup, which is an empty list rather than an error.
func (c *Client) Checks(owner, repo string, number int) (string, []Check, error) {
	vars := map[string]interface{}{
		"owner":  owner,
		"repo":   repo,
		"number": number,
		"cursor": (*string)(nil),
	}

	var sha string
	var all []Check
	for {
		var resp checksResponse
		if err := c.gql.Do(checksQuery, vars, &resp); err != nil {
			return "", nil, fmt.Errorf("fetching checks: %w", err)
		}
		commits := resp.Repository.PullRequest.Commits.Nodes
		if len(commits) == 0 {
			return "", nil, nil
		}
		sha = commits[0].Commit.Oid
		rollup := commits[0].Commit.StatusCheckRollup
		if rollup == nil {
			return sha, nil, nil
		}
		for _, ctx := range rollup.Contexts.Nodes {
			all = append(all, ctx.toCheck())
		}
		if !rollup.Contexts.PageInfo.HasNextPage {
			break
		}
		cursor := rollup.Contexts.PageInfo.EndCursor
		vars["cursor"] = &cursor
	}
	return sha, all, nil
}

func (ctx checkContext) toCheck() Check {
	if ctx.Typename == "StatusContext" {
		c := Check{
			Name:      ctx.Context,
			Detail:    ctx.Description,
			URL:       ctx.TargetURL,
			StartedAt: ctx.CreatedAt,
		}
		switch ctx.State {
		case "SUCCESS":
			c.State = CheckPass
		case "PENDING", "EXPECTED":
			c.State = CheckPending
		default: // FAILURE, ERROR
			c.State = CheckFail
		}
		if c.Detail == "" {
			c.Detail = strings.ToLower(ctx.State)
		}
		return c
	}

	c := Check{
		Name:        ctx.Name,
		URL:         ctx.DetailsURL,
		StartedAt:   ctx.StartedAt,
		CompletedAt: ctx.CompletedAt,
	}
	if run := ctx.CheckSuite.WorkflowRun; run != nil {
		c.Workflow = run.Workflow.Name
		c.Event = run.Event
		c.JobID = ctx.DatabaseID
		c.RunID = run.DatabaseID
		c.RunNumber = run.RunNumber
	}
	if ctx.Status != "COMPLETED" {
		c.State = CheckPending
		c.Detail = strings.ToLower(strings.ReplaceAll(ctx.Status, "_", " "))
		return c
	}
	switch ctx.Conclusion {
	case "SUCCESS":
		c.State = CheckPass
	case "SKIPPED", "NEUTRAL":
		c.State = CheckSkipped
	default: // FAILURE, CANCELLED, TIMED_OUT, ACTION_REQUIRED, STARTUP_FAILURE, STALE
		c.State = CheckFail
	}
	c.Detail = strings.ToLower(strings.ReplaceAll(ctx.Conclusion, "_", " "))
	return c
}

// RerunScope is what to re-run, matching the buttons on a workflow run's page.
type RerunScope int

const (
	RerunJob    RerunScope = iota // just the selected job
	RerunFailed                   // the failed jobs of its run
	RerunAll                      // every job of its run
)

// Rerun asks GitHub Actions to re-run the check's job or workflow run.
func (c *Client) Rerun(owner, repo string, check Check, scope RerunScope) error {
	var path string
	switch scope {
	case RerunJob:
		path = fmt.Sprintf("repos/%s/%s/actions/jobs/%d/rerun", owner, repo, check.JobID)
	case RerunFailed:
		path = fmt.Sprintf("repos/%s/%s/actions/runs/%d/rerun-failed-jobs", owner, repo, check.RunID)
	default:
		path = fmt.Sprintf("repos/%s/%s/actions/runs/%d/rerun", owner, repo, check.RunID)
	}
	// Request rather than Post: these endpoints answer 201 with no useful
	// body, which Post would try to decode as JSON.
	resp, err := c.rest.Request(http.MethodPost, path, nil)
	if err != nil {
		return fmt.Errorf("re-running: %w", err)
	}
	resp.Body.Close()
	return nil
}
