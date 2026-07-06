package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestWorkspaceTeardownBlocker drives the squash-merge gate: a workspace with
// off-trunk commits should be reapable once GitHub confirms the branch's PR
// merged, but stay put when it's still open, has no PR, or grew new work on top
// of the merge.
func TestWorkspaceTeardownBlocker(t *testing.T) {
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
		if reason := workspaceTeardownBlocker(ws, "o/r", "fakerepo", []string{"feat"}, false, ""); reason != "" {
			t.Errorf("expected merged PR to reap, blocked by: %q", reason)
		}
	})

	t.Run("open PR keeps the rig", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "OPEN")
		if reason := workspaceTeardownBlocker(ws, "o/r", "fakerepo", []string{"feat"}, false, ""); reason == "" {
			t.Error("expected open PR to keep the rig")
		}
	})

	t.Run("no PR keeps the rig", func(t *testing.T) {
		t.Setenv("GH_FAKE_NOPR", "1")
		if reason := workspaceTeardownBlocker(ws, "o/r", "fakerepo", []string{"feat"}, false, ""); reason == "" {
			t.Error("expected missing PR to keep the rig")
		}
	})

	t.Run("no branch keeps the rig without asking gh", func(t *testing.T) {
		// No recorded branch means we can't map the work to a PR; the local
		// off-trunk commit alone keeps the rig, no gh round-trip.
		if reason := workspaceTeardownBlocker(ws, "o/r", "fakerepo", nil, false, ""); reason == "" {
			t.Error("expected a branchless workspace to keep the rig")
		}
	})

	// Layer a second non-empty commit D on top of the merged branch: now even
	// a merged PR shouldn't reap, because D isn't accounted for.
	t.Run("work beyond the merge keeps the rig", func(t *testing.T) {
		write("more.txt", "kept working\n")
		run("jj", "commit", "-m", "more")
		t.Setenv("GH_FAKE_STATE", "MERGED")
		reason := workspaceTeardownBlocker(ws, "o/r", "fakerepo", []string{"feat"}, false, "")
		if reason == "" {
			t.Error("expected post-merge work to keep the rig")
		}
	})

	// Review-rig gate (review=true): the same off-trunk author commits that
	// block an authoring rig are irrelevant here — what matters is whether "me"
	// has posted a review. The PR stays OPEN throughout, proving merge state is
	// beside the point for a review.
	t.Run("review rig: unreviewed PR keeps the rig", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "OPEN")
		t.Setenv("GH_FAKE_REVIEWS", `[]`)
		reason := workspaceTeardownBlocker(ws, "o/r", "fakerepo", []string{"feat"}, true, "me")
		if reason == "" {
			t.Error("expected an unreviewed PR to keep the review rig")
		}
	})

	t.Run("review rig: my posted review clears the gate", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "OPEN")
		t.Setenv("GH_FAKE_REVIEWS", `[{"author":{"login":"me"},"state":"COMMENTED"}]`)
		// Park @ on a fresh empty child so gate 2 (uncommitted changes) is clear;
		// the author's commits sit in @'s parents, which the review gate ignores.
		run("jj", "new")
		reason := workspaceTeardownBlocker(ws, "o/r", "fakerepo", []string{"feat"}, true, "me")
		if reason != "" {
			t.Errorf("expected my posted review to reap the rig, blocked by: %q", reason)
		}
	})

	t.Run("review rig: someone else's review doesn't count", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "OPEN")
		t.Setenv("GH_FAKE_REVIEWS", `[{"author":{"login":"someone-else"},"state":"APPROVED"}]`)
		reason := workspaceTeardownBlocker(ws, "o/r", "fakerepo", []string{"feat"}, true, "me")
		if reason == "" {
			t.Error("expected another user's review to keep my review rig")
		}
	})
}
