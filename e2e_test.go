package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpDown exercises the full rig up → rig down cycle against a fake repo
// and a dedicated tmux server, so it doesn't touch the user's real tmux
// sessions or workspaces.
func TestUpDown(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}

	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	// Lives under ~/src/<host>/<owner>/<repo> so owner derivation (and thus
	// the manifest's [repos] table / GH_REPO wiring) has something to chew on.
	repoDir := filepath.Join(home, "src", "github.com", "fakeowner", "fakerepo")
	rigBin := filepath.Join(home, "rig")

	mustMkdir(t, bin)
	mustMkdir(t, repoDir)

	build := exec.Command("go", "build", "-o", rigBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	env := append(os.Environ(),
		"HOME="+home,
		"PATH="+bin+":"+os.Getenv("PATH"),
		// Avoid relying on the host's git/jj identity config.
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"JJ_USER=Test", "JJ_EMAIL=test@example.com",
	)

	// Stand up a colocated git+jj repo with a single commit and a main
	// bookmark, then alias trunk() to main so rig's start-rev fallback
	// resolves without an origin remote.
	mustRun(t, repoDir, env, "git", "init", "-q", "-b", "main")
	mustRun(t, repoDir, env, "git", "commit", "-q", "--allow-empty", "-m", "init")
	mustRun(t, repoDir, env, "jj", "git", "init", "--colocate")
	mustRun(t, repoDir, env, "jj", "config", "set", "--repo", `revset-aliases."trunk()"`, "main")

	// Fake linearis: always returns FAKE-1 → fake/fake-1-do-the-thing.
	linearis := `#!/bin/sh
if [ "$1" = "issues" ] && [ "$2" = "read" ]; then
  cat <<JSON
{"identifier":"FAKE-1","title":"do the thing","branchName":"fake/fake-1-do-the-thing"}
JSON
  exit 0
fi
echo "fake linearis: unsupported invocation $*" >&2
exit 1
`
	mustWriteExec(t, filepath.Join(bin, "linearis"), linearis)

	// tmux wrapper routes every call to a dedicated server socket so the
	// user's real tmux is untouched.
	tmuxWrap := fmt.Sprintf("#!/bin/sh\nexec %s -L rig-e2e \"$@\"\n", realTmux)
	mustWriteExec(t, filepath.Join(bin, "tmux"), tmuxWrap)

	// Fakes for the commands launched into the session panes. They just
	// hang so the panes stay open for our assertions; tmux would close a
	// pane whose command exits immediately.
	sleeper := "#!/bin/sh\nexec sleep infinity\n"
	mustWriteExec(t, filepath.Join(bin, "recto"), sleeper)
	mustWriteExec(t, filepath.Join(bin, "claude"), sleeper)

	t.Cleanup(func() {
		_ = exec.Command(realTmux, "-L", "rig-e2e", "kill-server").Run()
	})

	// --- rig up ---
	upCmd := exec.Command(rigBin, "up", "FAKE-1")
	upCmd.Dir = repoDir
	upCmd.Env = env
	if out, err := upCmd.CombinedOutput(); err != nil {
		t.Fatalf("rig up: %v\n%s", err, out)
	}

	basedir := filepath.Join(home, "workspaces", "fake-1-do-the-thing")
	wantFiles := []string{
		basedir,
		filepath.Join(basedir, ".rig.toml"),
		filepath.Join(basedir, ".envrc"),
		filepath.Join(basedir, "fakerepo", ".jj"),
		// direnv anchor written because the fake repo ships no .envrc of its own.
		filepath.Join(basedir, "fakerepo", ".envrc"),
	}
	for _, p := range wantFiles {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist after up: %v", p, err)
		}
	}

	manifest, err := os.ReadFile(filepath.Join(basedir, ".rig.toml"))
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	if !strings.Contains(string(manifest), `id    = "fake-1"`) {
		t.Errorf("manifest missing id:\n%s", manifest)
	}
	if !strings.Contains(string(manifest), `title = "do the thing"`) {
		t.Errorf("manifest missing title:\n%s", manifest)
	}
	// The [repos] table is what the global direnvrc reads to set GH_REPO.
	if !strings.Contains(string(manifest), `fakerepo = "fakeowner/fakerepo"`) {
		t.Errorf("manifest missing repos mapping:\n%s", manifest)
	}

	// Session named after the basedir, session-wizard full-path style.
	session := "~/workspaces/fake-1-do-the-thing"
	if err := exec.Command(realTmux, "-L", "rig-e2e", "has-session", "-t", session).Run(); err != nil {
		t.Errorf("expected tmux session %s: %v", session, err)
	}

	// Two panes: claude on the left, recto on the right.
	panes := mustOutput(t, "", env, realTmux, "-L", "rig-e2e", "list-panes", "-t", session+":0", "-F", "#{pane_current_command}")
	paneLines := strings.Split(strings.TrimSpace(panes), "\n")
	if len(paneLines) != 2 {
		t.Errorf("expected 2 panes after up, got %d:\n%s", len(paneLines), panes)
	}

	// Workspace registered on source repo.
	wsList := mustOutput(t, repoDir, env, "jj", "workspace", "list")
	if !strings.Contains(wsList, "fake-1-fakerepo") {
		t.Errorf("workspace fake-1-fakerepo not registered:\n%s", wsList)
	}

	// --- rig down --- run from inside basedir, the friendly path
	downCmd := exec.Command(rigBin, "down")
	downCmd.Dir = basedir
	downCmd.Env = env
	if out, err := downCmd.CombinedOutput(); err != nil {
		t.Fatalf("rig down: %v\n%s", err, out)
	}

	if _, err := os.Stat(basedir); err == nil {
		t.Errorf("basedir still exists after down")
	}
	if err := exec.Command(realTmux, "-L", "rig-e2e", "has-session", "-t", session).Run(); err == nil {
		t.Errorf("tmux session still exists after down")
	}
	wsList = mustOutput(t, repoDir, env, "jj", "workspace", "list")
	if strings.Contains(wsList, "fake-1-fakerepo") {
		t.Errorf("workspace not forgotten after down:\n%s", wsList)
	}
}

// TestReview exercises `rig review <pr-url>` end to end against a fake repo
// (already cloned under the ghq root) and fake gh/ghq, then checks `rig ls`
// reports the rig. It pre-creates the PR branch so the pull/N/head fetch is
// skipped (gitHasBranch short-circuits), keeping the test offline.
func TestReview(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}

	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	// ghq root == ~/src; the "already cloned" repo lives at the ghq path.
	repoDir := filepath.Join(home, "src", "github.com", "fakeowner", "fakerepo")
	rigBin := filepath.Join(home, "rig")

	mustMkdir(t, bin)
	mustMkdir(t, repoDir)

	build := exec.Command("go", "build", "-o", rigBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	env := append(os.Environ(),
		"HOME="+home,
		"PATH="+bin+":"+os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"JJ_USER=Test", "JJ_EMAIL=test@example.com",
	)

	// Source repo with a main commit and a separate pr-branch commit. The
	// branch standing in for the PR head must exist so `rig review` finds it.
	mustRun(t, repoDir, env, "git", "init", "-q", "-b", "main")
	mustRun(t, repoDir, env, "git", "commit", "-q", "--allow-empty", "-m", "init")
	mustRun(t, repoDir, env, "git", "checkout", "-q", "-b", "pr-branch")
	mustRun(t, repoDir, env, "git", "commit", "-q", "--allow-empty", "-m", "pr work")
	mustRun(t, repoDir, env, "git", "checkout", "-q", "main")

	// Fake gh: only `gh pr view N -R owner/repo --json headRefName,title`.
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  cat <<JSON
{"headRefName":"pr-branch","title":"fix the thing"}
JSON
  exit 0
fi
echo "fake gh: unsupported invocation $*" >&2
exit 1
`
	mustWriteExec(t, filepath.Join(bin, "gh"), ghScript)

	// Fake ghq: root prints ~/src; get is a no-op (repo already present).
	ghqScript := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"root\" ]; then echo %s; exit 0; fi\nexit 0\n", filepath.Join(home, "src"))
	mustWriteExec(t, filepath.Join(bin, "ghq"), ghqScript)

	tmuxWrap := fmt.Sprintf("#!/bin/sh\nexec %s -L rig-e2e-review \"$@\"\n", realTmux)
	mustWriteExec(t, filepath.Join(bin, "tmux"), tmuxWrap)

	sleeper := "#!/bin/sh\nexec sleep infinity\n"
	mustWriteExec(t, filepath.Join(bin, "recto"), sleeper)
	mustWriteExec(t, filepath.Join(bin, "claude"), sleeper)

	t.Cleanup(func() {
		_ = exec.Command(realTmux, "-L", "rig-e2e-review", "kill-server").Run()
	})

	// --- rig review <url> --- run from $HOME (not the repo); review resolves
	// the repo via ghq, not cwd.
	reviewCmd := exec.Command(rigBin, "review", "https://github.com/fakeowner/fakerepo/pull/42")
	reviewCmd.Dir = home
	reviewCmd.Env = env
	if out, err := reviewCmd.CombinedOutput(); err != nil {
		t.Fatalf("rig review: %v\n%s", err, out)
	}

	basedir := filepath.Join(home, "workspaces", "pr-42-fakerepo-pr-branch")
	for _, p := range []string{
		basedir,
		filepath.Join(basedir, ".rig.toml"),
		filepath.Join(basedir, "fakerepo", ".jj"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist after review: %v", p, err)
		}
	}

	manifest := string(mustReadFile(t, filepath.Join(basedir, ".rig.toml")))
	if !strings.Contains(manifest, `id    = "pr-42-fakerepo"`) {
		t.Errorf("manifest missing id:\n%s", manifest)
	}
	if !strings.Contains(manifest, `title = "fix the thing"`) {
		t.Errorf("manifest missing title:\n%s", manifest)
	}
	if !strings.Contains(manifest, `fakerepo = "fakeowner/fakerepo"`) {
		t.Errorf("manifest missing repos mapping:\n%s", manifest)
	}

	// Session named after the basedir, session-wizard full-path style.
	session := "~/workspaces/pr-42-fakerepo-pr-branch"
	if err := exec.Command(realTmux, "-L", "rig-e2e-review", "has-session", "-t", session).Run(); err != nil {
		t.Errorf("expected tmux session %s: %v", session, err)
	}

	// The workspace should sit on the PR branch's commit, not main's.
	desc := mustOutput(t, filepath.Join(basedir, "fakerepo"), env, "jj", "log", "-r", "@-", "--no-graph", "-T", "description")
	if !strings.Contains(desc, "pr work") {
		t.Errorf("workspace not on PR branch; @- description was:\n%s", desc)
	}

	// --- rig ls --- should report the rig on stdout.
	lsOut := mustOutput(t, home, env, rigBin, "ls")
	if !strings.Contains(lsOut, "pr-42-fakerepo") {
		t.Errorf("rig ls missing the review rig:\n%s", lsOut)
	}

	// --- rig down --- tidy up.
	downCmd := exec.Command(rigBin, "down")
	downCmd.Dir = basedir
	downCmd.Env = env
	if out, err := downCmd.CombinedOutput(); err != nil {
		t.Fatalf("rig down: %v\n%s", err, out)
	}
	if _, err := os.Stat(basedir); err == nil {
		t.Errorf("basedir still exists after down")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustRun(t *testing.T, dir string, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func mustOutput(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}
