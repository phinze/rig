package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBoardState(t *testing.T) {
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
			if got := boardState(c.s); got != c.want {
				t.Errorf("boardState = %q, want %q", got, c.want)
			}
		})
	}
}

// writeBoardFixtureRig lays down a manifest the way sweep_test's mk does: enough
// for the local board reads to be real.
func writeBoardFixtureRig(t *testing.T, m manifest) string {
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

// TestRigsForJSONCheapVsFull checks the encoder: the cheap tier leaves every
// repo's PRs null ("not looked"), and the full tier nests the row's flat PRs
// into the repo they belong to, keyed by owner/repo.
func TestRigsForJSONCheapVsFull(t *testing.T) {
	prs := []rigPR{{
		Repo: "miren/zig", Branch: "mir-75-add-zig-stack",
		prInfo: prInfo{Number: 7, State: "OPEN", URL: "https://example/pr/7",
			Checks: "failing", Review: "CHANGES_REQUESTED", FailingChecks: []string{"pop"}},
	}}
	row := rigStatus{
		ID: "mir-75", Slug: "mir-75-zig",
		Repos: []rigRepo{{Name: "miren/zig", Branches: []string{"mir-75-add-zig-stack"}}},
		PRs:   prs,
	}

	cheap := rigsForJSON([]rigStatus{row}, false)
	if cheap[0].Repos[0].PRs != nil {
		t.Error("cheap tier must leave prs null (not looked), got a value")
	}
	blob, err := json.Marshal(cheap[0])
	if err != nil {
		t.Fatal(err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(blob, &asMap); err != nil {
		t.Fatal(err)
	}
	if v := asMap["repos"].([]any)[0].(map[string]any)["prs"]; v != nil {
		t.Errorf("cheap prs should marshal as null, got %v", v)
	}

	full := rigsForJSON([]rigStatus{row}, true)
	if full[0].Repos[0].PRs == nil {
		t.Fatal("full tier must materialize prs, got nil")
	}
	got := (*full[0].Repos[0].PRs)
	if len(got) != 1 || got[0].Number != 7 || got[0].State != "OPEN" {
		t.Errorf("pr = %+v, want number 7 OPEN (prInfo dialect)", got)
	}
	// The flat row PRs must not double into the wire shape.
	if blob, _ := json.Marshal(full[0]); json.Valid(blob) {
		var m map[string]any
		_ = json.Unmarshal(blob, &m)
		if _, ok := m["prs"]; ok {
			t.Error("flat PRs should not serialize on the wire shape (tagged json:-)")
		}
	}
}

// TestRigsForJSONFullTierWithNoPR: a full-tier repo with no PR reads as an
// empty list, never null — "looked, found none".
func TestRigsForJSONFullTierWithNoPR(t *testing.T) {
	row := rigStatus{ID: "pers-9", Repos: []rigRepo{{Name: "o/r"}}}
	full := rigsForJSON([]rigStatus{row}, true)
	if full[0].Repos[0].PRs == nil {
		t.Fatal("full tier with no PR must be an empty list, not null")
	}
	blob, err := json.Marshal(full[0])
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
}

func TestEnrichBoardPRsUsesFreshCache(t *testing.T) {
	// A fresh cache entry for every rig means no gh work at all: the test would
	// hang or fail against a real gh if the fan-out ran, so passing is the proof
	// the cache short-circuited it.
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
	asOf := enrichBoardPRs(statuses, false, time.Now())
	if asOf == nil {
		t.Fatal("expected a prsAsOf timestamp")
	}
	if len(statuses[0].PRs) != 1 || statuses[0].PRs[0].Number != 7 {
		t.Errorf("PRs = %+v, want the cached answer", statuses[0].PRs)
	}
}

func TestBoardInboxFor(t *testing.T) {
	pinned := []notification{
		{Level: "info", Title: "fyi"},
		{Level: "error", Title: "broke"},
		{Level: "warn", Title: "hmm"},
	}
	box := boardInboxFor(pinned)
	if box.NotifyCount != 3 {
		t.Errorf("count = %d, want 3", box.NotifyCount)
	}
	if box.WorstLevel != "error" {
		t.Errorf("worst = %q, want error", box.WorstLevel)
	}
	if got := boardInboxFor(nil); got.NotifyCount != 0 || got.WorstLevel != "" {
		t.Errorf("empty inbox = %+v, want zero values", got)
	}
}
