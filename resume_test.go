package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureRigRuntimeUpgradesLegacyBareSession(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	basedir := filepath.Join(home, "workspaces", "legacy-rig")
	marker := filepath.Join(home, "claude.args")
	mustMkdir(t, bin)
	for _, repo := range []string{"zeta", "alpha"} {
		mustMkdir(t, filepath.Join(basedir, repo))
	}

	socket := "rig-resume-legacy-e2e"
	tmuxWrap := fmt.Sprintf("#!/bin/sh\nexec %s -L %s -f /dev/null \"$@\"\n", realTmux, socket)
	mustWriteExec(t, filepath.Join(bin, "tmux"), tmuxWrap)
	mustWriteExec(t, filepath.Join(bin, "recto"), "#!/bin/sh\nexec sleep infinity\n")
	mustWriteExec(t, filepath.Join(bin, "claude"), fmt.Sprintf(
		"#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\nexec sleep infinity\n", shellQuote(marker)))
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HISTFILE", "/dev/null")
	t.Cleanup(func() { _ = exec.Command(realTmux, "-L", socket, "kill-server").Run() })

	m := manifest{
		ID: "legacy-rig", SessionID: "legacy-session",
		Repos: map[string]string{"zeta": "o/zeta", "alpha": "o/alpha"},
	}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}
	session := tmuxSessionName(basedir)
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", session, "-c", basedir).CombinedOutput(); err != nil {
		t.Fatalf("starting legacy bare session: %v\n%s", err, out)
	}

	if _, err := ensureRigRuntime(basedir, m); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "legacy agent resume", func() bool {
		raw, _ := os.ReadFile(marker)
		return strings.Contains(string(raw), "--resume legacy-session")
	})

	out, err := exec.Command("tmux", "list-panes", "-s", "-t", session, "-F",
		"#{window_name}\t#{pane_current_path}\t#{@rig-window-role}\t#{@rig-window-repo}\t#{@rig-pane-role}\t#{@rig-pane-repo}").CombinedOutput()
	if err != nil {
		t.Fatalf("reading upgraded layout: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("upgraded layout has %d panes, want main pair + background Recto:\n%s", len(lines), out)
	}
	mainPanes := 0
	background := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "main/alpha\t"+filepath.Join(basedir, "alpha")+"\tmain\talpha\t") && strings.HasSuffix(line, "\talpha"):
			mainPanes++
		case line == "zeta\t"+filepath.Join(basedir, "zeta")+"\trepo\tzeta\trecto\tzeta":
			background = true
		}
	}
	if mainPanes != 2 || !background {
		t.Errorf("legacy session did not become the canonical sorted-first carousel:\n%s", out)
	}
}
