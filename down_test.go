package main

import (
	"testing"
)

// unmergedPRsBlocker is `rig down`'s eager gate: every recorded branch's PR must
// be merged (or absent). It's what refuses a teardown while a PR — primary or a
// tracked secondary — is still open, independent of local commit state.
func TestUnmergedPRsBlocker(t *testing.T) {
	fakeGh(t)
	dir := t.TempDir()
	if err := writeManifest(dir, manifest{
		ID:       "mir-1",
		Repos:    map[string]string{"rig": "o/r"},
		Branches: map[string][]string{"rig": {"feat"}},
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("merged clears the gate", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "MERGED")
		if reason := unmergedPRsBlocker(dir); reason != "" {
			t.Errorf("expected merged PR to clear, blocked by: %q", reason)
		}
	})

	t.Run("open PR blocks", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "OPEN")
		if reason := unmergedPRsBlocker(dir); reason == "" {
			t.Error("expected an open PR to block teardown")
		}
	})

	t.Run("no PR does not block", func(t *testing.T) {
		t.Setenv("GH_FAKE_NOPR", "1")
		if reason := unmergedPRsBlocker(dir); reason != "" {
			t.Errorf("expected a branch with no PR to pass, blocked by: %q", reason)
		}
	})

	t.Run("open secondary blocks even when primary merged", func(t *testing.T) {
		// The whole point of rig track: a second PR on the same repo counts.
		if _, err := addBranchToManifest(dir, "rig", "bugfix"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GH_FAKE_STATE", "OPEN")
		if reason := unmergedPRsBlocker(dir); reason == "" {
			t.Error("expected an open secondary PR to block teardown")
		}
	})

	t.Run("gh failure blocks, fail-closed", func(t *testing.T) {
		t.Setenv("GH_FAKE_ERR", "1")
		if reason := unmergedPRsBlocker(dir); reason == "" {
			t.Error("expected a gh error to block teardown")
		}
	})
}
