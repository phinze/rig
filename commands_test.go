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
