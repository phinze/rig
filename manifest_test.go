package main

import (
	"strings"
	"testing"
)

// A manifest with recorded branches must round-trip through write/read, and the
// branches table must be omitted entirely when no branch was captured (so older
// single-repo rigs don't grow an empty [branches] header).
func TestManifestBranchesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := manifest{
		ID:       "mir-75",
		Title:    "add zig stack",
		Repos:    map[string]string{"rig": "phinze/rig", "recto": "phinze/recto"},
		Branches: map[string]string{"rig": "phinze/mir-75-add-zig-stack"},
	}
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}

	got, err := readManifest(dir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if got.Branches["rig"] != "phinze/mir-75-add-zig-stack" {
		t.Errorf("branch not round-tripped: got %q", got.Branches["rig"])
	}
	// recto had no recorded branch; it should read back absent, not empty-string
	// noise that would shadow the heuristic fallback in an unexpected way.
	if b, ok := got.Branches["recto"]; ok {
		t.Errorf("expected no recorded branch for recto, got %q", b)
	}
}

func TestManifestNoBranchesTable(t *testing.T) {
	dir := t.TempDir()
	m := manifest{ID: "pr-9", Repos: map[string]string{"rig": "phinze/rig"}}
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	raw := string(mustReadFile(t, dir+"/"+manifestName))
	if strings.Contains(raw, "[branches]") {
		t.Errorf("expected no [branches] table when none recorded:\n%s", raw)
	}
}

// addRepoToManifest should record the branch when given one and leave it out
// when handed the empty string (the added-repo / trunk case).
func TestAddRepoToManifestBranch(t *testing.T) {
	dir := t.TempDir()
	if err := writeManifest(dir, manifest{ID: "mir-1", Repos: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if err := addRepoToManifest(dir, "rig", "phinze/rig", "phinze/mir-1-feature"); err != nil {
		t.Fatal(err)
	}
	if err := addRepoToManifest(dir, "extra", "phinze/extra", ""); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Branches["rig"] != "phinze/mir-1-feature" {
		t.Errorf("rig branch = %q, want phinze/mir-1-feature", m.Branches["rig"])
	}
	if _, ok := m.Branches["extra"]; ok {
		t.Error("added repo with empty branch should record no branch")
	}
}
