package main

import (
	"fmt"
	"os"
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

	for source, names := range forgetGroups {
		fmt.Fprintf(os.Stderr, "rig: jj workspace forget %v (from %s)\n", names, source)
		if err := jjWorkspaceForget(source, names); err != nil {
			return fmt.Errorf("jj workspace forget: %w", err)
		}
	}

	// Note if the caller will be stranded by their cwd vanishing. This
	// matters when we're NOT killing the session (running from outside
	// tmux or from a different session) — otherwise the session dies and
	// the question is moot.
	cwdInside := false
	if cwd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(basedir, cwd); err == nil && !strings.HasPrefix(rel, "..") {
			cwdInside = true
		}
	}

	// Move our own cwd out so RemoveAll can walk basedir cleanly.
	if err := os.Chdir(os.Getenv("HOME")); err != nil {
		return err
	}
	if err := os.RemoveAll(basedir); err != nil {
		return fmt.Errorf("removing basedir: %w", err)
	}

	fmt.Fprintf(os.Stderr, "rig: down %s — %s gone\n", m.ID, basedir)

	// Kill the session LAST so the SIGHUP it produces (when we're inside
	// it) can't cut short the destructive steps above. If we're outside
	// the session this is just a normal kill; if we're inside, our
	// terminal exits cleanly with the work already done.
	session := tmuxSessionName(m.ID)
	if cwdInside && !insideTmuxSession(session) {
		fmt.Fprintf(os.Stderr, "rig: note: your shell's cwd was inside the basedir; run `cd` to recover.\n")
	}
	if err := tmuxKillSession(session); err != nil {
		return fmt.Errorf("tmux kill-session %s: %w", session, err)
	}
	return nil
}
