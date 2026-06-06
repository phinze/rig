package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func runDown(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: rig down")
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
