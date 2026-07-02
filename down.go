package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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
	// uncommitted changes or off-trunk commits not under a merged branch), and
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
	if cwdInside && !insideTmuxSession(session) {
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
// block (nothing to merge); a gh error blocks, fail-closed.
func unmergedPRsBlocker(basedir string) string {
	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Sprintf("reading manifest: %v", err)
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

	for source, names := range forgetGroups {
		fmt.Fprintf(os.Stderr, "rig: jj workspace forget %v (from %s)\n", names, source)
		if err := jjWorkspaceForget(source, names); err != nil {
			return fmt.Errorf("jj workspace forget: %w", err)
		}
	}

	if err := os.RemoveAll(basedir); err != nil {
		return fmt.Errorf("removing basedir: %w", err)
	}
	return nil
}

// isoStop stops one iso session by exact name, run from the workspace dir
// so iso resolves the right project scope.
func isoStop(workspaceDir, session string) error {
	cmd := exec.Command("iso", "stop", "--session", session)
	cmd.Dir = workspaceDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
