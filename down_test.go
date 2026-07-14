package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestIsUnder(t *testing.T) {
	tests := []struct {
		child, parent string
		want          bool
	}{
		{"/rigs/task/cloud", "/rigs/task", true},
		{"/rigs/task", "/rigs/task", true},
		{"/rigs/task-other/cloud", "/rigs/task", false}, // prefix but not nested
		{"/rigs/other", "/rigs/task", false},
		{"/rigs/task/a/b/c", "/rigs/task", true},
	}
	for _, tt := range tests {
		if got := isUnder(tt.child, tt.parent); got != tt.want {
			t.Errorf("isUnder(%q, %q) = %v, want %v", tt.child, tt.parent, got, tt.want)
		}
	}
}

func TestTeardownQuarantinesBeforeRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	t.Setenv("PATH", t.TempDir()) // Skip optional iso and docker cleanup.

	t.Run("permission failure leaves only quarantined debris", func(t *testing.T) {
		root := t.TempDir()
		basedir := filepath.Join(root, "rig")
		bin := filepath.Join(basedir, "runtime", "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "miren"), nil, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(bin, 0o555); err != nil {
			t.Fatal(err)
		}

		err := teardownRig(basedir, manifest{})
		if err != nil {
			t.Fatalf("permission failure in quarantined cleanup should not fail teardown: %v", err)
		}
		if _, err := os.Stat(basedir); !os.IsNotExist(err) {
			t.Errorf("canonical basedir still exists after quarantine: %v", err)
		}

		trashRoot := filepath.Join(root, ".rig-trash")
		entries, err := os.ReadDir(trashRoot)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "rig-") {
			t.Fatalf("expected one clearly named quarantined rig, got %v", entries)
		}
		quarantinedBin := filepath.Join(trashRoot, entries[0].Name(), "runtime", "bin")
		t.Cleanup(func() { _ = os.Chmod(quarantinedBin, 0o755) })
		if _, err := os.Stat(filepath.Join(quarantinedBin, "miren")); err != nil {
			t.Errorf("root-owned stand-in was not left in quarantine: %v", err)
		}
	})

	t.Run("successful removal cleans up the quarantine", func(t *testing.T) {
		root := t.TempDir()
		basedir := filepath.Join(root, "rig")
		if err := os.Mkdir(basedir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(basedir, "scratch"), nil, 0o644); err != nil {
			t.Fatal(err)
		}

		if err := teardownRig(basedir, manifest{}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(basedir); !os.IsNotExist(err) {
			t.Errorf("canonical basedir still exists: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, ".rig-trash")); !os.IsNotExist(err) {
			t.Errorf("empty quarantine directory still exists: %v", err)
		}
	})
}
