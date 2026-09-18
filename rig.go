package main

import (
	"errors"
	"fmt"
	"github.com/phinze/rig/internal/mux"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var nonSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

const ghShim = "#!/bin/sh\nexec rig __gh \"$@\"\n"

// slugify lowercases s and collapses any run of non-alphanumeric characters to
// a single dash, trimming dashes off the ends. Used for basedir / rig-id slugs.
func slugify(s string) string {
	return strings.Trim(nonSlugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// taskSlug joins a task id with a slugified title to form the basedir name,
// matching the shape Linear mints for branch names (id + slugified title,
// hard-capped with the trailing dash trimmed). Linear hands us its slug via
// branchName; this derives the equivalent for tasks that don't come with one
// (GitHub PRs, from their title).
func taskSlug(id, title string) string {
	const maxLen = 60
	t := slugify(title)
	if t == "" {
		return id
	}
	s := id + "-" + t
	if len(s) > maxLen {
		s = strings.TrimRight(s[:maxLen], "-")
	}
	return s
}

// basedirPath returns the absolute basedir for a rig given its slug name.
func basedirPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "workspaces", name), nil
}

// rigCreationInterrupted reports whether a rig on disk is the wreckage of its
// own interrupted create: the manifest landed, the workspace never did. It asks
// the manifest rather than the filesystem on purpose. A missing workspace dir
// is ambiguous (an interrupted create looks exactly like a workspace someone
// deleted out from under a real rig, and the second still owns branches and a
// conversation), while BuildingRepo is written by the create path itself and
// cleared the moment a workspace exists. Rigs predating that field read as
// complete, which is the pre-existing behavior and the safe way to be wrong.
func rigCreationInterrupted(m manifest) bool { return m.BuildingRepo != "" }

// finishRigHint names the way out of a half-built rig. `up` can finish one from
// its tracker id, so say that exact command. A rig that never had a ticket was
// made from a kickoff we can't reconstruct here, so it points at the discard
// rather than inventing a command that wouldn't work.
func finishRigHint(m manifest) string {
	if m.TrackerID != "" {
		return fmt.Sprintf("run `rig up %s` to finish it, or `rig down` to discard it", m.TrackerID)
	}
	return "run `rig down` to discard it, then start it over"
}

// createBasedir makes the rig basedir and writes its manifest + root .envrc.
// It errors if the basedir already exists so we never stomp an in-flight rig.
func createBasedir(basedir string, m manifest) error {
	if _, err := os.Stat(basedir); err == nil {
		// The one basedir we may reuse is the wreckage of our own interrupted
		// create: no workspace, no commits, no agent conversation, nothing to
		// lose. Reusing it is what makes re-running the creating command the
		// cure, which is the idempotency `rig up` already advertises. Every
		// other existing basedir is someone's in-flight work and stays
		// untouchable, including one whose manifest we can't read.
		old, err := readManifest(basedir)
		if err != nil || !rigCreationInterrupted(old) {
			return fmt.Errorf("basedir already exists: %s", basedir)
		}
		// Keep the original creation time: the task started when you first ran
		// the command, and ls sorting and ages should say so rather than reset
		// every time a create is retried.
		if m.Created.IsZero() {
			m.Created = old.Created
		}
	}
	if m.Created.IsZero() {
		m.Created = time.Now()
	}
	if m.Touched.IsZero() {
		m.Touched = m.Created
	}
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		return err
	}
	if err := writeRigShims(basedir); err != nil {
		return err
	}
	if err := writeManifest(basedir, m); err != nil {
		return err
	}
	if err := writeRootEnvrc(basedir, m); err != nil {
		return err
	}
	seedCodexTrustFor(basedir)
	return direnvAllow(basedir)
}

func writeRigShims(basedir string) error {
	shimDir := filepath.Join(basedir, ".rig", "bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(shimDir, "gh")
	if body, err := os.ReadFile(path); err == nil && string(body) == ghShim {
		return os.Chmod(path, 0o755)
	}
	if err := os.WriteFile(path, []byte(ghShim), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

// addRepoWorkspace creates a jj workspace for repo under basedir at startRev
// and records the repo (and its branch, when known) in the manifest. branch is
// the intended branch for this repo's work — up passes the Linear branch,
// review the PR head, add passes "" since an added repo starts on trunk with no
// branch yet. Returns the absolute path to the created workspace directory.
func addRepoWorkspace(basedir, rigID string, repo repoRef, startRev, branch string) (string, error) {
	repoDest := filepath.Join(basedir, repo.Name)
	wsName := jjWorkspaceName(rigID, repo.Name)

	// An orphan registration (name registered but dir gone, e.g. from a
	// cleanup that removed the dir but not the registration) blocks recreate.
	// Forget it first so we can claim the name again. jpickup parity.
	if !dirExists(repoDest) && workspaceRegistered(repo.Path, wsName) {
		fmt.Fprintf(os.Stderr, "rig: forgetting orphan workspace %s before recreate\n", wsName)
		_ = jjWorkspaceForget(repo.Path, []string{wsName})
	}

	// trunk() resolves against the source clone's remote-tracking refs, so a
	// repo nobody has fetched in a while seeds the workspace with whatever it
	// last saw. Branch-based callers already fetched in resolveStartRev; this
	// covers the ones that start from trunk. Best-effort on purpose, since
	// offline should still get you a workspace, but say so when it fails:
	// silently handing an agent a months-old tree is how you spend an afternoon
	// re-fixing something that landed upstream weeks ago.
	if startRev == "trunk()" {
		if err := jjGitFetch(repo.Path); err != nil {
			fmt.Fprintf(os.Stderr, "rig: warning: fetch of %s failed, starting from possibly stale trunk: %v\n", repo.Path, err)
		}
	}

	fmt.Fprintf(os.Stderr, "rig: jj workspace add %s (from %s) → %s\n", wsName, startRev, repoDest)
	if err := jjWorkspaceAdd(repo.Path, wsName, startRev, repoDest); err != nil {
		return "", fmt.Errorf("jj workspace add: %w", err)
	}

	// The workspace dir is where the agent pane and every ad hoc split actually
	// start, so it's the directory codex would stop to ask about.
	seedCodexTrustFor(repoDest)

	// A repo-owned .envrc takes precedence; otherwise direnv finds the rig's
	// basedir .envrc above it. Either entrypoint runs the global stdlib, which
	// projects the cwd-specific environment with `rig env`.
	if err := direnvAllow(repoDest); err != nil {
		return "", err
	}

	if err := addRepoToManifest(basedir, repo.Name, repo.nameWithOwner(), branch); err != nil {
		return "", err
	}

	// Refresh each agent-facing breadcrumb so its repo list reflects the repo we
	// just added. Best-effort: missing guidance shouldn't fail the add.
	if m, err := readManifest(basedir); err == nil {
		_ = writeRigAgentInstructions(basedir, m)
	}

	return repoDest, nil
}

// sessionSpec captures the per-verb session layout: what runs in the right
// (recto) pane, the agent in the left pane, and its opening prompt.
type sessionSpec struct {
	rectoCmd string
	repo     string
	prompt   string
	agent    agentKind
	// command overrides the agent invocation entirely. A fresh rig sends a
	// prompt, but a resurrected one sends a resume, which is a different verb
	// rather than a different prompt: the conversation it's reopening already
	// contains everything a kickoff would have said.
	command string
}

// spawnSession creates the rig's tmux session (recto right, agent left) if it
// doesn't already exist, and returns the session name. The session is named
// after the basedir (session-wizard convention) even though the panes start in
// the primary repo dir: the basedir is the rig's unit, and multi-repo rigs
// still get one session. Idempotent: an existing session is left untouched.
func spawnSession(basedir, paneCwd string, sess sessionSpec) (string, error) {
	session := rigSessionName(basedir)
	if backend.HasSession(session) {
		return session, nil
	}
	repo := sess.repo
	if repo == "" {
		repo = filepath.Base(paneCwd)
	}
	agentPane, mainWindow, err := backend.NewSession(session, mainWindowName(repo), paneCwd)
	if err != nil {
		return "", fmt.Errorf("tmux new-session: %w", err)
	}
	if err := markRigMainWindow(mainWindow, repo); err != nil {
		return "", fmt.Errorf("marking main window: %w", err)
	}
	if err := markRigPane(agentPane, rigPaneAgent, repo); err != nil {
		return "", fmt.Errorf("marking agent pane: %w", err)
	}
	rectoPane, err := backend.SplitCommand(agentPane, paneCwd, sess.rectoCmd)
	if err != nil {
		return "", fmt.Errorf("tmux split-window: %w", err)
	}
	if err := markRigPane(rectoPane, rigPaneRecto, repo); err != nil {
		return "", fmt.Errorf("marking recto pane: %w", err)
	}
	if err := backend.SelectPane(agentPane); err != nil {
		return "", fmt.Errorf("tmux select-pane: %w", err)
	}
	agentLine := sess.command
	if agentLine == "" {
		agentLine = sess.agent.launchCommand(sess.prompt)
	}
	if err := backend.SendKeys(agentPane, agentLine); err != nil {
		return "", fmt.Errorf("tmux send-keys: %w", err)
	}
	return session, nil
}

// spawnProjectSession creates the repositoryless control-plane variant of a
// rig session. Its agent starts at the basedir root and there is deliberately
// no Recto split: a project rig observes task rigs rather than owning a diff.
func spawnProjectSession(basedir string, sess sessionSpec) (string, error) {
	session := rigSessionName(basedir)
	if backend.HasSession(session) {
		return session, nil
	}
	agentPane, mainWindow, err := backend.NewSession(session, mainWindowName(""), basedir)
	if err != nil {
		return "", fmt.Errorf("tmux new-session: %w", err)
	}
	if err := markRigMainWindow(mainWindow, ""); err != nil {
		return "", fmt.Errorf("marking main window: %w", err)
	}
	if err := markRigPane(agentPane, rigPaneAgent, ""); err != nil {
		return "", fmt.Errorf("marking agent pane: %w", err)
	}
	agentLine := sess.command
	if agentLine == "" {
		agentLine = sess.agent.launchProjectCommand(sess.prompt)
	}
	if err := backend.SendKeys(agentPane, agentLine); err != nil {
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
	if err := backend.Attach(session); err != nil {
		if errors.Is(err, mux.ErrNoClientSwitch) {
			fmt.Fprintf(os.Stderr, "rig: session %q is ready, but %v — switch to it by hand\n", session, err)
			return nil
		}
		return err
	}
	return nil
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
