package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	setHermeticGit(t)
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
	featOID, err := exec.Command("jj", "-R", ws, "log", "--no-graph", "-r", "feat", "-T", "commit_id").Output()
	if err != nil {
		t.Fatalf("resolving feature commit: %v", err)
	}
	t.Setenv("GH_FAKE_HEAD_OID", string(featOID))

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
		// Park @ on a fresh empty child so the review's scratch-work gate is clear;
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

// TestEvolvingWorkspaceTeardown covers the long-lived rig shape: one PR
// merges and its branch disappears, then the same workspace grows a second PR.
// Teardown should use GitHub's immutable PR head for the old work and explain
// the new branch precisely, without relying on a timely `jj new`.
func TestEvolvingWorkspaceTeardown(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}
	setHermeticGit(t)
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
	oid := func(rev string) string {
		t.Helper()
		out, err := exec.Command("jj", "-R", ws, "log", "--no-graph", "-r", rev, "-T", "commit_id").Output()
		if err != nil {
			t.Fatalf("resolving %s: %v", rev, err)
		}
		return string(out)
	}

	run("git", "init", "-q", "-b", "main")
	run("git", "commit", "-q", "--allow-empty", "-m", "init")
	run("jj", "git", "init", "--colocate")
	run("jj", "config", "set", "--repo", `revset-aliases."trunk()"`, "main")
	write("first.txt", "first PR\n")
	run("jj", "commit", "-m", "first")
	run("jj", "bookmark", "create", "first", "-r", "@-")
	firstOID := oid("first")

	// GitHub squash-merges the first PR and deletes its branch. The commit is
	// still addressable by OID in jj, even though the bookmark name is gone.
	run("jj", "bookmark", "delete", "first")
	write("second.txt", "second PR\n")
	run("jj", "describe", "-m", "second")
	run("jj", "bookmark", "create", "second", "-r", "@")
	secondOID := oid("second")

	t.Setenv("GH_FAKE_STATE", "MERGED")
	t.Setenv("GH_FAKE_HEAD_OID", firstOID)
	t.Setenv("GH_FAKE_ALT_BRANCH", "second")
	t.Setenv("GH_FAKE_ALT_STATE", "MERGED")
	t.Setenv("GH_FAKE_ALT_HEAD_OID", secondOID)

	t.Run("merged PR at working copy is fully accounted", func(t *testing.T) {
		if reason := workspaceTeardownBlocker(ws, "o/r", "repo", []string{"first", "second"}, false, ""); reason != "" {
			t.Fatalf("expected both merged PR heads to clear, blocked by %q", reason)
		}
	})

	// Mirror the ordinary jj finish-up shape: the secondary PR is now @-'s
	// bookmark under a fresh empty working-copy commit.
	run("jj", "new")
	t.Setenv("GH_FAKE_ALT_STATE", "OPEN")

	t.Run("untracked secondary is actionable", func(t *testing.T) {
		reason := workspaceTeardownBlocker(ws, "o/r", "repo", []string{"first"}, false, "")
		want := "untracked branch second"
		if !strings.Contains(reason, want) || !strings.Contains(reason, "rig track second") {
			t.Fatalf("reason = %q, want %q and a rig track hint", reason, want)
		}
	})

	t.Run("tracked secondary reports its open PR", func(t *testing.T) {
		reason := workspaceTeardownBlocker(ws, "o/r", "repo", []string{"first", "second"}, false, "")
		if !strings.Contains(reason, "PR #8 (second) is open") {
			t.Fatalf("reason = %q, want the open secondary PR", reason)
		}
	})

	t.Run("merged PR under empty working copy is fully accounted", func(t *testing.T) {
		t.Setenv("GH_FAKE_ALT_STATE", "MERGED")
		if reason := workspaceTeardownBlocker(ws, "o/r", "repo", []string{"first", "second"}, false, ""); reason != "" {
			t.Fatalf("expected both merged PR heads to clear, blocked by %q", reason)
		}
	})

	t.Run("local edits beyond pushed PR still block", func(t *testing.T) {
		t.Setenv("GH_FAKE_ALT_STATE", "MERGED")
		write("second.txt", "second PR, plus local edits\n")
		reason := workspaceTeardownBlocker(ws, "o/r", "repo", []string{"first", "second"}, false, "")
		if !strings.Contains(reason, "changes beyond merged PR #8 (second)") {
			t.Fatalf("reason = %q, want changes beyond the merged PR", reason)
		}
	})
}
