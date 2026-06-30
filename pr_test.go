package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRepoSubdirForCwd(t *testing.T) {
	base := filepath.Join("/rigs", "mir-75")
	multi := manifest{Repos: map[string]string{"rig": "phinze/rig", "recto": "phinze/recto"}}
	single := manifest{Repos: map[string]string{"rig": "phinze/rig"}}

	cases := []struct {
		name    string
		cwd     string
		m       manifest
		want    string
		wantErr bool
	}{
		{"inside a repo subdir", filepath.Join(base, "recto"), multi, "recto", false},
		{"deep inside a repo subdir", filepath.Join(base, "rig", "internal", "x"), multi, "rig", false},
		{"basedir root, single repo", base, single, "rig", false},
		{"basedir root, multiple repos is ambiguous", base, multi, "", true},
		{"unknown subdir at root falls through to ambiguity", filepath.Join(base, "ghost"), multi, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := repoSubdirForCwd(base, c.cwd, c.m)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got subdir %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestJJPRBranch(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}
	// Hermetic identity so we don't depend on the host's jj/git config.
	t.Setenv("JJ_USER", "Test")
	t.Setenv("JJ_EMAIL", "test@example.com")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	// Colocated git+jj with a single commit on main, and trunk() aliased to
	// main so the `~ trunk()` filter resolves without an origin remote.
	run("git", "init", "-q", "-b", "main")
	run("git", "commit", "-q", "--allow-empty", "-m", "init")
	run("jj", "git", "init", "--colocate")
	run("jj", "config", "set", "--repo", `revset-aliases."trunk()"`, "main")

	// On trunk with no feature bookmark: nothing to open.
	got, err := jjPRBranch(repo)
	if err != nil {
		t.Fatalf("jjPRBranch (no branch): %v", err)
	}
	if got != "" {
		t.Errorf("expected empty branch on trunk, got %q", got)
	}

	// Put the work on a feature bookmark; that's the branch a PR rides.
	run("jj", "new", "-m", "wip")
	run("jj", "bookmark", "create", "phinze/mir-75-do-thing", "-r", "@")
	got, err = jjPRBranch(repo)
	if err != nil {
		t.Fatalf("jjPRBranch (with branch): %v", err)
	}
	if got != "phinze/mir-75-do-thing" {
		t.Errorf("got branch %q, want phinze/mir-75-do-thing", got)
	}
}
