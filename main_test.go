package main

import (
	"flag"
	"io"
	"reflect"
	"testing"
)

func TestParseArgsAcceptsFlagsAnywhere(t *testing.T) {
	cases := []struct {
		args     []string
		wantRepo string
		wantPos  []string
	}{
		{[]string{"123", "--repo", "o/r"}, "o/r", []string{"123"}},
		{[]string{"--repo", "o/r", "123"}, "o/r", []string{"123"}},
		{[]string{"-R", "o/r"}, "o/r", nil},
		{[]string{"123"}, "", []string{"123"}},
		{[]string{"--", "-5", "--repo"}, "", []string{"-5", "--repo"}},
		{[]string{"1", "--", "--repo", "o/r"}, "", []string{"1", "--repo", "o/r"}},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var repo string
		fs.StringVar(&repo, "repo", "", "")
		fs.StringVar(&repo, "R", "", "")
		got := parseArgs(fs, c.args)
		if repo != c.wantRepo || !reflect.DeepEqual(got, c.wantPos) {
			t.Errorf("parseArgs(%q): repo=%q args=%q, want repo=%q args=%q", c.args, repo, got, c.wantRepo, c.wantPos)
		}
	}
}
