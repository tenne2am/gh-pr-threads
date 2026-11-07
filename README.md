# gh-pr-threads

A [GitHub CLI](https://cli.github.com) extension that shows the review threads on a
pull request in a terminal UI and lets you reply to them, resolve them, and jump to them
on github.com without leaving the terminal.

The left pane lists the threads (`file.go:42` plus the first line of the opening
comment); the right pane shows the code the thread was opened on (as a coloured diff) followed
by the whole conversation for the selected thread.
Resolved threads are hidden until you press `a`.

## Install

Requires `gh` and an existing `gh auth login`.

```sh
gh extension install OWNER/gh-pr-threads
gh extension upgrade pr-threads   # later, to update
```

## Usage

```
gh pr-threads [<number>] [flags]
```

```sh
gh pr-threads                    # the PR for the current branch
gh pr-threads 123                # a specific PR (`#123` works too)
gh pr-threads 123 --repo cli/cli # a PR in another repository
```

| Flag | Description |
| --- | --- |
| `-R`, `--repo OWNER/REPO` | select a repository (default: the one in the current directory) |

With no number, the PR is found with `gh pr view` for the current branch.

## Diff view

Above each conversation, the detail pane shows the code the thread was left on, so you
can read a comment in context without opening the browser:

```
main.go:14-16  open

@@ -10,6 +10,8 @@ func run() error {
⋮
 	c := 3
 	d := 4
-	old()
+	if x == nil {
+		return nil
+	}

@alice  2d ago
this needs a nil check
```

* Added lines are green, removed lines red, and the `@@` hunk header is highlighted.
* By default only the commented lines and the 3 lines above them are shown, as on
  github.com. `⋮` marks where earlier lines of the hunk were left out.
* Press `d` to toggle between this compact view and the full hunk; the setting stays
  as you move between threads.
* Threads on outdated code show the hunk as it was when the comment was written.
* Long lines are cut at the pane width rather than wrapped, so the code keeps its
  shape; use `o` to see the full line on github.com.

### Quoting the diff in a reply

Press `Q` to start a reply with the code already quoted, or `ctrl+q` while writing a
reply to insert it at the cursor. The quote is exactly what the diff view is showing
(compact or full, following `d`), without the `@@` header, as a fenced block that
github.com renders with diff colours:

````md
```diff
 	c := 3
 	d := 4
-	old()
+	if x == nil {
+		return nil
+	}
```
````

To quote only some of the lines, press `v` to pick them first:

```
@@ -10,6 +10,8 @@ func run() error {
⋮
  -	old()
▌ +	if x == nil {      ← selected
▌ +		return nil     ← selected
  +	}
```

* The selection starts on the commented line(s), as github.com highlights them.
* `j` / `k` move the end of the selection; `space` restarts it at the current line.
* `d` still switches to the full hunk, so you can select lines above the compact view.
* `enter` (or `Q` / `r`) opens the reply with only the selected lines quoted; `esc`
  goes back without replying.

The quote starts on its own line and leaves the cursor below it, so you can keep
typing. If the code itself contains backticks, a longer fence is used. Tabs in the
code become spaces, because the reply box can't hold tab characters.

## Keys

**Browsing**

| Key | Action |
| --- | --- |
| `j` / `k`, `↓` / `↑` | move between threads |
| `g` / `G`, `home` / `end` | first / last thread |
| `J` / `K`, `ctrl+d` / `ctrl+u`, `pgdown` / `pgup` | scroll the conversation half a page |
| `d` | show the full diff hunk / only the commented lines |
| `r` | reply to the selected thread |
| `Q` | reply with the diff quoted |
| `v` | select lines of the diff to quote |
| `t` | resolve / unresolve the selected thread |
| `a` | show or hide resolved threads |
| `o` | open the thread on github.com |
| `R`, `ctrl+r` | reload |
| `q`, `esc`, `ctrl+c` | quit |

**Selecting diff lines** (after `v`)

| Key | Action |
| --- | --- |
| `j` / `k`, `↓` / `↑` | extend the selection down / up |
| `g` / `G`, `home` / `end` | extend to the first / last shown line |
| `space` | restart the selection at the current line |
| `d` | show the full diff hunk / only the commented lines |
| `J` / `K`, `ctrl+d` / `ctrl+u` | scroll the pane |
| `enter`, `Q`, `r` | reply with the selected lines quoted |
| `esc`, `v`, `q` | back to browsing |

**Replying**

| Key | Action |
| --- | --- |
| `ctrl+s`, `alt+enter` | send the reply |
| `ctrl+q` | quote the diff at the cursor |
| `esc` | cancel |

## Develop

Requires Go (see `go.mod` for the version).

```sh
go build -o gh-pr-threads .   # build
go test ./...                 # run the tests
gh extension install .        # install this checkout as `gh pr-threads`
gh pr-threads                 # run it
```

`gh extension install .` symlinks the directory, so after any change just rebuild —
no reinstall needed. `gh extension remove pr-threads` when you're done.

| File | Contents |
| --- | --- |
| [`main.go`](main.go) | flag parsing, repository / PR number resolution |
| [`github.go`](github.go) | GraphQL queries and mutations |
| [`ui.go`](ui.go) | the Bubble Tea model, key handling, and rendering |
| [`ui_test.go`](ui_test.go) | model tests driven by synthetic messages (no network) |
| [`github_test.go`](github_test.go) | GraphQL client tests against a fake HTTP transport |

## How it works

* Auth comes free from `gh`: `api.DefaultGraphQLClient()` in [`github.go`](github.go)
  reuses whatever `gh auth login` stored, including GHES hosts.
* Threads are read with the GraphQL `pullRequest.reviewThreads` connection, paging
  through all of them. The REST endpoint (`/pulls/{n}/comments`) returns a *flat*
  comment list that you would have to re-group by `in_reply_to_id` yourself, and it
  can't tell you `isResolved`.
* The code shown above each conversation is the opening comment's `diffHunk`. It ends at
  the commented line, so by default only the commented range and 3 lines of context
  above it are shown, like on github.com; `d` shows the whole hunk.
* Replies use the `addPullRequestReviewThreadReply` mutation; resolving uses
  `resolveReviewThread` / `unresolveReviewThread`.
* The TUI is [Bubble Tea](https://github.com/charmbracelet/bubbletea) — the same stack
  `gh-dash` uses. All network calls are `tea.Cmd`s so the UI never blocks.

## Release

Tag it; the workflow in [`.github/workflows/release.yml`](.github/workflows/release.yml)
cross-compiles with `cli/gh-extension-precompile` and uploads the binaries that
`gh extension install` and `gh extension upgrade` look for.

```sh
git tag v0.1.0 && git push origin v0.1.0
```
