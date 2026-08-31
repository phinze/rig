package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteRigClaudeMD(t *testing.T) {
	dir := t.TempDir()
	m := manifest{
		ID:    "MIR-75",
		Title: "Fix the widget reaper",
		Repos: map[string]string{
			"rig":   "phinze/rig",
			"recto": "phinze/recto",
		},
	}
	if err := writeRigClaudeMD(dir, m); err != nil {
		t.Fatalf("writeRigClaudeMD: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("reading generated CLAUDE.md: %v", err)
	}
	got := string(raw)

	// The heading should carry the live task identity so the agent knows which
	// rig it's sitting in.
	if !strings.Contains(got, "# Rig MIR-75: Fix the widget reaper") {
		t.Errorf("missing task heading in:\n%s", got)
	}
	// Both repos, rendered as owner/repo with their subdir, so the agent knows
	// its siblings.
	for _, want := range []string{"`phinze/rig` (./rig)", "`phinze/recto` (./recto)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing repo line %q in:\n%s", want, got)
		}
	}
	// The whole point: steer agents off git and onto jj.
	if !strings.Contains(got, "jj workspaces") {
		t.Errorf("expected jj-not-git guidance in:\n%s", got)
	}
	// The home anchor: name this rig's dir and set the stay-inside-the-rig
	// default, so a pasted prompt full of foreign absolute paths doesn't quietly
	// pull the agent into another rig's workspace.
	if !strings.Contains(got, dir) || !strings.Contains(got, "This rig is your home") {
		t.Errorf("missing home anchor in:\n%s", got)
	}
	if !strings.Contains(got, "rig relay <discovery>") || !strings.Contains(got, "does not post to Linear") {
		t.Errorf("missing project-discovery relay guidance in:\n%s", got)
	}
}

// A title-less rig (e.g. a GH issue with no resolved title yet) should still
// render a clean heading rather than a dangling separator.
func TestWriteRigClaudeMD_NoTitle(t *testing.T) {
	dir := t.TempDir()
	m := manifest{ID: "MIR-9", Repos: map[string]string{"rig": "phinze/rig"}}
	if err := writeRigClaudeMD(dir, m); err != nil {
		t.Fatalf("writeRigClaudeMD: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("reading generated CLAUDE.md: %v", err)
	}
	if got := string(raw); !strings.Contains(got, "# Rig MIR-9\n") {
		t.Errorf("expected bare id heading, got:\n%s", got)
	}
}

// The pasted brief only earns a bullet in the auto-loaded instructions when it
// actually exists — most rigs are kickoff-line-only, and a pointer to a missing
// file is worse than no pointer at all. Regenerating on `rig add` is what makes
// the bullet stick, so this goes through the same render path.
func TestRigInstructionsPointAtKickoff(t *testing.T) {
	dir := t.TempDir()
	m := manifest{ID: "local-thing", Title: "Investigate flaky radar refresh", Repos: map[string]string{"rig": "phinze/rig"}}

	if got := renderRigInstructions(dir, m); strings.Contains(got, rigKickoffName) {
		t.Errorf("instructions advertise a kickoff file that isn't there:\n%s", got)
	}

	if err := writeRigKickoff(dir, m.Title, "  jim: radar hangs on enter\nme: after a sweep?  "); err != nil {
		t.Fatalf("writeRigKickoff: %v", err)
	}
	raw := string(mustReadFile(t, filepath.Join(dir, rigKickoffName)))
	want := "# Kickoff: Investigate flaky radar refresh\n\njim: radar hangs on enter\nme: after a sweep?\n"
	if raw != want {
		t.Errorf("kickoff file =\n%q\nwant\n%q", raw, want)
	}

	got := renderRigInstructions(dir, m)
	if !strings.Contains(got, "The brief lives in `"+rigKickoffName+"`") || !strings.Contains(got, "../"+rigKickoffName) {
		t.Errorf("instructions missing the kickoff pointer:\n%s", got)
	}
}

func TestWriteRigAgentInstructions(t *testing.T) {
	dir := t.TempDir()
	m := manifest{ID: "MIR-10", Repos: map[string]string{"rig": "phinze/rig"}}
	if err := writeRigAgentInstructions(dir, m); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md", filepath.Join(".agents", "rules", "rig.md")} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("reading %s: %v", name, err)
			continue
		}
		if !strings.Contains(string(raw), "# Rig MIR-10") {
			t.Errorf("%s lacks rig context", name)
		}
	}
}

func TestProjectRigInstructionsDescribeControlPlane(t *testing.T) {
	dir := t.TempDir()
	m := manifest{
		ID: "project-byoi", Title: "Bring Your Own Image", Kind: "project",
		Tracker: "linear", TrackerID: "project-uuid", TrackerURL: "https://linear.app/miren/project/byoi",
	}
	got := renderRigInstructions(dir, m)
	for _, want := range []string{"project overview rig", "does not own a code checkout", "rig project status --format=json", m.TrackerURL, "Ordinary sweep will not collect it"} {
		if !strings.Contains(got, want) {
			t.Errorf("project instructions lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "jj workspaces") || strings.Contains(got, "main tmux window holds") {
		t.Errorf("project instructions inherited task-workspace guidance:\n%s", got)
	}
}
