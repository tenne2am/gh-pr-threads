package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cli/go-gh/v2"
	"github.com/cli/go-gh/v2/pkg/repository"
)

func main() {
	var repoFlag string
	flag.StringVar(&repoFlag, "repo", "", "select a repository as OWNER/REPO")
	flag.StringVar(&repoFlag, "R", "", "select a repository as OWNER/REPO (shorthand)")
	flag.Usage = usage

	if err := run(repoFlag, parseArgs(flag.CommandLine, os.Args[1:])); err != nil {
		fmt.Fprintf(os.Stderr, "gh pr-threads: %v\n", err)
		os.Exit(1)
	}
}

// parseArgs parses flags wherever they appear, so `gh pr-threads 123 --repo
// o/r` works like gh's own commands; the flag package alone stops at the first
// positional argument. Everything after "--" is positional.
func parseArgs(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			return positional
		}
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...)
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Browse and reply to pull request review threads in a TUI.

USAGE
  gh pr-threads [<number>] [flags]

FLAGS
  -R, --repo OWNER/REPO   select a repository (default: the current one)

With no argument, the pull request for the current branch is used.
`)
}

func run(repoFlag string, args []string) error {
	owner, repo, err := resolveRepo(repoFlag)
	if err != nil {
		return err
	}

	number, err := resolveNumber(repoFlag, args)
	if err != nil {
		return err
	}

	client, err := NewClient()
	if err != nil {
		return err
	}

	p := tea.NewProgram(
		newModel(client, owner, repo, number),
		tea.WithAltScreen(),
	)
	final, err := p.Run()
	if err != nil {
		return err
	}
	if m, ok := final.(model); ok && m.err != nil {
		return m.err
	}
	return nil
}

func resolveRepo(repoFlag string) (string, string, error) {
	if repoFlag != "" {
		owner, repo, ok := strings.Cut(repoFlag, "/")
		if !ok || owner == "" || repo == "" {
			return "", "", fmt.Errorf("expected OWNER/REPO, got %q", repoFlag)
		}
		return owner, repo, nil
	}
	current, err := repository.Current()
	if err != nil {
		return "", "", fmt.Errorf("no repository detected here; run inside a repository or pass --repo OWNER/REPO")
	}
	return current.Owner, current.Name, nil
}

// resolveNumber takes the PR number from the arguments, or asks gh which
// pull request belongs to the current branch.
func resolveNumber(repoFlag string, args []string) (int, error) {
	if len(args) > 0 {
		n, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
		if err != nil {
			return 0, fmt.Errorf("invalid pull request number %q", args[0])
		}
		return n, nil
	}

	ghArgs := []string{"pr", "view", "--json", "number"}
	if repoFlag != "" {
		ghArgs = append(ghArgs, "--repo", repoFlag)
	}
	stdout, stderr, err := gh.Exec(ghArgs...)
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return 0, fmt.Errorf("no pull request found for the current branch: %s", msg)
	}
	var out struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return 0, fmt.Errorf("parsing gh pr view output: %w", err)
	}
	return out.Number, nil
}
