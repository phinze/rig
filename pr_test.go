package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestRigPRCandidatesIncludesTrackedAndCurrentBranchesAcrossRepos(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}
	setTestVCSIdentity(t)

	basedir := t.TempDir()
	initPRTestRepo(t, filepath.Join(basedir, "site"), "phinze/mir-766-preview")
	initPRTestRepo(t, filepath.Join(basedir, "runtime"), "phinze/mir-766-runtime")
	initPRTestRepo(t, filepath.Join(basedir, "brand"), "")
	m := manifest{
		Repos: map[string]string{
			"brand":   "o/brand",
			"runtime": "o/runtime",
			"site":    "o/site",
		},
		Branches: map[string][]string{
			"site": {"phinze/mir-766-original", "phinze/mir-766-copy-fix"},
		},
	}

	got, err := rigPRCandidates(basedir, m)
	if err != nil {
		t.Fatal(err)
	}
	want := []rigPRCandidate{
		{Repo: "o/runtime", Branch: "phinze/mir-766-runtime"},
		{Repo: "o/site", Branch: "phinze/mir-766-original"},
		{Repo: "o/site", Branch: "phinze/mir-766-copy-fix"},
		{Repo: "o/site", Branch: "phinze/mir-766-preview"},
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRunPRPicksAmongMatchingPRsInRig(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}
	setTestVCSIdentity(t)

	basedir := t.TempDir()
	initPRTestRepo(t, filepath.Join(basedir, "site"), "phinze/mir-766-preview")
	initPRTestRepo(t, filepath.Join(basedir, "runtime"), "phinze/mir-766-runtime")
	m := manifest{
		ID: "mir-766",
		Repos: map[string]string{
			"runtime": "o/runtime",
			"site":    "o/site",
		},
		Branches: map[string][]string{
			"site": {"phinze/mir-766-original"},
		},
	}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(t.TempDir(), "opened")
	binDir := t.TempDir()
	gh := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  case "$*" in
  *"--json"*)
    case "$3" in
    phinze/mir-766-original)
      echo 'no pull requests found for branch "phinze/mir-766-original"' >&2
      exit 1
      ;;
    phinze/mir-766-preview) number=104 ;;
    phinze/mir-766-runtime) number=105 ;;
    *) echo "unexpected branch $3" >&2; exit 1 ;;
    esac
    printf '{"number":%s,"state":"OPEN","url":"https://github.com/o/r/pull/%s","title":"PR %s","statusCheckRollup":[]}\n' "$number" "$number" "$number"
    exit 0
    ;;
  *"--web"*)
    printf '%s\n' "$*" >> "$RIG_PR_TEST_LOG"
    exit 0
    ;;
  esac
fi
echo "unsupported gh invocation: $*" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RIG_PR_TEST_LOG", logPath)

	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(basedir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })

	var pickerRows []string
	picker := func(rows []string, prompt string) (string, error) {
		pickerRows = rows
		if prompt != "Open PR: " {
			t.Fatalf("prompt = %q, want Open PR", prompt)
		}
		return rows[1], nil // choose site #104 over runtime #105
	}
	if err := runPRWithPicker(nil, picker); err != nil {
		t.Fatal(err)
	}
	wantRows := []string{
		"o/runtime\t#105\tPR 105\t0",
		"o/site\t#104\tPR 104\t1",
	}
	if len(pickerRows) != len(wantRows) {
		t.Fatalf("picker rows = %q, want %q", pickerRows, wantRows)
	}
	for i := range wantRows {
		if pickerRows[i] != wantRows[i] {
			t.Fatalf("pickerRows[%d] = %q, want %q", i, pickerRows[i], wantRows[i])
		}
	}
	opened, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(opened))
	if want := "pr view 104 -R o/site --web"; line != want {
		t.Fatalf("opened = %q, want %q", line, want)
	}
}

func setTestVCSIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("JJ_USER", "Test")
	t.Setenv("JJ_EMAIL", "test@example.com")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

func initPRTestRepo(t *testing.T, repo, branch string) {
	t.Helper()
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main")
	run("git", "commit", "-q", "--allow-empty", "-m", "init")
	run("jj", "git", "init", "--colocate")
	run("jj", "config", "set", "--repo", `revset-aliases."trunk()"`, "main")
	if branch != "" {
		run("jj", "new", "-m", "work")
		run("jj", "bookmark", "create", branch, "-r", "@")
	}
}
