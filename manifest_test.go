package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A manifest with recorded branches must round-trip through write/read, and the
// branches table must be omitted entirely when no branch was captured (so older
// single-repo rigs don't grow an empty [branches] header).
func TestManifestBranchesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := manifest{
		ID:    "mir-75",
		Title: "add zig stack",
		Repos: map[string]string{"rig": "phinze/rig", "recto": "phinze/recto"},
		// rig carries a primary plus a secondary (the bugfix-while-in-here case);
		// both must survive the round-trip in order.
		Branches: map[string][]string{"rig": {"phinze/mir-75-add-zig-stack", "phinze/mir-75-bugfix"}},
	}
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}

	got, err := readManifest(dir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	want := []string{"phinze/mir-75-add-zig-stack", "phinze/mir-75-bugfix"}
	if !slices.Equal(got.Branches["rig"], want) {
		t.Errorf("branches not round-tripped: got %v, want %v", got.Branches["rig"], want)
	}
	// recto had no recorded branch; it should read back absent, not empty-slice
	// noise that would shadow the heuristic fallback in an unexpected way.
	if b, ok := got.Branches["recto"]; ok {
		t.Errorf("expected no recorded branch for recto, got %v", b)
	}
}

// A legacy manifest that wrote branches as a bare scalar must still read back —
// as a one-element list — so rigs created before N-per-repo keep resolving.
func TestManifestLegacyScalarBranch(t *testing.T) {
	dir := t.TempDir()
	raw := "id = \"mir-9\"\n\n[repos]\nrig = \"phinze/rig\"\n\n[branches]\nrig = \"phinze/legacy\"\n"
	if err := os.WriteFile(filepath.Join(dir, manifestName), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(dir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if !slices.Equal(m.Branches["rig"], []string{"phinze/legacy"}) {
		t.Errorf("legacy scalar branch = %v, want [phinze/legacy]", m.Branches["rig"])
	}
}

// The parked timestamp must round-trip, and an unparked rig must write no
// parked key at all (so switch/ls read it as in-flight).
func TestManifestParkedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	parked := time.Now().UTC().Truncate(time.Second)
	m := manifest{ID: "mir-5", Repos: map[string]string{"rig": "phinze/rig"}, Parked: parked}
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	got, err := readManifest(dir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if !got.Parked.Equal(parked) {
		t.Errorf("parked = %v, want %v", got.Parked, parked)
	}

	// An unparked rig leaves no parked key behind.
	dir2 := t.TempDir()
	if err := writeManifest(dir2, manifest{ID: "mir-6", Repos: map[string]string{"rig": "phinze/rig"}}); err != nil {
		t.Fatal(err)
	}
	raw := string(mustReadFile(t, dir2+"/"+manifestName))
	if strings.Contains(raw, "parked") {
		t.Errorf("expected no parked key for an in-flight rig:\n%s", raw)
	}
	got2, err := readManifest(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if !got2.Parked.IsZero() {
		t.Errorf("in-flight rig should read Parked zero, got %v", got2.Parked)
	}
}

// A review rig's kind must round-trip, and an authoring rig must write no kind
// key at all (so it reads back as the safe authoring default, isReview()==false).
func TestManifestKindRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := manifest{ID: "pr-882", Kind: "review", Repos: map[string]string{"runtime": "o/runtime"}}
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	got, err := readManifest(dir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if !got.isReview() {
		t.Errorf("kind = %q, want a review rig", got.Kind)
	}

	// An authoring rig leaves no kind key behind and reads as not-a-review.
	dir2 := t.TempDir()
	if err := writeManifest(dir2, manifest{ID: "mir-6", Repos: map[string]string{"rig": "phinze/rig"}}); err != nil {
		t.Fatal(err)
	}
	raw := string(mustReadFile(t, dir2+"/"+manifestName))
	if strings.Contains(raw, "kind") {
		t.Errorf("expected no kind key for an authoring rig:\n%s", raw)
	}
	got2, err := readManifest(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if got2.isReview() {
		t.Error("an authoring rig should read isReview()==false")
	}
}

func TestManifestAgentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := writeManifest(dir, manifest{ID: "mir-7", Agent: "codex"}); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.agentKind() != agentCodex {
		t.Errorf("agent = %q, want codex", m.agentKind())
	}

	legacy := t.TempDir()
	if err := writeManifest(legacy, manifest{ID: "mir-8"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_AGENT", "codex") // new-rig default must not relabel old rigs
	m, err = readManifest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if m.agentKind() != agentClaude {
		t.Errorf("legacy agent = %q, want claude", m.agentKind())
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
	if !slices.Equal(m.Branches["rig"], []string{"phinze/mir-1-feature"}) {
		t.Errorf("rig branch = %v, want [phinze/mir-1-feature]", m.Branches["rig"])
	}
	if _, ok := m.Branches["extra"]; ok {
		t.Error("added repo with empty branch should record no branch")
	}
}

// addBranchToManifest appends secondaries, keeps the primary first, and is a
// no-op for a branch already tracked.
func TestAddBranchToManifest(t *testing.T) {
	dir := t.TempDir()
	if err := writeManifest(dir, manifest{ID: "mir-2", Repos: map[string]string{"rig": "phinze/rig"}}); err != nil {
		t.Fatal(err)
	}
	if err := addRepoToManifest(dir, "rig", "phinze/rig", "phinze/mir-2-primary"); err != nil {
		t.Fatal(err)
	}
	added, err := addBranchToManifest(dir, "rig", "phinze/mir-2-bugfix")
	if err != nil || !added {
		t.Fatalf("first track: added=%v err=%v, want added=true", added, err)
	}
	// Re-tracking the same branch changes nothing.
	added, err = addBranchToManifest(dir, "rig", "phinze/mir-2-bugfix")
	if err != nil || added {
		t.Fatalf("re-track: added=%v err=%v, want added=false", added, err)
	}
	// The primary is never demoted, no matter what's re-tracked.
	added, err = addBranchToManifest(dir, "rig", "phinze/mir-2-primary")
	if err != nil || added {
		t.Fatalf("re-track primary: added=%v err=%v, want added=false", added, err)
	}
	m, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"phinze/mir-2-primary", "phinze/mir-2-bugfix"}
	if !slices.Equal(m.Branches["rig"], want) {
		t.Errorf("branches = %v, want %v", m.Branches["rig"], want)
	}
}
