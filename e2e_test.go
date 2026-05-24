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
	repoDir := filepath.Join(home, "src", "fakerepo")
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

	if err := exec.Command(realTmux, "-L", "rig-e2e", "has-session", "-t", "rig-fake-1").Run(); err != nil {
		t.Errorf("expected tmux session rig-fake-1: %v", err)
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
	if err := exec.Command(realTmux, "-L", "rig-e2e", "has-session", "-t", "rig-fake-1").Run(); err == nil {
		t.Errorf("tmux session still exists after down")
	}
	wsList = mustOutput(t, repoDir, env, "jj", "workspace", "list")
	if strings.Contains(wsList, "fake-1-fakerepo") {
		t.Errorf("workspace not forgotten after down:\n%s", wsList)
	}
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
