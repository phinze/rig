package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCensusState(t *testing.T) {
	last := time.Now()
	cases := []struct {
		name string
		s    rigStatus
		want string
	}{
		{"parked wins over liveness", rigStatus{Parked: true, SessionLive: true, Agent: "working"}, "parked"},
		{"half-built wins over liveness", rigStatus{Building: "o/r", SessionLive: true}, "building"},
		{"no session is stopped", rigStatus{}, "stopped"},
		{"live and recent is working", rigStatus{SessionLive: true, Agent: "working", LastActive: &last}, "working"},
		{"live and quiet is idle", rigStatus{SessionLive: true, Agent: "idle", LastActive: &last}, "idle"},
		{"live with no signal is idle", rigStatus{SessionLive: true}, "idle"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := censusState(c.s); got != c.want {
				t.Errorf("censusState = %q, want %q", got, c.want)
			}
		})
	}
}

// writeCensusFixtureRig lays down a manifest the way sweep_test's mk does:
// enough for censusRigFor's reads to be real.
func writeCensusFixtureRig(t *testing.T, m manifest) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCensusRigForCheapVsFull(t *testing.T) {
	m := manifest{
		ID:        "mir-75",
		Title:     "add zig stack",
		Tracker:   "linear",
		TrackerID: "MIR-75",
		Created:   time.Now().Add(-48 * time.Hour),
		Agent:     "codex",
		Repos:     map[string]string{"zig": "miren/zig"},
	}
	dir := writeCensusFixtureRig(t, m)
	prs := []rigPR{{
		Repo: "miren/zig", Branch: "mir-75-add-zig-stack",
		prInfo: prInfo{Number: 7, State: "OPEN", URL: "https://example/pr/7",
			Checks: "failing", Review: "CHANGES_REQUESTED", FailingChecks: []string{"pop"}},
	}}
	s := rigStatus{ID: m.ID, Path: dir, Repos: []string{"miren/zig"}, SessionLive: true, Agent: "idle", PRs: prs}

	cheap := censusRigFor(s, nil, true)
	if cheap.Agent.Type != "codex" {
		t.Errorf("agent type = %q, want codex", cheap.Agent.Type)
	}
	if cheap.Links.Linear == nil || *cheap.Links.Linear != "MIR-75" {
		t.Errorf("linear link = %v, want MIR-75", cheap.Links.Linear)
	}
	if cheap.Links.Vikunja != nil || cheap.Links.GitHub != nil {
		t.Errorf("untouched link slots should stay null, got %+v", cheap.Links)
	}
	if len(cheap.Repos) != 1 {
		t.Fatalf("repos = %d, want 1", len(cheap.Repos))
	}
	if cheap.Repos[0].PRs != nil {
		t.Error("cheap tier must leave prs null (not looked), got a value")
	}
	if cheap.Disposition != "" {
		t.Errorf("cheap tier must not compute a disposition, got %q", cheap.Disposition)
	}

	full := censusRigFor(s, nil, false)
	if full.Repos[0].PRs == nil {
		t.Fatal("full tier must populate prs, got null")
	}
	got := (*full.Repos[0].PRs)[0]
	if got.Number != 7 || got.State != "open" || got.Review != "changes_requested" {
		t.Errorf("pr = %+v, want number 7 open changes_requested", got)
	}
	if got.CI.Status != "failing" || len(got.CI.FailingJobs) != 1 || got.CI.FailingJobs[0] != "pop" {
		t.Errorf("ci = %+v, want failing [pop]", got.CI)
	}
	if full.Disposition != "changes requested" {
		t.Errorf("disposition = %q, want %q", full.Disposition, "changes requested")
	}
}

func TestCensusRigForFullTierWithNoPR(t *testing.T) {
	m := manifest{ID: "pers-9", Title: "scratch", Repos: map[string]string{"r": "o/r"}}
	dir := writeCensusFixtureRig(t, m)
	s := rigStatus{ID: m.ID, Path: dir, Repos: []string{"o/r"}}

	row := censusRigFor(s, nil, false)
	if row.Repos[0].PRs == nil {
		t.Fatal("full tier with no PR must be an empty list, not null")
	}
	blob, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(blob, &round); err != nil {
		t.Fatal(err)
	}
	prs, ok := round["repos"].([]any)[0].(map[string]any)["prs"].([]any)
	if !ok || len(prs) != 0 {
		t.Errorf("prs should marshal as [], got %v", round["repos"])
	}
	if row.Disposition != "no PR" {
		t.Errorf("disposition = %q, want %q", row.Disposition, "no PR")
	}
}

func TestCensusEnrichPRsUsesFreshCache(t *testing.T) {
	// A fresh cache entry for every rig means no gh work at all: the test
	// would hang or fail against a real gh if the fan-out ran, so passing is
	// the proof the cache short-circuited it.
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	dir := filepath.Join(cacheRoot, "rig")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cached := map[string]radarCacheEntry{
		"mir-75-x": {At: time.Now(), PRs: []rigPR{{Repo: "o/r", prInfo: prInfo{Number: 7, State: "OPEN"}}}},
	}
	blob, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "radar-prs.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}

	statuses := []rigStatus{{ID: "mir-75", Slug: "mir-75-x"}}
	asOf := censusEnrichPRs(statuses, false, time.Now())
	if asOf == nil {
		t.Fatal("expected a prsAsOf timestamp")
	}
	if len(statuses[0].PRs) != 1 || statuses[0].PRs[0].Number != 7 {
		t.Errorf("PRs = %+v, want the cached answer", statuses[0].PRs)
	}
}

func TestCensusInboxFor(t *testing.T) {
	pinned := []notification{
		{Level: "info", Title: "fyi"},
		{Level: "error", Title: "broke"},
		{Level: "warn", Title: "hmm"},
	}
	box := censusInboxFor(pinned)
	if box.NotifyCount != 3 {
		t.Errorf("count = %d, want 3", box.NotifyCount)
	}
	if box.WorstLevel != "error" {
		t.Errorf("worst = %q, want error", box.WorstLevel)
	}
	if got := censusInboxFor(nil); got.NotifyCount != 0 || got.WorstLevel != "" {
		t.Errorf("empty inbox = %+v, want zero values", got)
	}
}
