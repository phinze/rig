package main

import (
	"os"
	"path/filepath"
	"testing"
)

func notifyTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func mustPost(t *testing.T, args ...string) {
	t.Helper()
	if err := runNotifyPost(args); err != nil {
		t.Fatalf("post %v: %v", args, err)
	}
}

// The archetypal poster is an hourly cron repeating itself. Re-posting the same
// (source, key) has to land on one entry that counts the repeats and keeps its
// original Created, or the inbox becomes a log and stops being readable.
func TestNotifyPostUpsertsAndCounts(t *testing.T) {
	notifyTestEnv(t)

	mustPost(t, "--source", "nix-config-sync", "--key", "stall",
		"--level", "warn", "--title", "input bump stalled", "--body", "1 run")
	first := activeNotifications()
	if len(first) != 1 || first[0].Count != 1 {
		t.Fatalf("after one post: %+v", first)
	}

	mustPost(t, "--source", "nix-config-sync", "--key", "stall",
		"--level", "error", "--title", "input bump stalled", "--body", "2 runs")
	got := activeNotifications()
	if len(got) != 1 {
		t.Fatalf("re-post should update one entry, got %d", len(got))
	}
	if got[0].Count != 2 {
		t.Errorf("count = %d, want 2", got[0].Count)
	}
	if !got[0].Created.Equal(first[0].Created) {
		t.Errorf("Created moved on re-post: %v → %v", first[0].Created, got[0].Created)
	}
	// Everything describing the current state should be the new value.
	if got[0].Level != "error" || got[0].Body != "2 runs" {
		t.Errorf("re-post did not refresh state: level=%q body=%q", got[0].Level, got[0].Body)
	}
}

// A different key from the same source is a different story and gets its own row.
func TestNotifyDistinctKeysCoexist(t *testing.T) {
	notifyTestEnv(t)
	mustPost(t, "--source", "s", "--key", "a", "--title", "first")
	mustPost(t, "--source", "s", "--key", "b", "--title", "second")
	if got := activeNotifications(); len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
}

func TestNotifyListOrdersLoudestFirst(t *testing.T) {
	notifyTestEnv(t)
	mustPost(t, "--source", "s", "--key", "quiet", "--level", "info", "--title", "fyi")
	mustPost(t, "--source", "s", "--key", "loud", "--level", "error", "--title", "broken")
	mustPost(t, "--source", "s", "--key", "mid", "--level", "warn", "--title", "iffy")

	got := activeNotifications()
	want := []string{"error", "warn", "info"}
	for i, level := range want {
		if got[i].Level != level {
			t.Errorf("position %d = %q, want %q", i, got[i].Level, level)
		}
	}
}

func TestNotifyDismiss(t *testing.T) {
	notifyTestEnv(t)
	mustPost(t, "--source", "s", "--key", "a", "--title", "first")
	mustPost(t, "--source", "s", "--key", "b", "--title", "second")

	if err := runNotifyDismiss([]string{"s/a"}); err != nil {
		t.Fatalf("dismiss by ref: %v", err)
	}
	// A bare key is accepted while it's unambiguous.
	if err := runNotifyDismiss([]string{"b"}); err != nil {
		t.Fatalf("dismiss by bare key: %v", err)
	}
	if got := activeNotifications(); len(got) != 0 {
		t.Fatalf("inbox should be empty, got %+v", got)
	}
	if err := runNotifyDismiss([]string{"gone"}); err == nil {
		t.Error("dismissing a missing key should error")
	}
}

// Two sources can pick the same key. A bare key stops being a valid handle then,
// and saying so beats dismissing whichever one happened to be first.
func TestNotifyDismissAmbiguousBareKey(t *testing.T) {
	notifyTestEnv(t)
	mustPost(t, "--source", "a", "--key", "stall", "--title", "one")
	mustPost(t, "--source", "b", "--key", "stall", "--title", "two")

	if err := runNotifyDismiss([]string{"stall"}); err == nil {
		t.Fatal("ambiguous bare key should error")
	}
	// The fully-qualified handle still works, and takes only its own entry.
	if err := runNotifyDismiss([]string{"b/stall"}); err != nil {
		t.Fatalf("dismiss by ref: %v", err)
	}
	got := activeNotifications()
	if len(got) != 1 || got[0].Source != "a" {
		t.Fatalf("wrong entry dismissed: %+v", got)
	}
}

func TestNotifyDismissAll(t *testing.T) {
	notifyTestEnv(t)
	mustPost(t, "--source", "s", "--key", "a", "--title", "first")
	mustPost(t, "--source", "s", "--key", "b", "--title", "second")
	if err := runNotifyDismiss([]string{"--all"}); err != nil {
		t.Fatalf("dismiss --all: %v", err)
	}
	if got := activeNotifications(); len(got) != 0 {
		t.Fatalf("want empty inbox, got %+v", got)
	}
}

// Rig-pinned entries ride that rig's row; loose ones banner above the table.
// Every board depends on this split, so it gets its own test.
func TestNotifyRigSplit(t *testing.T) {
	notifyTestEnv(t)
	mustPost(t, "--source", "s", "--key", "loose", "--title", "no rig")
	mustPost(t, "--source", "s", "--key", "mine", "--title", "on a rig", "--rig", "mir-75")
	mustPost(t, "--source", "s", "--key", "theirs", "--title", "other rig", "--rig", "mir-99")

	list := activeNotifications()
	if got := looseNotifications(list); len(got) != 1 || got[0].Key != "loose" {
		t.Errorf("loose = %+v", got)
	}
	if got := notificationsForRig(list, "mir-75"); len(got) != 1 || got[0].Key != "mine" {
		t.Errorf("for mir-75 = %+v", got)
	}
	if got := notificationsForRig(list, "nobody"); len(got) != 0 {
		t.Errorf("for an unknown rig = %+v", got)
	}
}

func TestNotifyPostValidation(t *testing.T) {
	notifyTestEnv(t)
	cases := [][]string{
		{"--key", "k", "--title", "t"},                                     // no source
		{"--source", "s", "--title", "t"},                                  // no key
		{"--source", "s", "--key", "k"},                                    // no title
		{"--source", "s", "--key", "k", "--title", "t", "--level", "loud"}, // bad level
		{"--source", "s", "--key", "k", "--title", "t", "--nope", "x"},     // unknown flag
	}
	for _, args := range cases {
		if err := runNotifyPost(args); err == nil {
			t.Errorf("post %v should have failed", args)
		}
	}
}

// The inbox is advisory. A corrupt file must not become a wall that blocks every
// future post until someone hand-edits JSON.
func TestNotifyCorruptFileRecovers(t *testing.T) {
	notifyTestEnv(t)
	path, err := notifyPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	mustPost(t, "--source", "s", "--key", "k", "--title", "still works")
	if got := activeNotifications(); len(got) != 1 {
		t.Fatalf("want 1 entry after recovering, got %+v", got)
	}
}

// A poster that varies its key (a bug, but a plausible one) must not grow the
// file forever. Eviction drops the stalest, so what is currently recurring stays.
func TestNotifyEvictsBeyondCap(t *testing.T) {
	notifyTestEnv(t)
	for i := range notifyMax + 25 {
		mustPost(t, "--source", "s", "--key", string(rune('a'+i%26))+string(rune('a'+i/26)),
			"--title", "spam")
	}
	if got := activeNotifications(); len(got) > notifyMax {
		t.Fatalf("inbox grew to %d, cap is %d", len(got), notifyMax)
	}
}
