package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func runUp(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: rig up <issue-id>")
	}
	id := args[0]

	tk, err := resolveTask(id)
	if err != nil {
		return err
	}

	repo, err := detectPrimaryRepo()
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	basedir := filepath.Join(home, "workspaces", tk.basedirName())
	if _, err := os.Stat(basedir); err == nil {
		return fmt.Errorf("basedir already exists: %s", basedir)
	}

	if err := ensureJJColocated(repo.Path); err != nil {
		return fmt.Errorf("colocating jj on %s: %w", repo.Path, err)
	}

	if err := os.MkdirAll(basedir, 0o755); err != nil {
		return err
	}

	m := manifest{ID: tk.rigID(), Title: tk.Title}
	if err := writeManifest(basedir, m); err != nil {
		return err
	}
	if err := writeRootEnvrc(basedir, m); err != nil {
		return err
	}
	if err := direnvAllow(basedir); err != nil {
		return err
	}

	repoDest := filepath.Join(basedir, repo.Name)
	wsName := jjWorkspaceName(tk.rigID(), repo.Name)
	startRev := resolveStartRev(repo.Path, tk.BranchName)
	fmt.Fprintf(os.Stderr, "rig: jj workspace add %s (from %s) → %s\n", wsName, startRev, repoDest)
	if err := jjWorkspaceAdd(repo.Path, wsName, startRev, repoDest); err != nil {
		return fmt.Errorf("jj workspace add: %w", err)
	}
	// The workspace's working copy may already contain a .envrc (e.g. a
	// project's `use flake` shim). Bless it so direnv loads it on cd.
	if err := direnvAllow(repoDest); err != nil {
		return err
	}

	session := tmuxSessionName(tk.rigID())
	if !tmuxHasSession(session) {
		if err := tmuxNewSession(session, repoDest); err != nil {
			return fmt.Errorf("tmux new-session: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "rig: up %s — %s\n", tk.Identifier, basedir)
	if !stdinIsTTY() {
		fmt.Fprintf(os.Stderr, "rig: not a tty — session ready as %q, attach manually\n", session)
		return nil
	}
	return tmuxAttach(session)
}
