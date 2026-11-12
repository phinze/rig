package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A workspace made before rig recorded starts gets the same answer back from
// jj's operation log, even after trunk moved on and the workspace was rebased
// onto it, which is exactly when the live fork point stops being the start.
func TestRecoverWorkspaceStartFromTheOpLog(t *testing.T) {
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
	run(source, "git", "init", "-q", "-b", "main")
	run(source, "git", "commit", "-q", "--allow-empty", "-m", "init")
	run(source, "jj", "git", "init", "--colocate")
	run(source, "jj", "config", "set", "--repo", `revset-aliases."trunk()"`, "main")
	began := run(source, "jj", "log", "--no-graph", "-r", "main", "-T", "commit_id")

	ws := filepath.Join(dir, "rig", "repo")
	if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := jjWorkspaceAdd(source, "rig-repo", "trunk()", ws); err != nil {
		t.Fatal(err)
	}
	if got, err := workspaceStart(ws); err != nil || got != began {
		t.Fatalf("workspaceStart = %q, %v; want %s", got, err, began)
	}

	// Trunk moves on, and the workspace follows it.
	run(source, "jj", "new", "main", "-m", "someone else's change")
	run(source, "jj", "bookmark", "set", "main", "-r", "@")
	run(ws, "jj", "rebase", "-d", "main")
	if now, _ := workspaceStart(ws); now == began {
		t.Fatal("the live fork point should have moved with the rebase")
	}

	got, err := recoverWorkspaceStart(ws)
	if err != nil {
		t.Fatal(err)
	}
	if got != began {
		t.Fatalf("recovered start = %s, want %s", got, began)
	}

	// A recorded start wins without asking jj.
	m := manifest{Starts: map[string]string{"repo": "recorded"}}
	if got, _ := repoStart(filepath.Join(dir, "rig"), m, "repo"); got != "recorded" {
		t.Fatalf("repoStart = %q, want the recorded one", got)
	}
}
