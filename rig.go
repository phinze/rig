package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var nonSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify lowercases s and collapses any run of non-alphanumeric characters to
// a single dash, trimming dashes off the ends. Used for basedir / rig-id slugs.
func slugify(s string) string {
	return strings.Trim(nonSlugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// basedirPath returns the absolute basedir for a rig given its slug name.
func basedirPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "workspaces", name), nil
}

// createBasedir makes the rig basedir and writes its manifest + root .envrc.
// It errors if the basedir already exists so we never stomp an in-flight rig.
func createBasedir(basedir string, m manifest) error {
	if _, err := os.Stat(basedir); err == nil {
		return fmt.Errorf("basedir already exists: %s", basedir)
	}
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		return err
	}
	if err := writeManifest(basedir, m); err != nil {
		return err
	}
	if err := writeRootEnvrc(basedir, m); err != nil {
		return err
	}
	return direnvAllow(basedir)
}

// addRepoWorkspace creates a jj workspace for repo under basedir at startRev,
// drops a direnv anchor so GH_REPO loads, and records the repo in the manifest.
// Returns the absolute path to the created workspace directory.
func addRepoWorkspace(basedir, rigID string, repo repoRef, startRev string) (string, error) {
	repoDest := filepath.Join(basedir, repo.Name)
	wsName := jjWorkspaceName(rigID, repo.Name)

	// An orphan registration (name registered but dir gone, e.g. from a
	// cleanup that removed the dir but not the registration) blocks recreate.
	// Forget it first so we can claim the name again. jpickup parity.
	if !dirExists(repoDest) && workspaceRegistered(repo.Path, wsName) {
		fmt.Fprintf(os.Stderr, "rig: forgetting orphan workspace %s before recreate\n", wsName)
		_ = jjWorkspaceForget(repo.Path, []string{wsName})
	}

	fmt.Fprintf(os.Stderr, "rig: jj workspace add %s (from %s) → %s\n", wsName, startRev, repoDest)
	if err := jjWorkspaceAdd(repo.Path, wsName, startRev, repoDest); err != nil {
		return "", fmt.Errorf("jj workspace add: %w", err)
	}

	// direnv anchor: the workspace needs *some* .envrc for direnv to fire, at
	// which point the global direnvrc reads GH_REPO out of the manifest. Don't
	// clobber a project's own .envrc (nix devshells etc.) — it triggers direnv
	// on its own and picks up GH_REPO the same way.
	envrcPath := filepath.Join(repoDest, ".envrc")
	if _, err := os.Stat(envrcPath); err != nil {
		if err := os.WriteFile(envrcPath, []byte("source_up\n"), 0o644); err != nil {
			return "", err
		}
	}
	if err := direnvAllow(repoDest); err != nil {
		return "", err
	}

	if err := addRepoToManifest(basedir, repo.Name, repo.nameWithOwner()); err != nil {
		return "", err
	}
	return repoDest, nil
}

// sessionSpec captures the per-verb session layout: what runs in the right
// (recto) pane and the prompt sent to claude in the left pane.
type sessionSpec struct {
	rectoCmd string
	prompt   string
}

// spawnSession creates the rig's tmux session (recto right, claude left) if it
// doesn't already exist, and returns the session name. Idempotent: an existing
// session is left untouched.
func spawnSession(rigID, paneCwd string, sess sessionSpec) (string, error) {
	session := tmuxSessionName(rigID)
	if tmuxHasSession(session) {
		return session, nil
	}
	if err := tmuxNewSession(session, paneCwd); err != nil {
		return "", fmt.Errorf("tmux new-session: %w", err)
	}
	if err := tmuxSplitH(session, paneCwd, sess.rectoCmd); err != nil {
		return "", fmt.Errorf("tmux split-window: %w", err)
	}
	left := session + ":0.0"
	if err := tmuxSelectPane(left); err != nil {
		return "", fmt.Errorf("tmux select-pane: %w", err)
	}
	claudeLine := "claude --dangerously-skip-permissions " + shellQuote(sess.prompt)
	if err := tmuxSendKeys(left, claudeLine); err != nil {
		return "", fmt.Errorf("tmux send-keys: %w", err)
	}
	return session, nil
}

// attachOrReport attaches to the session when stdin is a tty, otherwise prints
// how to attach manually (e.g. when invoked from a script or test).
func attachOrReport(session string) error {
	if !stdinIsTTY() {
		fmt.Fprintf(os.Stderr, "rig: not a tty — session ready as %q, attach manually\n", session)
		return nil
	}
	return tmuxAttach(session)
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
