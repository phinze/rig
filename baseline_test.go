package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A baseline moves to where the workspace forks off trunk now, or to a named
// commit on trunk, and refuses one off trunk, which the window couldn't
// anchor at.
func TestSetBaseline(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}
	setHermeticGit(t)

	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(in string, args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = in
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	id := func(in, rev string) string {
		return run(in, "jj", "log", "--no-graph", "-r", rev, "-T", "commit_id")
	}
	run(source, "git", "init", "-q", "-b", "main")
	run(source, "git", "commit", "-q", "--allow-empty", "-m", "init")
	run(source, "jj", "git", "init", "--colocate")
	run(source, "jj", "config", "set", "--repo", `revset-aliases."trunk()"`, "main")
	began := id(source, "main")

	basedir := filepath.Join(dir, "rig")
	ws := filepath.Join(basedir, "repo")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := jjWorkspaceAdd(source, "rig-repo", "trunk()", ws); err != nil {
		t.Fatal(err)
	}

	// Other work lands on trunk, the workspace moves up onto it, and has a
	// change of its own that isn't on trunk.
	run(source, "jj", "new", "main", "-m", "landed elsewhere")
	run(source, "jj", "bookmark", "set", "main", "-r", "@")
	landed := id(source, "main")
	run(ws, "jj", "rebase", "-d", "main")
	run(ws, "jj", "describe", "-m", "mine, unpushed")
	run(ws, "jj", "new")
	mine := id(ws, "@-")

	m := manifest{Repos: map[string]string{"repo": "o/repo"}, Starts: map[string]string{"repo": began}}
	old, now, err := setBaseline(basedir, &m, "repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if old != began || now != landed || m.Starts["repo"] != landed {
		t.Fatalf("baseline moved %s → %s (recorded %s); want %s → %s", old, now, m.Starts["repo"], began, landed)
	}

	if _, now, err := setBaseline(basedir, &m, "repo", began); err != nil || now != began {
		t.Fatalf("--at the old start = %s, %v; want %s", now, err, began)
	}
	if _, _, err := setBaseline(basedir, &m, "repo", mine); err == nil || !strings.Contains(err.Error(), "isn't on trunk") {
		t.Fatalf("a commit off trunk should be refused, got %v", err)
	}
	if m.Starts["repo"] != began {
		t.Fatalf("a refused baseline changed the start to %s", m.Starts["repo"])
	}
	if _, _, err := setBaseline(basedir, &m, "nope", ""); err == nil {
		t.Fatal("an unknown repo should be refused")
	}
}
