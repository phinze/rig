package tmux

import (
	"strings"
	"testing"
)

// parsePanes turns one paneFormat line into one Pane, fills Target in tmux's
// session:window.pane shape, and drops a line whose field count is off rather
// than misreading it.
func TestParsePanes(t *testing.T) {
	out := strings.Join([]string{
		strings.Join([]string{"s", "%3", "1", "@2", "0", "main/rig", "agent", "rig", "main", "rig", "claude", "/work/rig", "999990", "✳ Task A"}, "\t"),
		strings.Join([]string{"s", "%4", "0", "@5", "2", "cloud", "", "", "repo", "cloud", "recto", "/work/cloud", "0", "recto — cloud"}, "\t"),
		"s\tbroken line",
	}, "\n")
	panes := parsePanes(out)
	if len(panes) != 2 {
		t.Fatalf("panes = %d (%+v), want 2 with the broken line dropped", len(panes), panes)
	}
	p := panes[0]
	if p.Session != "s" || p.PaneID != "%3" || p.PaneIdx != "1" || p.WindowID != "@2" || p.WindowIdx != "0" || p.WindowName != "main/rig" {
		t.Errorf("identity = %+v", p)
	}
	if p.Target != "s:0.1" {
		t.Errorf("target = %q, want s:0.1", p.Target)
	}
	if p.Role != "agent" || p.Repo != "rig" || p.WindowRole != "main" || p.WindowRepo != "rig" {
		t.Errorf("marks = %+v", p)
	}
	if p.Command != "claude" || p.Path != "/work/rig" || p.Activity != 999990 || p.Title != "✳ Task A" {
		t.Errorf("process = %+v", p)
	}
	if q := panes[1]; q.Role != "" || q.WindowRole != "repo" || q.Activity != 0 || q.Title != "recto — cloud" {
		t.Errorf("unmarked pane = %+v", q)
	}
}
