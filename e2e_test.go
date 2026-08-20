package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSpawnSessionAgents(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	mustMkdir(t, bin)
	tmuxWrap := fmt.Sprintf("#!/bin/sh\nexec %s -L rig-agent-e2e -f /dev/null \"$@\"\n", realTmux)
	mustWriteExec(t, filepath.Join(bin, "tmux"), tmuxWrap)
	mustWriteExec(t, filepath.Join(bin, "recto"), "#!/bin/sh\nwhile :; do sleep 60; done\n")
	for _, name := range []string{"claude", "codex", "agy"} {
		marker := filepath.Join(home, name+".args")
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" > %s\nwhile :; do sleep 60; done\n", shellQuote(marker))
		mustWriteExec(t, filepath.Join(bin, name), script)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HISTFILE", "/dev/null")
	t.Cleanup(func() { _ = exec.Command(realTmux, "-L", "rig-agent-e2e", "kill-server").Run() })

	cases := []struct {
		agent  agentKind
		binary string
		flag   string
	}{
		{agentClaude, "claude", "--dangerously-skip-permissions"},
		{agentCodex, "codex", "--dangerously-bypass-approvals-and-sandbox"},
		{agentAntigravity, "agy", "--prompt-interactive"},
	}
	for _, c := range cases {
		cwd := filepath.Join(home, string(c.agent), "repo")
		mustMkdir(t, cwd)
		session, err := spawnSession(filepath.Dir(cwd), cwd, sessionSpec{
			rectoCmd: "recto", agent: c.agent, prompt: "test prompt",
		})
		if err != nil {
			t.Fatalf("spawn %s: %v", c.agent, err)
		}
		marker := filepath.Join(home, c.binary+".args")
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil || !time.Now().Before(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		raw, err := os.ReadFile(marker)
		if err != nil {
			t.Fatalf("%s did not launch in %s: %v", c.agent, session, err)
		}
		args := string(raw)
		if !strings.Contains(args, c.flag) || !strings.Contains(args, "test prompt") {
			t.Errorf("%s args = %q, want %s and prompt", c.agent, args, c.flag)
		}
	}
}

// TestNew exercises ticketless creation without providing a linearis shim: the
// kickoff mints the local identity, the explicit repo takes the same clone-
// resolution path as up's picker result, and the workspace deliberately starts
// branchless at trunk().
func TestNew(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}

	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	repoDir := filepath.Join(home, "src", "github.com", "fakeowner", "fakerepo")
	rigBin := filepath.Join(home, "rig")
	marker := filepath.Join(home, "codex.args")
	mustMkdir(t, bin)
	mustMkdir(t, repoDir)
	// A ~/.codex is how rig knows codex is set up on this machine and worth
	// seeding directory trust for.
	mustMkdir(t, filepath.Join(home, ".codex"))

	build := exec.Command("go", "build", "-o", rigBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	env := append(os.Environ(),
		"HOME="+home,
		"PATH="+bin+":"+os.Getenv("PATH"),
		"SHELL=/bin/sh",
		"HISTFILE=/dev/null",
	)
	env = append(env, hermeticGitVars()...)
	mustRun(t, repoDir, env, "git", "init", "-q", "-b", "main")
	mustRun(t, repoDir, env, "git", "commit", "-q", "--allow-empty", "-m", "init")
	mustRun(t, repoDir, env, "jj", "git", "init", "--colocate")
	mustRun(t, repoDir, env, "jj", "config", "set", "--repo", `revset-aliases."trunk()"`, "main")

	ghq := "#!/bin/sh\n" +
		"if [ \"$1\" = \"root\" ]; then echo \"$HOME/src\"; exit 0; fi\n" +
		"echo \"fake ghq: unsupported invocation $*\" >&2\nexit 1\n"
	mustWriteExec(t, filepath.Join(bin, "ghq"), ghq)
	tmuxWrap := fmt.Sprintf("#!/bin/sh\nexec %s -L rig-new-e2e -f /dev/null \"$@\"\n", realTmux)
	mustWriteExec(t, filepath.Join(bin, "tmux"), tmuxWrap)
	mustWriteExec(t, filepath.Join(bin, "recto"), "#!/bin/sh\nexec sleep infinity\n")
	codex := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" > %s\nexec sleep infinity\n", shellQuote(marker))
	mustWriteExec(t, filepath.Join(bin, "codex"), codex)
	t.Cleanup(func() { _ = exec.Command(realTmux, "-L", "rig-new-e2e", "kill-server").Run() })

	kickoff := "Investigate flaky radar refresh"
	cmd := exec.Command(rigBin, "new", kickoff, "--repo", "fakeowner/fakerepo", "--agent", "codex")
	cmd.Dir = home
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rig new: %v\n%s", err, out)
	}

	rigID := "investigate-flaky-radar-refresh"
	basedir := filepath.Join(home, "workspaces", rigID)
	raw := string(mustReadFile(t, filepath.Join(basedir, ".rig.toml")))
	for _, want := range []string{
		`id    = "investigate-flaky-radar-refresh"`,
		`title = "Investigate flaky radar refresh"`,
		`agent = "codex"`,
		`fakerepo = "fakeowner/fakerepo"`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("manifest missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "[branches]") {
		t.Errorf("ticketless rig should start with no recorded branch:\n%s", raw)
	}

	// Both directories an agent or ad hoc shell can start in are pre-trusted, so
	// codex opens straight into the session instead of stopping to ask.
	codexCfg := string(mustReadFile(t, filepath.Join(home, ".codex", "config.toml")))
	for _, dir := range []string{basedir, filepath.Join(basedir, "fakerepo")} {
		if !strings.Contains(codexCfg, "[projects."+strconv.Quote(dir)+"]") {
			t.Errorf("codex config missing trust for %s:\n%s", dir, codexCfg)
		}
	}

	desc := mustOutput(t, filepath.Join(basedir, "fakerepo"), env,
		"jj", "log", "-r", "@-", "--no-graph", "-T", "description")
	if strings.TrimSpace(desc) != "init" {
		t.Errorf("workspace parent description = %q, want trunk commit %q", strings.TrimSpace(desc), "init")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	agentArgs := string(mustReadFile(t, marker))
	if !strings.Contains(agentArgs, "This is your kickoff for a new rig: "+kickoff) ||
		!strings.Contains(agentArgs, "there's no ticket to read") {
		t.Errorf("agent did not receive the ticketless kickoff prompt:\n%s", agentArgs)
	}

	// The kickoff-only rig gets no KICKOFF.md, and nothing in its generated
	// instructions should claim otherwise.
	if _, err := os.Stat(filepath.Join(basedir, "KICKOFF.md")); !os.IsNotExist(err) {
		t.Errorf("kickoff-only rig grew a KICKOFF.md (stat err = %v)", err)
	}
	if got := string(mustReadFile(t, filepath.Join(basedir, "CLAUDE.md"))); strings.Contains(got, "KICKOFF.md") {
		t.Errorf("instructions point at an absent brief:\n%s", got)
	}

	// Piped stdin is the non-interactive shape of the context step: the blob
	// lands in KICKOFF.md and the agent is pointed at it rather than handed it.
	if err := os.Remove(marker); err != nil {
		t.Fatalf("clearing agent marker: %v", err)
	}
	blob := "jim: radar hangs on enter again\nme: only after a sweep?\njim: yeah, every time"
	withContext := exec.Command(rigBin, "new", "Fix radar enter hang", "--repo", "fakeowner/fakerepo", "--agent", "codex")
	withContext.Dir = home
	withContext.Env = env
	withContext.Stdin = strings.NewReader(blob)
	if out, err := withContext.CombinedOutput(); err != nil {
		t.Fatalf("rig new with piped context: %v\n%s", err, out)
	}

	ctxBase := filepath.Join(home, "workspaces", "fix-radar-enter-hang")
	if got := string(mustReadFile(t, filepath.Join(ctxBase, "KICKOFF.md"))); got != "# Kickoff: Fix radar enter hang\n\n"+blob+"\n" {
		t.Errorf("KICKOFF.md =\n%s", got)
	}
	if got := string(mustReadFile(t, filepath.Join(ctxBase, "AGENTS.md"))); !strings.Contains(got, "The brief lives in `KICKOFF.md`") {
		t.Errorf("instructions missing the brief pointer:\n%s", got)
	}

	deadline = time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := string(mustReadFile(t, marker)); !strings.Contains(got, "../KICKOFF.md") {
		t.Errorf("agent was not pointed at the pasted context:\n%s", got)
	}

	// A repeated kickoff should point back to the existing rig instead of
	// silently manufacturing another local identity.
	again := exec.Command(rigBin, "new", kickoff, "--repo", "fakeowner/fakerepo")
	again.Dir = home
	again.Env = env
	if out, err := again.CombinedOutput(); err == nil {
		t.Fatalf("repeated rig new unexpectedly succeeded:\n%s", out)
	} else if !strings.Contains(string(out), "rig switch "+rigID) {
		t.Errorf("collision did not offer the existing rig:\n%s", out)
	}
}

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
	)
	env = append(env, hermeticGitVars()...)

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

	// Fake gh: this rig's branch was never pushed, so it has no PR. Answering
	// "no pull requests found" lets `rig down`'s guardrail see nothing unmerged
	// and tear the fresh rig down (the guardrail's pass-with-no-PR path).
	ghNoPR := "#!/bin/sh\n" +
		"if [ \"$1\" = \"pr\" ] && [ \"$2\" = \"view\" ]; then\n" +
		"  echo 'no pull requests found for branch' >&2\n  exit 1\nfi\n" +
		"echo \"fake gh: unsupported invocation $*\" >&2\nexit 1\n"
	mustWriteExec(t, filepath.Join(bin, "gh"), ghNoPR)

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
		filepath.Join(basedir, ".rig", "bin", "gh"),
		// Agent-facing breadcrumbs, rendered from the manifest.
		filepath.Join(basedir, "CLAUDE.md"),
		filepath.Join(basedir, "AGENTS.md"),
		filepath.Join(basedir, ".agents", "rules", "rig.md"),
		filepath.Join(basedir, "fakerepo", ".jj"),
	}
	for _, p := range wantFiles {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist after up: %v", p, err)
		}
	}
	// Repos without their own .envrc inherit the basedir's. Rig shouldn't add
	// generated files to the jj working copy just to trigger direnv.
	repoEnvrc := filepath.Join(basedir, "fakerepo", ".envrc")
	if _, err := os.Stat(repoEnvrc); !os.IsNotExist(err) {
		t.Errorf("expected rig not to create %s, got %v", repoEnvrc, err)
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
	if !strings.Contains(string(manifest), `created = "`) {
		t.Errorf("manifest missing created timestamp:\n%s", manifest)
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

	// --- rig up again (idempotent) --- naming a task whose rig already exists
	// switches to it instead of erroring on the basedir. Run from $HOME, not the
	// repo: it must short-circuit before any repo/tracker resolution (there's no
	// ghq stub here, so a fall-through to the create path would error on `ghq
	// list`), proving the existence check is both local and repo-independent.
	reUp := exec.Command(rigBin, "up", "FAKE-1")
	reUp.Dir = home
	reUp.Env = env
	if out, err := reUp.CombinedOutput(); err != nil {
		t.Fatalf("second rig up (idempotent): %v\n%s", err, out)
	} else if !strings.Contains(string(out), "already up") {
		t.Errorf("expected second up to switch to the existing rig, got:\n%s", out)
	}
	// Still exactly one rig on disk — no duplicate basedir was minted.
	if entries, err := os.ReadDir(filepath.Join(home, "workspaces")); err != nil {
		t.Fatalf("reading workspaces: %v", err)
	} else if len(entries) != 1 {
		t.Errorf("expected 1 rig after re-up, got %d", len(entries))
	}

	// --- rig down --- run from inside basedir, the friendly path
	downCmd := exec.Command(rigBin, "down")
	downCmd.Dir = basedir
	downCmd.Env = env
	if out, err := downCmd.CombinedOutput(); err != nil {
		t.Fatalf("rig down: %v\n%s", err, out)
	}
	waitFor(t, 10*time.Second, "rig down to finish", func() bool {
		_, statErr := os.Stat(basedir)
		sessionGone := exec.Command(realTmux, "-L", "rig-e2e", "has-session", "-t", "="+session).Run() != nil
		return os.IsNotExist(statErr) && sessionGone
	})

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
		"SHELL=/bin/sh",
		"HISTFILE=/dev/null",
	)
	env = append(env, hermeticGitVars()...)

	// Source repo with a main commit and a separate pr-branch commit. The
	// branch standing in for the PR head must exist so `rig review` finds it.
	mustRun(t, repoDir, env, "git", "init", "-q", "-b", "main")
	mustRun(t, repoDir, env, "git", "commit", "-q", "--allow-empty", "-m", "init")
	mustRun(t, repoDir, env, "git", "checkout", "-q", "-b", "pr-branch")
	mustRun(t, repoDir, env, "git", "commit", "-q", "--allow-empty", "-m", "pr work")
	mustRun(t, repoDir, env, "git", "checkout", "-q", "main")

	// Fake gh: `gh pr view` reports the head branch, title, and an author who
	// is NOT the current user, and `gh api user` names that current user — so
	// the authorship split routes this pickup to review, not authoring.
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  cat <<JSON
{"headRefName":"pr-branch","title":"fix the thing","author":{"login":"someoneelse"}}
JSON
  exit 0
fi
if [ "$1" = "api" ] && [ "$2" = "user" ]; then
  echo "testuser"
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

	// Basedir slug derives from the PR title, not the branch name.
	basedir := filepath.Join(home, "workspaces", "pr-42-fix-the-thing")
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
	if !strings.Contains(manifest, `id    = "pr-42"`) {
		t.Errorf("manifest missing id:\n%s", manifest)
	}
	if !strings.Contains(manifest, `title = "fix the thing"`) {
		t.Errorf("manifest missing title:\n%s", manifest)
	}
	if !strings.Contains(manifest, `fakerepo = "fakeowner/fakerepo"`) {
		t.Errorf("manifest missing repos mapping:\n%s", manifest)
	}

	// Session named after the basedir, session-wizard full-path style.
	session := "~/workspaces/pr-42-fix-the-thing"
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
	if !strings.Contains(lsOut, "pr-42") || !strings.Contains(lsOut, "fix the thing") {
		t.Errorf("rig ls missing the review rig:\n%s", lsOut)
	}

	prURL := "https://github.com/fakeowner/fakerepo/pull/42"

	// Repeating review is a local resume, not an attempt to recreate the
	// basedir. An active rig switches; a parked one wakes at the same cwd.
	reviewAgain := mustOutput(t, home, env, rigBin, "review", prURL)
	if !strings.Contains(reviewAgain, "pr-42 already up") {
		t.Errorf("repeated review should resume the active rig:\n%s", reviewAgain)
	}

	mustOutput(t, basedir, env, rigBin, "park")
	wakeURL := mustOutput(t, home, env, rigBin, "wake", prURL)
	if !strings.Contains(wakeURL, "woke pr-42") {
		t.Errorf("wake by PR URL should wake the review rig:\n%s", wakeURL)
	}
	if m := string(mustReadFile(t, filepath.Join(basedir, ".rig.toml"))); strings.Contains(m, "parked = \"") {
		t.Errorf("manifest still parked after wake by URL:\n%s", m)
	}

	// Explicit wake is idempotent too: an already-awake URL switches rather
	// than failing just because the parked set is empty.
	wakeAgain := mustOutput(t, home, env, rigBin, "wake", prURL)
	if !strings.Contains(wakeAgain, "pr-42 already up") {
		t.Errorf("repeated wake should resume the active rig:\n%s", wakeAgain)
	}

	mustOutput(t, basedir, env, rigBin, "park")
	reviewParked := mustOutput(t, home, env, rigBin, "review", prURL)
	if !strings.Contains(reviewParked, "woke pr-42") {
		t.Errorf("repeated review should wake the parked rig:\n%s", reviewParked)
	}

	// --- rig down --force --- a review rig wraps someone else's still-open PR,
	// which the merge guardrail would (correctly) refuse to tear down; --force
	// is the reviewer's honest teardown, and exercises the override path.
	downCmd := exec.Command(rigBin, "down", "--force")
	downCmd.Dir = basedir
	downCmd.Env = env
	if out, err := downCmd.CombinedOutput(); err != nil {
		t.Fatalf("rig down --force: %v\n%s", err, out)
	}
	waitFor(t, 10*time.Second, "review rig down to finish", func() bool {
		_, err := os.Stat(basedir)
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(basedir); err == nil {
		t.Errorf("basedir still exists after down")
	}
}

// TestUpFromOwnPR exercises `rig up <pr-url>` where the PR is the current
// user's: the authorship split routes it to authoring, not review. The PR rides
// a Linear-style branch, so the rig rebuilds under that issue's id and path
// (mir-75, not pr-42) — the property that lets claude --resume find the sessions
// you built under `rig up MIR-75`. gh reports the author as the current user, so
// the rig comes out authoring (no kind=review) on the branch you'd keep pushing.
func TestUpFromOwnPR(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}

	home := t.TempDir()
	bin := filepath.Join(home, "bin")
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
	)
	env = append(env, hermeticGitVars()...)

	// Source repo with a main commit and a Linear-style PR branch. The branch
	// carries the issue id (mir-75), which is what the pickup keys identity off.
	// resolveStartRev prefers branch@origin but falls back to the local branch,
	// which is what it finds here (no origin remote).
	mustRun(t, repoDir, env, "git", "init", "-q", "-b", "main")
	mustRun(t, repoDir, env, "git", "commit", "-q", "--allow-empty", "-m", "init")
	mustRun(t, repoDir, env, "git", "checkout", "-q", "-b", "phinze/mir-75-fix-the-thing")
	mustRun(t, repoDir, env, "git", "commit", "-q", "--allow-empty", "-m", "pr work")
	mustRun(t, repoDir, env, "git", "checkout", "-q", "main")

	// Fake gh: the PR's author IS the current user, so authorship routes to
	// authoring. `gh api user` names that same login.
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  cat <<JSON
{"headRefName":"phinze/mir-75-fix-the-thing","title":"fix the thing","author":{"login":"testuser"}}
JSON
  exit 0
fi
if [ "$1" = "api" ] && [ "$2" = "user" ]; then
  echo "testuser"
  exit 0
fi
echo "fake gh: unsupported invocation $*" >&2
exit 1
`
	mustWriteExec(t, filepath.Join(bin, "gh"), ghScript)

	ghqScript := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"root\" ]; then echo %s; exit 0; fi\nexit 0\n", filepath.Join(home, "src"))
	mustWriteExec(t, filepath.Join(bin, "ghq"), ghqScript)

	tmuxWrap := fmt.Sprintf("#!/bin/sh\nexec %s -L rig-e2e-up-pr \"$@\"\n", realTmux)
	mustWriteExec(t, filepath.Join(bin, "tmux"), tmuxWrap)

	sleeper := "#!/bin/sh\nexec sleep infinity\n"
	mustWriteExec(t, filepath.Join(bin, "recto"), sleeper)
	mustWriteExec(t, filepath.Join(bin, "claude"), sleeper)

	t.Cleanup(func() {
		_ = exec.Command(realTmux, "-L", "rig-e2e-up-pr", "kill-server").Run()
	})

	// --- rig up <url> --- run from $HOME; up resolves the repo from the PR.
	upCmd := exec.Command(rigBin, "up", "https://github.com/fakeowner/fakerepo/pull/42")
	upCmd.Dir = home
	upCmd.Env = env
	if out, err := upCmd.CombinedOutput(); err != nil {
		t.Fatalf("rig up <own-pr>: %v\n%s", err, out)
	}

	// Identity comes from the branch, not the PR number: the rig rebuilds at the
	// issue's path (mir-75-fix-the-thing), where `rig up MIR-75` would have built
	// it, so claude --resume can find the earlier sessions.
	basedir := filepath.Join(home, "workspaces", "mir-75-fix-the-thing")
	manifest := string(mustReadFile(t, filepath.Join(basedir, ".rig.toml")))
	if !strings.Contains(manifest, `id    = "mir-75"`) {
		t.Errorf("manifest id should be the issue id, not pr-42:\n%s", manifest)
	}
	// The whole point: this is authoring, not review, so kind must be unset.
	if strings.Contains(manifest, `kind  = "review"`) {
		t.Errorf("own-PR pickup should be authoring, but manifest is kind=review:\n%s", manifest)
	}
	// The branch is recorded so pr/ls/reap resolve this rig's own PR.
	if !strings.Contains(manifest, `fakerepo = ["phinze/mir-75-fix-the-thing"]`) {
		t.Errorf("manifest missing branch record:\n%s", manifest)
	}

	// Workspace sits on the PR branch commit, ready to keep pushing.
	desc := mustOutput(t, filepath.Join(basedir, "fakerepo"), env, "jj", "log", "-r", "@-", "--no-graph", "-T", "description")
	if !strings.Contains(desc, "pr work") {
		t.Errorf("workspace not on PR branch; @- description was:\n%s", desc)
	}

	// Idempotent: a second `rig up` on the same PR switches instead of erroring.
	reUp := exec.Command(rigBin, "up", "https://github.com/fakeowner/fakerepo/pull/42")
	reUp.Dir = home
	reUp.Env = env
	if out, err := reUp.CombinedOutput(); err != nil {
		t.Fatalf("second rig up <own-pr>: %v\n%s", err, out)
	} else if !strings.Contains(string(out), "already up") {
		t.Errorf("expected second up to switch to the existing rig, got:\n%s", out)
	}

	// Tear down so the temp HOME cleans up (the live session and workspace
	// otherwise keep the dir busy). --force because this authoring rig's PR
	// hasn't merged, which the guardrail would rightly block.
	downCmd := exec.Command(rigBin, "down", "--force")
	downCmd.Dir = basedir
	downCmd.Env = env
	if out, err := downCmd.CombinedOutput(); err != nil {
		t.Fatalf("rig down --force: %v\n%s", err, out)
	}
	waitFor(t, 10*time.Second, "own-PR rig down to finish", func() bool {
		_, err := os.Stat(basedir)
		return os.IsNotExist(err)
	})
}

// TestReap pins reap's post-judge contract: it is a janitor, not an executioner.
// The rig it builds is precisely the shape the old nightly pass used to collect
// — nothing off trunk, no WIP, no PR on record — which is also precisely the
// shape of a rig whose entire value is the agent conversation it carries. Reap
// must leave it standing at any age, and the idle knob it used to be judged by
// must be gone rather than merely defaulted to something safer.
func TestReap(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}

	home := t.TempDir()
	bin := filepath.Join(home, "bin")
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
		"SHELL=/bin/sh",
		"HISTFILE=/dev/null",
	)
	env = append(env, hermeticGitVars()...)

	mustRun(t, repoDir, env, "git", "init", "-q", "-b", "main")
	mustRun(t, repoDir, env, "git", "commit", "-q", "--allow-empty", "-m", "init")
	mustRun(t, repoDir, env, "jj", "git", "init", "--colocate")
	mustRun(t, repoDir, env, "jj", "config", "set", "--repo", `revset-aliases."trunk()"`, "main")

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
	ghNoPR := "#!/bin/sh\n" +
		"if [ \"$1\" = \"pr\" ] && [ \"$2\" = \"view\" ]; then\n" +
		"  echo 'no pull requests found for branch' >&2\n  exit 1\nfi\n" +
		"echo \"fake gh: unsupported invocation $*\" >&2\nexit 1\n"
	mustWriteExec(t, filepath.Join(bin, "gh"), ghNoPR)

	tmuxWrap := fmt.Sprintf("#!/bin/sh\nexec %s -L rig-e2e-reap \"$@\"\n", realTmux)
	mustWriteExec(t, filepath.Join(bin, "tmux"), tmuxWrap)

	sleeper := "#!/bin/sh\nexec sleep infinity\n"
	mustWriteExec(t, filepath.Join(bin, "recto"), sleeper)
	mustWriteExec(t, filepath.Join(bin, "claude"), sleeper)

	t.Cleanup(func() {
		_ = exec.Command(realTmux, "-L", "rig-e2e-reap", "kill-server").Run()
	})

	upCmd := exec.Command(rigBin, "up", "FAKE-1")
	upCmd.Dir = repoDir
	upCmd.Env = env
	if out, err := upCmd.CombinedOutput(); err != nil {
		t.Fatalf("rig up: %v\n%s", err, out)
	}
	basedir := filepath.Join(home, "workspaces", "fake-1-do-the-thing")

	// Backdate the manifest well past the window the old judge used, so this
	// asserts "reap does not collect rigs" rather than "the rig was too young".
	manifestPath := filepath.Join(basedir, ".rig.toml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	rewritten := regexp.MustCompile(`(?m)^created = .*$`).ReplaceAllString(string(raw), `created = "`+old+`"`)
	if rewritten == string(raw) {
		t.Fatalf("could not backdate manifest, no created line in:\n%s", raw)
	}
	if err := os.WriteFile(manifestPath, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}

	// A month-old rig with a clean tree and no PR: the old pass took this one.
	out := mustOutput(t, home, env, rigBin, "reap")
	if !strings.Contains(out, "runtime cleanup complete") {
		t.Errorf("expected reap to report runtime cleanup:\n%s", out)
	}
	if strings.Contains(out, "reaped") || strings.Contains(out, "would reap") {
		t.Errorf("reap still claims deletion authority:\n%s", out)
	}
	if _, err := os.Stat(basedir); err != nil {
		t.Errorf("reap deleted a rig it no longer has authority over: %v", err)
	}
	if err := exec.Command(realTmux, "-L", "rig-e2e-reap", "has-session", "-t", "~/workspaces/fake-1-do-the-thing").Run(); err != nil {
		t.Errorf("reap killed a live rig's session")
	}
	wsList := mustOutput(t, repoDir, env, "jj", "workspace", "list")
	if !strings.Contains(wsList, "fake-1-fakerepo") {
		t.Errorf("reap forgot a live rig's workspace:\n%s", wsList)
	}

	// The idle knob was the judge's only dial. It should be gone outright, not
	// quietly defaulted, so a stale unit passing it fails loudly instead of
	// looking like it still works.
	stale := exec.Command(rigBin, "reap", "--max-idle", "0")
	stale.Env = env
	if out, err := stale.CombinedOutput(); err == nil {
		t.Errorf("--max-idle still accepted:\n%s", out)
	}

	// --runtime-only survives as a no-op so deployed timers keep working
	// across a binary that updates before its unit does.
	out = mustOutput(t, home, env, rigBin, "reap", "--runtime-only")
	if !strings.Contains(out, "runtime cleanup complete") {
		t.Errorf("--runtime-only should still run the janitor:\n%s", out)
	}
}

// TestParkWake walks a rig through park → ls → switch → wake: parking kills the
// session and marks the manifest, ls still shows it (as parked), switch no
// longer offers it, and wake clears the mark and stands the session back up at
// the same basedir.
func TestParkWake(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}

	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	repoDir := filepath.Join(home, "src", "github.com", "fakeowner", "fakerepo")
	rigBin := filepath.Join(home, "rig")

	mustMkdir(t, bin)
	mustMkdir(t, repoDir)

	if out, err := exec.Command("go", "build", "-o", rigBin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	env := append(os.Environ(),
		"HOME="+home,
		"PATH="+bin+":"+os.Getenv("PATH"),
	)
	env = append(env, hermeticGitVars()...)

	mustRun(t, repoDir, env, "git", "init", "-q", "-b", "main")
	mustRun(t, repoDir, env, "git", "commit", "-q", "--allow-empty", "-m", "init")
	mustRun(t, repoDir, env, "jj", "git", "init", "--colocate")
	mustRun(t, repoDir, env, "jj", "config", "set", "--repo", `revset-aliases."trunk()"`, "main")

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

	socket := "rig-e2e-parkwake"
	tmuxWrap := fmt.Sprintf("#!/bin/sh\nexec %s -L %s \"$@\"\n", realTmux, socket)
	mustWriteExec(t, filepath.Join(bin, "tmux"), tmuxWrap)

	sleeper := "#!/bin/sh\nexec sleep infinity\n"
	mustWriteExec(t, filepath.Join(bin, "recto"), sleeper)
	mustWriteExec(t, filepath.Join(bin, "claude"), sleeper)

	t.Cleanup(func() {
		_ = exec.Command(realTmux, "-L", socket, "kill-server").Run()
	})

	upCmd := exec.Command(rigBin, "up", "FAKE-1")
	upCmd.Dir = repoDir
	upCmd.Env = env
	if out, err := upCmd.CombinedOutput(); err != nil {
		t.Fatalf("rig up: %v\n%s", err, out)
	}
	basedir := filepath.Join(home, "workspaces", "fake-1-do-the-thing")
	session := "~/workspaces/fake-1-do-the-thing"

	hasSession := func() bool {
		return exec.Command(realTmux, "-L", socket, "has-session", "-t", session).Run() == nil
	}
	if !hasSession() {
		t.Fatal("expected a session after up")
	}

	// --- rig park --- from inside the basedir.
	parkOut := mustOutput(t, basedir, env, rigBin, "park")
	if !strings.Contains(parkOut, "parked fake-1") {
		t.Errorf("park output missing confirmation:\n%s", parkOut)
	}
	if m := string(mustReadFile(t, filepath.Join(basedir, ".rig.toml"))); !strings.Contains(m, "parked = \"") {
		t.Errorf("manifest missing parked timestamp:\n%s", m)
	}
	if hasSession() {
		t.Error("expected park to kill the session")
	}

	// ls still lists the rig, now marked parked.
	lsOut := mustOutput(t, home, env, rigBin, "ls")
	if !strings.Contains(lsOut, "fake-1") || !strings.Contains(lsOut, "parked") {
		t.Errorf("ls should show the parked rig:\n%s", lsOut)
	}

	// switch no longer offers it — the only rig is parked, so there's nothing
	// to switch to.
	switchCmd := exec.Command(rigBin, "switch")
	switchCmd.Dir = home
	switchCmd.Env = env
	if out, err := switchCmd.CombinedOutput(); err == nil {
		t.Errorf("expected switch to skip the parked rig, got:\n%s", out)
	} else if !strings.Contains(string(out), "no other rigs in flight") {
		t.Errorf("expected 'no other rigs in flight', got:\n%s", out)
	}

	// --- rig wake --- clears the mark and rebuilds the session at the same path.
	wakeOut := mustOutput(t, home, env, rigBin, "wake", "FAKE-1")
	if !strings.Contains(wakeOut, "woke fake-1") {
		t.Errorf("wake output missing confirmation:\n%s", wakeOut)
	}
	if m := string(mustReadFile(t, filepath.Join(basedir, ".rig.toml"))); strings.Contains(m, "parked = \"") {
		t.Errorf("manifest still parked after wake:\n%s", m)
	}
	if !hasSession() {
		t.Error("expected wake to stand the session back up")
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

func waitFor(t *testing.T, timeout time.Duration, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ready() {
		if !time.Now().Before(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(20 * time.Millisecond)
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

// TestDownLeavesRecoverableTombstone is the round trip that justifies the
// tombstone existing at all: tear a rig down, then bring it back and land in
// the same conversation. It plants a claude session file first, because the
// agent stores key on cwd and the whole point is that teardown captures the
// session id at the one moment it's still resolvable.
func TestDownLeavesRecoverableTombstone(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}

	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	repoDir := filepath.Join(home, "src", "github.com", "fakeowner", "fakerepo")
	rigBin := filepath.Join(home, "rig")

	mustMkdir(t, bin)
	mustMkdir(t, repoDir)

	if out, err := exec.Command("go", "build", "-o", rigBin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	env := append(os.Environ(),
		"HOME="+home,
		"PATH="+bin+":"+os.Getenv("PATH"),
		"SHELL=/bin/sh",
		"HISTFILE=/dev/null",
		// The fixture below plants a Claude session for resurrection. Do not let
		// the developer's own RIG_AGENT turn this into a Codex-owned rig.
		"RIG_AGENT=claude",
		// Pin state explicitly so a set XDG_STATE_HOME in the developer's shell
		// can never route test tombstones into the real store.
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
	)
	env = append(env, hermeticGitVars()...)

	mustRun(t, repoDir, env, "git", "init", "-q", "-b", "main")
	mustRun(t, repoDir, env, "git", "commit", "-q", "--allow-empty", "-m", "init")
	mustRun(t, repoDir, env, "jj", "git", "init", "--colocate")
	mustRun(t, repoDir, env, "jj", "config", "set", "--repo", `revset-aliases."trunk()"`, "main")

	linearis := `#!/bin/sh
if [ "$1" = "issues" ] && [ "$2" = "read" ]; then
  cat <<JSON
{"identifier":"FAKE-1","title":"do the thing","branchName":"fake/fake-1-do-the-thing"}
JSON
  exit 0
fi
exit 1
`
	mustWriteExec(t, filepath.Join(bin, "linearis"), linearis)
	mustWriteExec(t, filepath.Join(bin, "gh"), "#!/bin/sh\n"+
		"if [ \"$1\" = \"pr\" ] && [ \"$2\" = \"view\" ]; then\n"+
		"  echo 'no pull requests found for branch' >&2\n  exit 1\nfi\nexit 1\n")
	mustWriteExec(t, filepath.Join(bin, "tmux"),
		fmt.Sprintf("#!/bin/sh\nexec %s -L rig-e2e-tomb \"$@\"\n", realTmux))
	sleeper := "#!/bin/sh\nexec sleep infinity\n"
	mustWriteExec(t, filepath.Join(bin, "recto"), sleeper)
	mustWriteExec(t, filepath.Join(bin, "claude"), sleeper)

	t.Cleanup(func() {
		_ = exec.Command(realTmux, "-L", "rig-e2e-tomb", "kill-server").Run()
	})

	upCmd := exec.Command(rigBin, "up", "FAKE-1")
	upCmd.Dir = repoDir
	upCmd.Env = env
	if out, err := upCmd.CombinedOutput(); err != nil {
		t.Fatalf("rig up: %v\n%s", err, out)
	}
	basedir := filepath.Join(home, "workspaces", "fake-1-do-the-thing")

	// The conversation this rig is "carrying". Claude names session files by
	// uuid, and that name is the id `--resume` takes.
	const sessionID = "11111111-2222-3333-4444-555555555555"
	projDir := filepath.Join(home, ".claude", "projects",
		claudeProjectDirName(filepath.Join(basedir, "fakerepo")))
	mustMkdir(t, projDir)
	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	downCmd := exec.Command(rigBin, "down")
	downCmd.Dir = basedir
	downCmd.Env = env
	if out, err := downCmd.CombinedOutput(); err != nil {
		t.Fatalf("rig down: %v\n%s", err, out)
	}
	waitFor(t, 10*time.Second, "rig down to finish", func() bool {
		_, statErr := os.Stat(basedir)
		return os.IsNotExist(statErr)
	})

	// History should offer it back, and mark it as having a live session.
	hist := mustOutput(t, home, env, rigBin, "history")
	if !strings.Contains(hist, "fake-1") {
		t.Fatalf("torn-down rig missing from history:\n%s", hist)
	}
	if !strings.Contains(hist, "↺") {
		t.Errorf("history should mark the rig recoverable:\n%s", hist)
	}

	res := exec.Command(rigBin, "resurrect", "fake-1")
	res.Dir = home
	res.Env = env
	out, err := res.CombinedOutput()
	if err != nil {
		t.Fatalf("rig resurrect: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), sessionID) {
		t.Errorf("resurrect should report the session it resumed:\n%s", out)
	}

	if _, err := os.Stat(filepath.Join(basedir, ".rig.toml")); err != nil {
		t.Errorf("resurrected rig has no manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(basedir, "fakerepo", ".jj")); err != nil {
		t.Errorf("resurrected rig did not get its workspace back: %v", err)
	}
	wsList := mustOutput(t, repoDir, env, "jj", "workspace", "list")
	if !strings.Contains(wsList, "fake-1-fakerepo") {
		t.Errorf("workspace not re-registered after resurrect:\n%s", wsList)
	}
}
