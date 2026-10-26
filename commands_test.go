package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/phinze/rig/internal/mux"
)

// enrichWithPRs must populate, not augment: the radar merges a rig's cached PRs
// back into its status before asking for a refresh, so a second call has to land
// on the same one PR rather than doubling it. This guards the cache-growth bug.
func TestEnrichWithPRsIdempotent(t *testing.T) {
	fakeGh(t)
	dir := t.TempDir()
	m := manifest{
		ID:       "r1",
		Repos:    map[string]string{"repo": "o/r"},
		Branches: map[string][]string{"repo": {"feat"}},
	}
	if err := writeManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	statuses := []rigStatus{{Slug: "r1", Path: dir}}

	enrichWithPRs(statuses)
	if len(statuses[0].PRs) != 1 {
		t.Fatalf("first pass: got %d PRs, want 1", len(statuses[0].PRs))
	}
	// Second pass with PRs already populated (the radar refetch path) must not
	// grow the slice.
	enrichWithPRs(statuses)
	if len(statuses[0].PRs) != 1 {
		t.Fatalf("second pass: got %d PRs, want 1 (append-doubling regression)", len(statuses[0].PRs))
	}
}

func TestAgentState(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	recent := now.Add(-time.Minute)
	stale := now.Add(-time.Hour)

	cases := []struct {
		name string
		last *time.Time
		want string
	}{
		{"no session", nil, ""},
		{"recent turn is working", &recent, "working"},
		{"old turn is idle", &stale, "idle"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentState(c.last, now); got != c.want {
				t.Errorf("agentState = %q, want %q", got, c.want)
			}
		})
	}
}

// agentMarker reads "parked" for a dormant rig regardless of session state, so
// the call-sheet still accounts for review-waiting rigs; otherwise it's the
// live agent state or a dash.
func TestAgentMarker(t *testing.T) {
	cases := []struct {
		name string
		s    rigStatus
		want string
	}{
		{"no session", rigStatus{}, "-"},
		{"working", rigStatus{Agent: "working", SessionLive: true}, "working"},
		{"fresh transcript, dead session", rigStatus{Agent: "working"}, "-"},
		{"parked wins over blank", rigStatus{Parked: true}, "parked"},
		{"parked wins over stale agent", rigStatus{Parked: true, Agent: "idle"}, "parked"},
		{"finished turn waits on Paul", rigStatus{Agent: "idle", SessionLive: true, AgentStatus: &agentStatus{State: "done"}}, "done"},
		{"blocked names its kind", rigStatus{Agent: "idle", SessionLive: true, AgentStatus: &agentStatus{State: "blocked", Kind: "permission"}}, "blocked:permission"},
		{"blocked without a kind", rigStatus{Agent: "working", SessionLive: true, AgentStatus: &agentStatus{State: "blocked"}}, "blocked"},
		{"reported idle reads idle", rigStatus{Agent: "idle", SessionLive: true, AgentStatus: &agentStatus{State: "idle"}}, "idle"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentMarker(c.s); got != c.want {
				t.Errorf("agentMarker = %q, want %q", got, c.want)
			}
		})
	}
}

func TestPrMarker(t *testing.T) {
	cases := []struct {
		name string
		prs  []rigPR
		want string
	}{
		{"no pr", nil, "-"},
		{"open with checks", []rigPR{{Repo: "o/rig", Number: 7, State: "OPEN", Checks: "passing"}}, "#7 OPEN/passing"},
		{"merged, no checks", []rigPR{{Repo: "o/rig", Number: 7, State: "MERGED"}}, "#7 MERGED"},
		{"failing checks", []rigPR{{Repo: "o/rig", Number: 12, State: "OPEN", Checks: "failing"}}, "#12 OPEN/failing"},
		{
			"multi-repo prefixes short repo names",
			[]rigPR{
				{Repo: "phinze/infra", Number: 80, State: "OPEN", Checks: "passing"},
				{Repo: "phinze/runtime", Number: 42, State: "OPEN", Checks: "failing"},
			},
			"infra #80 OPEN/passing  runtime #42 OPEN/failing",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := prMarker(rigStatus{PRs: c.prs}); got != c.want {
				t.Errorf("prMarker = %q, want %q", got, c.want)
			}
		})
	}
}

func TestEncodeRigsJSON(t *testing.T) {
	// An empty board still serializes its Rigs as [] so consumers can iterate
	// unconditionally.
	blob, err := encodeRigsJSON(boardDoc{Tier: "cheap"})
	if err != nil {
		t.Fatal(err)
	}
	var empty struct {
		Rigs []map[string]any `json:"rigs"`
	}
	if err := json.Unmarshal(blob, &empty); err != nil {
		t.Fatalf("output is not valid json: %v", err)
	}
	if empty.Rigs == nil {
		t.Errorf("empty encode rigs = null, want []")
	}

	last := time.Unix(1_700_000_000, 0).UTC()
	touched := last.Add(-time.Hour)
	statuses := []rigStatus{{
		ID:          "mir-75",
		Slug:        "mir-75-add-zig-stack",
		Title:       "add zig stack",
		Tracker:     "linear",
		TrackerID:   "MIR-75",
		Path:        "/home/phinze/workspaces/mir-75-add-zig-stack",
		Created:     time.Unix(1_699_999_000, 0).UTC(),
		LastTouched: touched,
		SessionLive: true,
		Agent:       "working",
		LastActive:  &last,
	}}
	doc := boardDoc{GeneratedAt: last, Tier: "cheap", Rigs: rigsForJSON(statuses, false)}
	blob, err = encodeRigsJSON(doc)
	if err != nil {
		t.Fatal(err)
	}

	var round struct {
		GeneratedAt string           `json:"generatedAt"`
		Tier        string           `json:"tier"`
		Rigs        []map[string]any `json:"rigs"`
	}
	if err := json.Unmarshal(blob, &round); err != nil {
		t.Fatalf("output is not valid json: %v", err)
	}
	if round.Tier != "cheap" || round.GeneratedAt == "" {
		t.Errorf("envelope = tier %q generatedAt %q, want cheap and a timestamp", round.Tier, round.GeneratedAt)
	}
	got := round.Rigs[0]
	for k, want := range map[string]any{
		"id":           "mir-75",
		"slug":         "mir-75-add-zig-stack",
		"session_live": true,
		"agent":        "working",
		"tracker":      "linear",
		"tracker_id":   "MIR-75",
		"last_touched": touched.Format(time.RFC3339),
	} {
		if got[k] != want {
			t.Errorf("json[%q] = %v, want %v", k, got[k], want)
		}
	}
	if _, ok := got["last_active"]; !ok {
		t.Error("expected last_active field to be present")
	}

	// A row with no agent omits last_active entirely (omitempty on a nil ptr).
	blob, _ = encodeRigsJSON(boardDoc{Tier: "cheap", Rigs: rigsForJSON([]rigStatus{{ID: "x"}}, false)})
	var bare struct {
		Rigs []map[string]any `json:"rigs"`
	}
	if err := json.Unmarshal(blob, &bare); err != nil {
		t.Fatal(err)
	}
	if _, ok := bare.Rigs[0]["last_active"]; ok {
		t.Error("last_active should be omitted when nil")
	}
	if bare.Rigs[0]["agent"] != "" {
		t.Errorf("agent = %v, want empty", bare.Rigs[0]["agent"])
	}
}

// Each live rig takes its agents' own reports over the mtime guess. The
// agent that most needs Paul speaks for the rig, the rig still reads working
// while any agent is, and a rig with no reports or no session keeps the guess.
func TestApplyAgentStatuses(t *testing.T) {
	child := func(state, kind string) agentChild {
		return agentChild{Status: mux.ProgramStatus{State: state, Kind: kind, Updated: 1_700_000_000}}
	}
	rig := func(path, agent string, live bool) rigStatus {
		return rigStatus{Path: path, Backend: "rex", Agent: agent, SessionLive: live}
	}
	statuses := []rigStatus{
		rig("/w/mixed", "idle", true),
		rig("/w/blocked", "working", true),
		rig("/w/silent", "working", true),
		rig("/w/dead", "working", false),
	}
	agents := map[sessionKey][]agentChild{
		rigKey("/w/mixed", "rex"):   {child("done", ""), child("working", "")},
		rigKey("/w/blocked", "rex"): {child("working", ""), child("blocked", "question")},
		rigKey("/w/silent", "rex"):  {{}},
		rigKey("/w/dead", "rex"):    {child("done", "")},
	}
	applyAgentStatuses(statuses, agents)

	if s := statuses[0]; s.Agent != "working" || s.AgentStatus == nil || s.AgentStatus.State != "done" || s.AgentStatus.Since == nil {
		t.Errorf("mixed = %+v %+v, want working with a done report", s, s.AgentStatus)
	}
	if s := statuses[1]; s.AgentStatus == nil || s.AgentStatus.State != "blocked" || s.AgentStatus.Kind != "question" {
		t.Errorf("blocked = %+v, want the blocked agent to speak for the rig", s.AgentStatus)
	}
	if s := statuses[2]; s.AgentStatus != nil || s.Agent != "working" {
		t.Errorf("silent = %+v, want the guess kept", s)
	}
	if s := statuses[3]; s.AgentStatus != nil {
		t.Errorf("dead session = %+v, want no report", s.AgentStatus)
	}
}
