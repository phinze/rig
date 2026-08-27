package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveRectoRepo(t *testing.T) {
	m := manifest{Repos: map[string]string{
		"runtime": "mirendev/runtime",
		"brand":   "mirendev/brand",
	}}
	for _, arg := range []string{"runtime", "mirendev/runtime"} {
		got, err := resolveRectoRepo(m, arg)
		if err != nil || got != "runtime" {
			t.Errorf("resolveRectoRepo(%q) = %q, %v; want runtime", arg, got, err)
		}
	}
	if _, err := resolveRectoRepo(m, "cloud"); err == nil {
		t.Error("missing repo resolved without error")
	}
}

func TestRectoCarouselPreservesAdHocShell(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	basedir := filepath.Join(home, "workspaces", "mir-1")
	mustMkdir(t, bin)
	for _, repo := range []string{"runtime", "cloud", "brand"} {
		mustMkdir(t, filepath.Join(basedir, repo))
	}
	tmuxWrap := fmt.Sprintf("#!/bin/sh\nexec %s -L rig-recto-e2e -f /dev/null \"$@\"\n", realTmux)
	mustWriteExec(t, filepath.Join(bin, "tmux"), tmuxWrap)
	mustWriteExec(t, filepath.Join(bin, "recto"), "#!/bin/sh\n"+
		"if [ \"$1\" = -R ]; then printf '%s\\n' \"$*\" > \"$HOME/recto.args\"; exit \"${RECTO_EXIT:-0}\"; fi\n"+
		"if [ \"$#\" -ne 0 ]; then printf 'unexpected recto args: %s\\n' \"$*\" >&2; exit 2; fi\n"+
		"exec sleep infinity\n")
	mustWriteExec(t, filepath.Join(bin, "claude"), "#!/bin/sh\nexec sleep infinity\n")
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HISTFILE", "/dev/null")
	t.Cleanup(func() { _ = exec.Command(realTmux, "-L", "rig-recto-e2e", "kill-server").Run() })

	m := manifest{ID: "mir-1", Repos: map[string]string{
		"runtime": "mirendev/runtime",
		"cloud":   "mirendev/cloud",
		"brand":   "mirendev/brand",
	}}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}
	session, err := spawnSession(basedir, filepath.Join(basedir, "runtime"), sessionSpec{
		rectoCmd: rectoCommand(), repo: "runtime", agent: agentClaude, prompt: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"cloud", "brand"} {
		pane, window, err := tmuxNewCommandWindow(session, repo, filepath.Join(basedir, repo), rectoCommand())
		if err != nil {
			t.Fatal(err)
		}
		if err := markRigPane(pane, rigPaneRecto, repo); err != nil {
			t.Fatal(err)
		}
		if err := markRigRepoWindow(window, repo); err != nil {
			t.Fatal(err)
		}
	}

	panes, err := tmuxRigPanes(session)
	if err != nil {
		t.Fatal(err)
	}
	cloudRecto, ok := findRectoPane(panes, "cloud")
	if !ok {
		t.Fatal("cloud recto missing")
	}
	// A normal tmux split from the full-screen Recto creates an ad hoc shell in
	// the same repo window. It must survive when cloud is promoted.
	if _, err := tmuxSplitHID(cloudRecto.PaneID, filepath.Join(basedir, "cloud"), "sleep infinity"); err != nil {
		t.Fatal(err)
	}
	if err := promoteRecto(session, basedir, "cloud", m); err != nil {
		t.Fatal(err)
	}

	panes, err = tmuxRigPanes(session)
	if err != nil {
		t.Fatal(err)
	}
	assertMainRecto(t, panes, "cloud")
	cloudWindow, ok := findRepoWindow(panes, "cloud", "")
	if !ok || cloudWindow.PaneRole != "" {
		t.Fatalf("cloud shell window did not survive promotion: %+v", cloudWindow)
	}
	if _, ok := findRepoWindow(panes, "runtime", ""); !ok {
		t.Fatal("outgoing runtime recto was not parked")
	}

	// Coming back to runtime parks cloud beside its surviving shell, while the
	// Recto-only runtime parking window collapses as its pane joins main.
	if err := promoteRecto(session, basedir, "runtime", m); err != nil {
		t.Fatal(err)
	}
	panes, err = tmuxRigPanes(session)
	if err != nil {
		t.Fatal(err)
	}
	assertMainRecto(t, panes, "runtime")
	cloudPanes := 0
	for _, p := range panes {
		if p.WindowRole == rigWindowRepo && p.WindowRepo == "cloud" {
			cloudPanes++
		}
		if p.WindowRole == rigWindowRepo && p.WindowRepo == "runtime" {
			t.Errorf("runtime parking window survived after its only pane was promoted: %+v", p)
		}
	}
	if cloudPanes != 2 {
		t.Errorf("cloud window has %d panes, want shell + parked recto", cloudPanes)
	}

	// The public command combines promotion with a repo-scoped Recto command;
	// this is the only carousel contract the companion skill needs to learn.
	t.Chdir(basedir)
	if err := runRecto([]string{"brand", "focus", "src/app.go:42"}); err != nil {
		t.Fatal(err)
	}
	args := string(mustReadFile(t, filepath.Join(home, "recto.args")))
	wantArgs := "-R " + filepath.Join(basedir, "brand") + " focus src/app.go:42\n"
	if args != wantArgs {
		t.Errorf("delegated recto args = %q, want %q", args, wantArgs)
	}
	panes, err = tmuxRigPanes(session)
	if err != nil {
		t.Fatal(err)
	}
	assertMainRecto(t, panes, "brand")

	t.Setenv("RECTO_EXIT", "2")
	err = runRecto([]string{"brand", "ping"})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Errorf("delegated exit = %v, want Recto exit 2", err)
	}
}

func assertMainRecto(t *testing.T, panes []rigTmuxPane, repo string) {
	t.Helper()
	main, _, recto, err := findMainParts(panes)
	if err != nil {
		t.Fatal(err)
	}
	if main.WindowName != mainWindowName(repo) || recto.PaneRepo != repo {
		t.Errorf("main = %q with recto %q, want %q", main.WindowName, recto.PaneRepo, mainWindowName(repo))
	}
}
