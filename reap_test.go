package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUnmergedReason drives the squash-merge gate: a workspace with off-trunk
// commits should reap once GitHub confirms the branch's PR merged, but stay
// put when it's still open, has no PR, or grew new work on top of the merge.
func TestUnmergedReason(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}
	t.Setenv("JJ_USER", "Test")
	t.Setenv("JJ_EMAIL", "test@example.com")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	fakeGh(t)

	ws := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Colocated repo with an empty init commit on main as trunk(), then a
	// non-empty feature commit C off trunk carrying the "feat" bookmark, with
	// @ parked on an empty child of C. That's the post-squash-merge shape:
	// C is off-trunk and looks unmerged to jj.
	run("git", "init", "-q", "-b", "main")
	run("git", "commit", "-q", "--allow-empty", "-m", "init")
	run("jj", "git", "init", "--colocate")
	run("jj", "config", "set", "--repo", `revset-aliases."trunk()"`, "main")
	write("feat.txt", "the feature\n")
	run("jj", "commit", "-m", "feat") // snapshots feat.txt into C, advances @
	run("jj", "bookmark", "create", "feat", "-r", "@-")

	t.Run("merged PR clears the gate", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "MERGED")
		if reason := unmergedReason(ws, "o/r", "fakerepo"); reason != "" {
			t.Errorf("expected merged PR to reap, blocked by: %q", reason)
		}
	})

	t.Run("open PR keeps the rig", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "OPEN")
		if reason := unmergedReason(ws, "o/r", "fakerepo"); reason == "" {
			t.Error("expected open PR to keep the rig")
		}
	})

	t.Run("no PR keeps the rig", func(t *testing.T) {
		t.Setenv("GH_FAKE_NOPR", "1")
		if reason := unmergedReason(ws, "o/r", "fakerepo"); reason == "" {
			t.Error("expected missing PR to keep the rig")
		}
	})

	// Layer a second non-empty commit D on top of the merged branch: now even
	// a merged PR shouldn't reap, because D isn't accounted for.
	t.Run("work beyond the merge keeps the rig", func(t *testing.T) {
		write("more.txt", "kept working\n")
		run("jj", "commit", "-m", "more")
		t.Setenv("GH_FAKE_STATE", "MERGED")
		reason := unmergedReason(ws, "o/r", "fakerepo")
		if reason == "" {
			t.Error("expected post-merge work to keep the rig")
		}
	})
}
