package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeHalfBuiltRig lays down exactly what an interrupted create leaves behind:
// a manifest naming the repo it was about to build, and no workspace under it.
func writeHalfBuiltRig(t *testing.T, basedir, id, trackerID, repo string) {
	t.Helper()
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := manifest{
		ID: id, Title: "Half a rig", Tracker: "linear", TrackerID: trackerID,
		MainRepo: repo[strings.Index(repo, "/")+1:], BuildingRepo: repo,
		Created: time.Now().Add(-time.Hour),
	}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}
}

func TestManifestRoundTripsBuildingRepo(t *testing.T) {
	dir := t.TempDir()
	if err := writeManifest(dir, manifest{ID: "mir-1", BuildingRepo: "mirendev/cloud"}); err != nil {
		t.Fatal(err)
	}
	got, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.BuildingRepo != "mirendev/cloud" {
		t.Fatalf("BuildingRepo = %q, want mirendev/cloud", got.BuildingRepo)
	}
	if !rigCreationInterrupted(got) {
		t.Fatal("a manifest with building_repo should read as creation-interrupted")
	}
}

// A rig predating the field must not start reading as half-built, or every rig
// on disk when this shipped would have become unenterable.
func TestLegacyManifestReadsAsComplete(t *testing.T) {
	dir := t.TempDir()
	if err := writeManifest(dir, manifest{
		ID: "mir-1", MainRepo: "cloud", Repos: map[string]string{"cloud": "mirendev/cloud"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rigCreationInterrupted(got) {
		t.Fatal("a manifest with no building_repo must read as complete")
	}
}

// The record of intent has to be cleared by the act of making the workspace
// real, or a finished rig would go on advertising itself as wreckage.
func TestAddRepoToManifestClearsBuildingRepo(t *testing.T) {
	dir := t.TempDir()
	writeHalfBuiltRig(t, dir, "mir-1", "MIR-1", "mirendev/cloud")

	if err := addRepoToManifest(dir, "cloud", "mirendev/cloud", "phinze/mir-1"); err != nil {
		t.Fatal(err)
	}
	got, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.BuildingRepo != "" {
		t.Fatalf("BuildingRepo = %q, want cleared once the workspace exists", got.BuildingRepo)
	}
	if got.Repos["cloud"] != "mirendev/cloud" {
		t.Fatalf("Repos = %v, want the repo recorded", got.Repos)
	}
}

// A second repo arriving via `rig add` must not clear another repo's in-flight
// marker, which is why the clear is keyed on the repo that actually landed.
func TestAddRepoToManifestLeavesOtherRepoBuilding(t *testing.T) {
	dir := t.TempDir()
	writeHalfBuiltRig(t, dir, "mir-1", "MIR-1", "mirendev/cloud")

	if err := addRepoToManifest(dir, "runtime", "mirendev/runtime", ""); err != nil {
		t.Fatal(err)
	}
	got, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.BuildingRepo != "mirendev/cloud" {
		t.Fatalf("BuildingRepo = %q, want the pending repo untouched", got.BuildingRepo)
	}
}

func TestCreateBasedirAdoptsHalfBuiltRig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	basedir := filepath.Join(home, "workspaces", "mir-1-half-a-rig")
	writeHalfBuiltRig(t, basedir, "mir-1", "MIR-1", "mirendev/cloud")

	before, err := readManifest(basedir)
	if err != nil {
		t.Fatal(err)
	}

	err = createBasedir(basedir, manifest{
		ID: "mir-1", Title: "Half a rig", Tracker: "linear", TrackerID: "MIR-1",
		MainRepo: "cloud", BuildingRepo: "mirendev/cloud", Agent: "codex",
	})
	if err != nil {
		t.Fatalf("createBasedir should adopt its own wreckage: %v", err)
	}
	got, err := readManifest(basedir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "codex" {
		t.Fatalf("Agent = %q, want the retry's values written through", got.Agent)
	}
	// The task started when you first ran the command; a retry shouldn't reset
	// the age ls sorts and renders by.
	if !got.Created.Equal(before.Created) {
		t.Fatalf("Created = %v, want the original %v preserved", got.Created, before.Created)
	}
}

func TestCreateBasedirRefusesCompleteRig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	basedir := filepath.Join(home, "workspaces", "mir-1-real-work")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(basedir, manifest{
		ID: "mir-1", MainRepo: "cloud", Repos: map[string]string{"cloud": "mirendev/cloud"},
	}); err != nil {
		t.Fatal(err)
	}

	err := createBasedir(basedir, manifest{ID: "mir-1", BuildingRepo: "mirendev/cloud"})
	if err == nil {
		t.Fatal("createBasedir must never stomp a rig that finished being built")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want the existing-basedir refusal", err)
	}
}

// An unreadable manifest is not proof of wreckage, so the basedir stays
// untouchable rather than being adopted on a guess.
func TestCreateBasedirRefusesBasedirWithoutManifest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	basedir := filepath.Join(home, "workspaces", "mystery")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := createBasedir(basedir, manifest{ID: "mystery", BuildingRepo: "a/b"}); err == nil {
		t.Fatal("createBasedir must refuse a basedir whose manifest it cannot read")
	}
}

// The regression that made the failure permanent: `up` matched the id, handed
// off to activateRig, and died in ensureRigRuntime. It has to decline instead so
// the create path gets a turn.
func TestAttachExistingRigDeclinesHalfBuiltRig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	basedir := filepath.Join(home, "workspaces", "mir-1564-cluster-detail")
	writeHalfBuiltRig(t, basedir, "mir-1564", "MIR-1564", "mirendev/cloud")

	done, unfinished, err := attachExistingRig("mir-1564")
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("attachExistingRig must not claim a rig it cannot enter")
	}
	if unfinished == nil {
		t.Fatal("attachExistingRig should hand back the half-built rig to finish")
	}
	if unfinished.Building != "mirendev/cloud" {
		t.Fatalf("Building = %q, want the repo the interrupted create recorded", unfinished.Building)
	}
}

// Matching by tracker id as well as local id is what `rig adopt` relies on, and
// it has to keep working for the half-built case too.
func TestAttachExistingRigDeclinesHalfBuiltByTrackerID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	basedir := filepath.Join(home, "workspaces", "some-kickoff-slug")
	writeHalfBuiltRig(t, basedir, "some-kickoff-slug", "MIR-1564", "mirendev/cloud")

	done, unfinished, err := attachExistingRig("mir-1564")
	if err != nil {
		t.Fatal(err)
	}
	if done || unfinished == nil {
		t.Fatalf("done = %v, unfinished = %v; want the tracker-id match handed back", done, unfinished)
	}
}

// captureRigRuntimeHints runs on the repair path. Blanking MainRepo there
// destroyed the only surviving record of which repo the rig was for, so each
// failed retry left it less recoverable.
func TestCaptureRigRuntimeHintsKeepsMainRepoWithNoWorkspace(t *testing.T) {
	dir := t.TempDir()
	m := manifest{ID: "mir-1564", MainRepo: "cloud", BuildingRepo: "mirendev/cloud"}
	captureRigRuntimeHints(dir, &m, false)
	if m.MainRepo != "cloud" {
		t.Fatalf("MainRepo = %q, want it preserved when there is no workspace to replace it", m.MainRepo)
	}
}

func TestFinishRigHintNamesTheCommand(t *testing.T) {
	got := finishRigHint(manifest{ID: "mir-1564", TrackerID: "MIR-1564"})
	if !strings.Contains(got, "rig up MIR-1564") {
		t.Fatalf("hint = %q, want it to name the finishing command", got)
	}
	// A kickoff rig has no id to re-up from, so the hint must not invent one.
	got = finishRigHint(manifest{ID: "some-kickoff"})
	if strings.Contains(got, "rig up") {
		t.Fatalf("hint = %q, want no up command for a rig with no tracker id", got)
	}
}

func TestAgentMarkerFlagsHalfBuiltRig(t *testing.T) {
	if got := agentMarker(rigStatus{Building: "mirendev/cloud"}); got != "half" {
		t.Fatalf("agentMarker = %q, want half", got)
	}
	// Parked still wins for an ordinary rig; only the half-built case jumps it.
	if got := agentMarker(rigStatus{Parked: true}); got != "parked" {
		t.Fatalf("agentMarker = %q, want parked", got)
	}
}

// A rig with no workspace, no commits, and no conversation has nothing to weigh,
// so sweep skips the whole ladder and offers to clear it out.
func TestSweepDecisionCollectsHalfBuiltRig(t *testing.T) {
	action, detail := sweepDecision(sweepInput{HalfBuilt: true, Disp: "no PR"})
	if action != actionDown {
		t.Fatalf("action = %v, want %v", action, actionDown)
	}
	if !strings.Contains(detail, "never finished") {
		t.Fatalf("detail = %q, want it to say why", detail)
	}
}
