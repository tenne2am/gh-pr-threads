package main

import (
	"fmt"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// Client wraps the authenticated GraphQL client that go-gh builds from
// whatever credentials `gh auth` already stored, so the extension never
// deals with tokens itself.
type Client struct {
	gql *api.GraphQLClient
}

func NewClient() (*Client, error) {
	gql, err := api.DefaultGraphQLClient()
	if err != nil {
		return nil, fmt.Errorf("creating GraphQL client: %w", err)
	}
	return &Client{gql: gql}, nil
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
