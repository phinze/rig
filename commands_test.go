package main

import (
	"encoding/json"
	"testing"
	"time"
)

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

func TestPrMarker(t *testing.T) {
	cases := []struct {
		name string
		prs  []rigPR
		want string
	}{
		{"no pr", nil, "-"},
		{"open with checks", []rigPR{{Repo: "o/rig", prInfo: prInfo{Number: 7, State: "OPEN", Checks: "passing"}}}, "#7 OPEN/passing"},
		{"merged, no checks", []rigPR{{Repo: "o/rig", prInfo: prInfo{Number: 7, State: "MERGED"}}}, "#7 MERGED"},
		{"failing checks", []rigPR{{Repo: "o/rig", prInfo: prInfo{Number: 12, State: "OPEN", Checks: "failing"}}}, "#12 OPEN/failing"},
		{
			"multi-repo prefixes short repo names",
			[]rigPR{
				{Repo: "phinze/infra", prInfo: prInfo{Number: 80, State: "OPEN", Checks: "passing"}},
				{Repo: "phinze/runtime", prInfo: prInfo{Number: 42, State: "OPEN", Checks: "failing"}},
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
	// nil must serialize as [] so consumers can iterate unconditionally.
	blob, err := encodeRigsJSON(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != "[]" {
		t.Errorf("empty encode = %q, want []", blob)
	}

	last := time.Unix(1_700_000_000, 0).UTC()
	statuses := []rigStatus{{
		ID:          "mir-75",
		Slug:        "mir-75-add-zig-stack",
		Title:       "add zig stack",
		Path:        "/home/phinze/workspaces/mir-75-add-zig-stack",
		Created:     time.Unix(1_699_999_000, 0).UTC(),
		SessionLive: true,
		Agent:       "working",
		LastActive:  &last,
	}}
	blob, err = encodeRigsJSON(statuses)
	if err != nil {
		t.Fatal(err)
	}

	var round []map[string]any
	if err := json.Unmarshal(blob, &round); err != nil {
		t.Fatalf("output is not valid json: %v", err)
	}
	got := round[0]
	for k, want := range map[string]any{
		"id":           "mir-75",
		"slug":         "mir-75-add-zig-stack",
		"session_live": true,
		"agent":        "working",
	} {
		if got[k] != want {
			t.Errorf("json[%q] = %v, want %v", k, got[k], want)
		}
	}
	if _, ok := got["last_active"]; !ok {
		t.Error("expected last_active field to be present")
	}
	// PRs are omitted unless --full populated them.
	if _, ok := got["prs"]; ok {
		t.Error("prs should be omitted when empty")
	}

	// A row with no agent omits last_active entirely (omitempty on a nil ptr).
	blob, _ = encodeRigsJSON([]rigStatus{{ID: "x"}})
	var bare []map[string]any
	if err := json.Unmarshal(blob, &bare); err != nil {
		t.Fatal(err)
	}
	if _, ok := bare[0]["last_active"]; ok {
		t.Error("last_active should be omitted when nil")
	}
	if bare[0]["agent"] != "" {
		t.Errorf("agent = %v, want empty", bare[0]["agent"])
	}
}
