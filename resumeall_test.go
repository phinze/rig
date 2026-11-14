package main

import (
	"slices"
	"testing"
)

func TestResumeAllTargets(t *testing.T) {
	statuses := []rigStatus{
		{ID: "zeta", Path: "/w/zeta", SessionLive: true, Agent: agentNone},
		{ID: "alpha", Path: "/w/alpha", SessionLive: false},
		{ID: "self", Path: "/w/self", SessionLive: true, Agent: agentNone},
		{ID: "working", Path: "/w/working", SessionLive: true, Agent: "working"},
		{ID: "idle", Path: "/w/idle", SessionLive: true, Agent: "idle"},
		{ID: "parked", Path: "/w/parked", Parked: true},
		{ID: "half", Path: "/w/half", Building: "repo"},
		{ID: "far", Path: "/w/far", remote: &remoteRig{}},
	}
	var got []string
	for _, s := range resumeAllTargets(statuses, rigSessionName("/w/self")) {
		got = append(got, s.ID)
	}
	// The caller's own rig goes last, since resuming it from its own seat
	// replaces the process doing the loop.
	if want := []string{"alpha", "zeta", "self"}; !slices.Equal(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}
}
