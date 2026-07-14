package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func runDown(args []string) error {
	force := false
	for _, a := range args {
		switch a {
		case "--force", "-f":
			force = true
		default:
			return fmt.Errorf("usage: rig down [--force]")
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	basedir, err := findBasedir(cwd)
	if err != nil {
		return err
	}

	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}

	// Safety gate, unless forced. Two checks that together mean "this rig's
	// work is truly done": the shared, lazy local-work judgment (reap's too —
	// working-copy changes or off-trunk commits not covered by a merged PR), and
	// an eager pass that asks GitHub about every recorded PR, so an OPEN PR
	// blocks even when the workspace holds no local commits. --force skips both.
	if !force {
		if reason := rigTeardownBlocker(basedir, map[string]bool{}); reason != "" {
			return downRefusal(reason)
		}
		if reason := unmergedPRsBlocker(basedir); reason != "" {
			return downRefusal(reason)
		}
	}

	// Note if the caller will be stranded by their cwd vanishing. This
	// matters when we're NOT killing the session (running from outside
	// tmux or from a different session) — otherwise the session dies and
	// the question is moot.
	cwdInside := false
	if rel, err := filepath.Rel(basedir, cwd); err == nil && !strings.HasPrefix(rel, "..") {
		cwdInside = true
	}

	// Move our own cwd out so RemoveAll can walk basedir cleanly.
	if err := os.Chdir(os.Getenv("HOME")); err != nil {
		return err
	}

	if err := teardownRig(basedir, m); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "rig: down %s — %s gone\n", m.ID, basedir)

	// Kill the session LAST so the SIGHUP it produces (when we're inside
	// it) can't cut short the destructive steps above. If we're outside
	// the session this is just a normal kill; if we're inside, our
	// terminal exits cleanly with the work already done.
	session := tmuxSessionName(basedir)
	inDoomed := insideTmuxSession(session)

	// Interactive rescue: when down runs from inside the very session it's about
	// to kill, pop the radar so you can pick where to land next instead of the
	// kill dropping you to the outer shell. The teardown above already happened —
	// the pick only chooses a destination, and escaping falls through to the
	// plain detach. We only do this when we're actually in the doomed session:
	// run from elsewhere, the kill never touches your client, so a TUI would just
	// hijack a terminal that was going to stay put anyway.
	if inDoomed && stdinIsTTY() {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dest, err := radarPick(home)
		if err != nil {
			return err
		}
		if dest != nil {
			// switch-client onto the destination before the kill, so by the time
			// this session dies our client is already looking elsewhere.
			if err := radarAct(*dest); err != nil {
				return err
			}
		}
	}

	if cwdInside && !inDoomed {
		fmt.Fprintf(os.Stderr, "rig: note: your shell's cwd was inside the basedir; run `cd` to recover.\n")
	}
	if err := tmuxKillSession(session); err != nil {
		return fmt.Errorf("tmux kill-session %s: %w", session, err)
	}
	return nil
}

// downRefusal wraps a blocker reason as the error `rig down` exits with, always
// pointing at the escape hatch so the gate never feels like a dead end.
func downRefusal(reason string) error {
	return fmt.Errorf("refusing to tear down: %s\n      run `rig down --force` to override", reason)
}

// unmergedPRsBlocker reports the first recorded PR that isn't merged, or "" when
// every recorded branch's PR is merged (or has no PR at all). Unlike the lazy
// workspace WIP check it always asks GitHub, which is what lets `rig down`
// refuse on an OPEN PR even when the workspace holds no local commits — the
// disjoint secondary-PR case reap's lazy path skips. A branch with no PR doesn't
// block (nothing to merge); a gh error blocks, fail-closed. Review rigs are
// exempt — merge state isn't their terminal condition (see below).
func unmergedPRsBlocker(basedir string) string {
	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Sprintf("reading manifest: %v", err)
	}
	// Review rigs are exempt: an OPEN PR is the whole point of reviewing it, not
	// a reason to keep the basedir. Their terminal condition ("you've posted a
	// review") is enforced by the shared rigTeardownBlocker, so this eager
	// all-PRs-merged gate would only ever wrongly refuse them.
	if m.isReview() {
		return ""
	}
	subdirs := make([]string, 0, len(m.Repos))
	for sub := range m.Repos {
		subdirs = append(subdirs, sub)
	}
	sort.Strings(subdirs)
	for _, sub := range subdirs {
		branches, err := repoBranches(m, sub, filepath.Join(basedir, sub))
		if err != nil {
			return fmt.Sprintf("%s: resolving branches: %v", sub, err)
		}
		for _, b := range branches {
			pr, err := prForBranch(m.Repos[sub], b)
			if err != nil {
				return fmt.Sprintf("%s: checking PR for %s: %v", sub, b, err)
			}
			if pr != nil && pr.State != "MERGED" {
				return fmt.Sprintf("%s PR #%d (%s) is %s, not merged",
					sub, pr.Number, b, strings.ToLower(pr.State))
			}
		}
	}
	return ""
}

// teardownRig dismantles a rig's resources except its tmux session: iso
// sessions are stopped by exact name, jj workspace registrations forgotten,
// and the basedir removed. The session kill is left to callers so they can
// sequence it last — down may be running *inside* the session, and the
// SIGHUP from the kill must not cut short the steps here.
func teardownRig(basedir string, m manifest) error {
	// Walk subdirs for jj workspaces. Group forget calls by source repo so
	// multi-repo rigs don't try to forget workspace-A's name against
	// workspace-B's source.
	entries, err := os.ReadDir(basedir)
	if err != nil {
		return err
	}
	forgetGroups := map[string][]string{} // source repo path → workspace names
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(basedir, e.Name())
		if _, err := os.Stat(filepath.Join(p, ".jj")); err != nil {
			continue
		}
		source, err := jjSourceRepo(p)
		if err != nil {
			return fmt.Errorf("resolving source repo for %s: %w", p, err)
		}
		name := jjWorkspaceName(m.ID, e.Name())
		forgetGroups[source] = append(forgetGroups[source], name)
	}

	// Stop iso sessions before their workspace dirs vanish. Exact name only —
	// iso's project scope is basename-derived, so an --all-sessions from a
	// workspace dir would also stop the main checkout's container of a
	// same-named repo. Best-effort: a failed stop shouldn't strand teardown.
	if _, err := exec.LookPath("iso"); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(basedir, e.Name())
			if !dirExists(filepath.Join(p, ".iso")) {
				continue
			}
			session := isoSessionName(m.ID, e.Name())
			fmt.Fprintf(os.Stderr, "rig: iso stop --session %s\n", session)
			if err := isoStop(p, session); err != nil {
				fmt.Fprintf(os.Stderr, "rig: warning: iso stop %s: %v\n", session, err)
			}
		}
	}

	// Reap compose-based dev-envs (e.g. cloud's postgres/valkey). The iso loop
	// above only touches .iso projects, so a repo whose dev-env is docker-compose
	// would otherwise leak its containers, network, and volumes on every teardown.
	// Compose stamps each container with the working_dir it was started from, so
	// we find every project rooted inside this rig without needing to know its
	// (dev-id-derived) project name. Best-effort, same as iso stop.
	if _, err := exec.LookPath("docker"); err == nil {
		for project, workdir := range composeProjectsUnder(basedir) {
			fmt.Fprintf(os.Stderr, "rig: docker compose -p %s down -v\n", project)
			if err := composeDown(workdir, project); err != nil {
				fmt.Fprintf(os.Stderr, "rig: warning: compose down %s: %v\n", project, err)
			}
		}
	}

	for source, names := range forgetGroups {
		fmt.Fprintf(os.Stderr, "rig: jj workspace forget %v (from %s)\n", names, source)
		if err := jjWorkspaceForget(source, names); err != nil {
			return fmt.Errorf("jj workspace forget: %w", err)
		}
	}

	// Move the whole rig out of the active namespace before recursively deleting
	// it. Rename only touches the two parent directories, so root-owned output
	// nested anywhere inside cannot block this atomic step. If RemoveAll later
	// fails, the rig is still fully down; only clearly named trash remains.
	quarantined, err := quarantineBasedir(basedir)
	if err != nil {
		return fmt.Errorf("quarantining basedir: %w", err)
	}
	if err := os.RemoveAll(quarantined); err != nil {
		fmt.Fprintf(os.Stderr, "rig: warning: could not fully remove quarantined basedir: %v\n", err)
		fmt.Fprintf(os.Stderr, "rig: warning: clean it up with: sudo rm -rf %s\n", shellQuote(quarantined))
		return nil
	}
	// Usually remove the now-empty trash directory too. A concurrent teardown or
	// older cleanup failure can legitimately leave it non-empty.
	_ = os.Remove(filepath.Dir(quarantined))
	return nil
}

// quarantineBasedir atomically moves basedir into a hidden sibling directory.
// Keeping the trash directory beside the rigs guarantees the rename stays on
// one filesystem, where directory rename is atomic and independent of the
// ownership of anything below basedir.
func quarantineBasedir(basedir string) (string, error) {
	trashRoot := filepath.Join(filepath.Dir(basedir), ".rig-trash")
	if err := os.Mkdir(trashRoot, 0o700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("creating %s: %w", trashRoot, err)
	}

	dest := filepath.Join(trashRoot, fmt.Sprintf("%s-%d-%d",
		filepath.Base(basedir), time.Now().UnixNano(), os.Getpid()))
	if err := os.Rename(basedir, dest); err != nil {
		return "", fmt.Errorf("moving %s to %s: %w", basedir, dest, err)
	}
	return dest, nil
}

// isoStop stops one iso session by exact name, run from the workspace dir
// so iso resolves the right project scope.
func isoStop(workspaceDir, session string) error {
	cmd := exec.Command("iso", "stop", "--session", session)
	cmd.Dir = workspaceDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// composeProjectsUnder returns docker-compose projects whose containers were
// started from somewhere inside basedir, mapping project name to a working_dir
// we can run `docker compose down` from. Matching on the working_dir label means
// we don't need to know a project's (dev-id-derived) name, and resolving symlinks
// on both sides keeps the prefix check honest.
func composeProjectsUnder(basedir string) map[string]string {
	out, err := exec.Command("docker", "ps", "-a",
		"--filter", "label=com.docker.compose.project",
		"--format", `{{.Label "com.docker.compose.project"}}`+"\t"+`{{.Label "com.docker.compose.project.working_dir"}}`,
	).Output()
	if err != nil {
		return nil
	}

	base := resolvePath(basedir)
	found := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		project, workdir, ok := strings.Cut(line, "\t")
		if !ok || project == "" || workdir == "" {
			continue
		}
		if isUnder(resolvePath(workdir), base) {
			found[project] = workdir
		}
	}
	return found
}

// composeDown tears a compose project down entirely (containers, network, and
// volumes) from its working dir. rig down is a permanent teardown, so a lingering
// dev database is just another orphan.
func composeDown(workdir, project string) error {
	cmd := exec.Command("docker", "compose", "-p", project, "down", "-v")
	cmd.Dir = workdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// resolvePath returns dir with symlinks resolved, falling back to a cleaned
// absolute path when the target can't be resolved (e.g. it no longer exists).
func resolvePath(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return filepath.Clean(dir)
}

// isUnder reports whether child is parent or nested inside it.
func isUnder(child, parent string) bool {
	return child == parent || strings.HasPrefix(child, parent+string(os.PathSeparator))
}
