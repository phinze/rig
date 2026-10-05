package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	// The home anchor names this rig's own dir, so a pasted prompt full of
	// foreign absolute paths doesn't quietly pull the agent elsewhere.
	if !strings.Contains(got, dir) {
		t.Errorf("missing basedir in:\n%s", got)
	}
	if !strings.Contains(got, "rig relay") {
		t.Errorf("linear rig should be pointed at relay:\n%s", got)
	}
	// relay refuses anything without a Linear identity, so a rig from another
	// tracker isn't told about it.
	if got := renderRigInstructions(dir, manifest{ID: "rig-9", Tracker: "github", TrackerID: "phinze/rig#9"}); strings.Contains(got, "rig relay") {
		t.Errorf("github rig should not be pointed at relay:\n%s", got)
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
	if !strings.Contains(got, rigKickoffName) {
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

func TestProjectRigInstructionsUseProjectTemplate(t *testing.T) {
	dir := t.TempDir()
	m := manifest{
		ID: "project-byoi", Title: "Bring Your Own Image", Kind: "project",
		Tracker: "linear", TrackerID: "project-uuid", TrackerURL: "https://linear.app/miren/project/byoi",
	}
	got := renderRigInstructions(dir, m)
	if !strings.HasPrefix(got, "# Project rig: Bring Your Own Image\n") {
		t.Errorf("project rig did not get the project template:\n%s", got)
	}
	if !strings.Contains(got, m.TrackerURL) {
		t.Errorf("project instructions lack the tracker URL:\n%s", got)
	}
}

// Every working rig learns the cos address, because a report-back line naming
// this morning's rig id would stop resolving once that rig is torn down. The
// cos rig gets its own instructions instead of a task-rig shape with no repos.
func TestRigInstructionsNameTheCoSAddress(t *testing.T) {
	dir := t.TempDir()
	for name, m := range map[string]manifest{
		"task":    {ID: "mir-75", Tracker: "linear", TrackerID: "MIR-75", Repos: map[string]string{"rig": "phinze/rig"}},
		"loose":   {ID: "local-thing", Repos: map[string]string{"rig": "phinze/rig"}},
		"project": {ID: "project-byoi", Kind: "project", Tracker: "linear"},
	} {
		if got := renderRigInstructions(dir, m); !strings.Contains(got, "rig send cos") {
			t.Errorf("%s rig should be told about rig send cos:\n%s", name, got)
		}
	}
	got := renderRigInstructions(dir, manifest{ID: "cos-2026-10-02", Title: "chief of staff 2026-10-02", Kind: "cos"})
	for _, want := range []string{"# Chief-of-staff rig: chief of staff 2026-10-02", "chief-of-staff skill", "rig send cos"} {
		if !strings.Contains(got, want) {
			t.Errorf("cos instructions missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "rig relay") {
		t.Errorf("cos rig has no project to relay to:\n%s", got)
	}
}

func TestCoSRigIDIsTheWorkday(t *testing.T) {
	day := time.Date(2026, 10, 2, 23, 59, 0, 0, time.Local)
	if got := cosRigID(day); got != "cos-2026-10-02" {
		t.Errorf("cosRigID = %q", got)
	}
}

func TestCoSKickoffAsksAStillRunningPredecessorForAHandover(t *testing.T) {
	if got := cosKickoff("2026-10-03", ""); strings.Contains(got, "handover") {
		t.Errorf("no predecessor, no handover ask: %s", got)
	}
	got := cosKickoff("2026-10-03", "cos-2026-10-02")
	if !strings.Contains(got, "rig send cos-2026-10-02") {
		t.Errorf("the handover ask must use the dated id, since cos now means today: %s", got)
	}
	if !strings.Contains(got, "rig down") {
		t.Errorf("the handover ends with tearing the predecessor down: %s", got)
	}
}
