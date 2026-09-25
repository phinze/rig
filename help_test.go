package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestEveryCommandHasCompleteHelp(t *testing.T) {
	for _, c := range commands {
		if !slices.Contains(commandGroups, c.group) {
			t.Errorf("%s: group %q is not in commandGroups, so the index drops it", c.name, c.group)
		}
		if c.summary == "" || len(c.usage) == 0 {
			t.Errorf("%s: needs a summary and at least one usage line", c.name)
		}
		for _, u := range c.usage {
			if u != c.name && !strings.HasPrefix(u, c.name+" ") {
				t.Errorf("%s: usage line %q doesn't start with the command", c.name, u)
			}
		}
	}
}

func TestWantsHelp(t *testing.T) {
	relay, _ := findCommand("relay")
	recto, _ := findCommand("recto")
	for _, tc := range []struct {
		c    command
		args []string
		want bool
	}{
		{relay, []string{"--help"}, true},
		{relay, []string{"-h"}, true},
		{relay, []string{"found", "a", "thing", "--help"}, true},
		{relay, []string{"found", "a", "thing"}, false},
		{relay, []string{"--", "--help"}, false},    // -- makes it data
		{relay, []string{"help"}, false},            // a bare word is data
		{recto, []string{"--help"}, true},           // first arg is rig's
		{recto, []string{"cloud", "--help"}, false}, // the rest is Recto's
	} {
		if got := wantsHelp(tc.c, tc.args); got != tc.want {
			t.Errorf("wantsHelp(%s, %q) = %v, want %v", tc.c.name, tc.args, got, tc.want)
		}
	}
}

func TestIndexListsEveryCommandOnOneScreen(t *testing.T) {
	var buf bytes.Buffer
	printIndex(&buf)
	out := buf.String()
	for _, name := range commandNames {
		if !strings.Contains(out, "  "+name+" ") {
			t.Errorf("index is missing %s", name)
		}
	}
	if n := strings.Count(out, "\n"); n > 50 {
		t.Errorf("index is %d lines; keep it to one screen", n)
	}
}

func TestHelpTopicResolvesLikeACommand(t *testing.T) {
	var buf bytes.Buffer
	if err := runHelp([]string{"swe"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "rig sweep:") {
		t.Errorf("rig help swe printed:\n%s", buf.String())
	}
	if err := runHelp([]string{"sweeep"}, &buf); err == nil || !strings.Contains(err.Error(), "did you mean") {
		t.Errorf("rig help sweeep: want a suggestion, got %v", err)
	}
}
