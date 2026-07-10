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
