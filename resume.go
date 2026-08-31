package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// runResume repairs and enters an active rig's runtime. With no query it uses
// the rig containing cwd; an explicit query uses the ordinary rig picker. It
// deliberately does not change lifecycle state: a parked rig still needs wake.
func runResume(args []string) error {
	var basedir string
	if len(args) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		basedir, err = findBasedir(cwd)
		if err != nil {
			return err
		}
	} else {
		rigs, err := listRigs()
		if err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		chosen, err := pickRigStatus(rigStatuses(rigs, home, time.Now()), args, "resume rig: ")
		if err != nil || chosen == nil {
			return err
		}
		basedir = chosen.Path
	}

	session, err := resumeRigRuntime(basedir, false, true)
	if err != nil {
		return err
	}
	return attachOrReport(session)
}

// resumeRigRuntime owns repair of an active rig. Explicit resume refreshes the
// durable conversation hint before inspecting tmux; ordinary switching reuses
// an existing hint and resolves one only when a legacy manifest has none.
func resumeRigRuntime(basedir string, nonblocking, refreshSession bool) (string, error) {
	lock, err := acquireRigMutationLockMode(basedir, nonblocking)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Close() }()

	m, err := readManifest(basedir)
	if err != nil {
		return "", fmt.Errorf("reading manifest: %w", err)
	}
	if !m.Parked.IsZero() {
		return "", fmt.Errorf("%s is parked; use `rig wake %s`", m.ID, m.ID)
	}
	m.Touched = time.Now()
	captureRigRuntimeHints(basedir, &m, refreshSession)
	if err := writeManifest(basedir, m); err != nil {
		return "", err
	}
	return ensureRigRuntime(basedir, m)
}

// captureRigRuntimeHints records the parts tmux cannot recover after its
// session dies. The current carousel repo comes from tmux metadata. Conversation
// discovery is comparatively expensive for Codex, so ordinary activation only
// does it when no id is recorded; park and explicit resume ask for a refresh.
func captureRigRuntimeHints(basedir string, m *manifest, refreshSession bool) {
	session := tmuxSessionName(basedir)
	if tmuxHasSession(session) {
		if panes, err := tmuxRigPanes(session); err == nil {
			if repo := mainRepoFromPanes(basedir, *m, panes); repo != "" {
				m.MainRepo = repo
			}
		}
	}
	if m.MainRepo == "" || m.Repos[m.MainRepo] == "" {
		m.MainRepo = firstRigRepo(basedir, *m)
	}
	if !refreshSession && m.SessionID != "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	if ref := agentSessionRef(home, basedir, m.agentKind()); ref != nil {
		m.SessionID = ref.ID
	}
}

func firstRigRepo(basedir string, m manifest) string {
	repos := make([]string, 0, len(m.Repos))
	for repo := range m.Repos {
		if dirExists(filepath.Join(basedir, repo)) {
			repos = append(repos, repo)
		}
	}
	sort.Strings(repos)
	if len(repos) == 0 {
		return ""
	}
	return repos[0]
}

func mainRepoFromPanes(basedir string, m manifest, panes []rigTmuxPane) string {
	for _, p := range panes {
		if p.WindowRole == rigWindowMain && m.Repos[p.WindowRepo] != "" {
			return p.WindowRepo
		}
	}
	for _, p := range panes {
		if p.WindowRole != rigWindowMain {
			continue
		}
		if m.Repos[p.PaneRepo] != "" {
			return p.PaneRepo
		}
		if repo := repoForWorkspacePath(basedir, m, p.Path); repo != "" {
			return repo
		}
	}
	return ""
}

func rigResumeCommand(m manifest, prompt ...string) string {
	next := ""
	if len(prompt) > 0 {
		next = strings.TrimSpace(prompt[0])
	}
	if m.SessionID != "" {
		return m.agentKind().resumeCommandWithPrompt(m.SessionID, next)
	}
	if next != "" {
		if m.isProject() {
			return m.agentKind().launchProjectCommand(next)
		}
		return m.agentKind().launchCommand(next)
	}
	label := m.ID
	if m.Title != "" {
		label += " (" + m.Title + ")"
	}
	promptText := "Resume work on this rig: " + label + ". Read the rig instructions and existing workspace state, then continue from where the previous session left off."
	if m.isProject() {
		return m.agentKind().launchProjectCommand(promptText)
	}
	return m.agentKind().launchCommand(promptText)
}

// ensureRigRuntime brings a rig back to the same carousel shape it had at
// creation. It also repairs the common half-alive case where tmux survived but
// the agent exited to its shell.
func ensureRigRuntime(basedir string, m manifest) (string, error) {
	return ensureRigRuntimeWithPrompt(basedir, m, "")
}

func ensureRigRuntimeWithPrompt(basedir string, m manifest, prompt string) (string, error) {
	if m.isProject() {
		return ensureProjectRuntime(basedir, m, prompt)
	}
	repo := m.MainRepo
	if m.Repos[repo] == "" || !dirExists(filepath.Join(basedir, repo)) {
		repo = firstRigRepo(basedir, m)
	}
	if repo == "" {
		return "", fmt.Errorf("rig %s has no available repo workspace", m.ID)
	}
	paneCwd := filepath.Join(basedir, repo)
	session := tmuxSessionName(basedir)
	command := rigResumeCommand(m, prompt)

	if !tmuxHasSession(session) {
		var err error
		session, err = spawnSession(basedir, paneCwd, sessionSpec{
			rectoCmd: rectoCommand(), repo: repo, agent: m.agentKind(), command: command,
		})
		if err != nil {
			return "", err
		}
		if err := ensureBackgroundRectos(session, basedir, repo, m); err != nil {
			return "", err
		}
		return session, nil
	}

	panes, err := adoptLegacyRigPanes(session, basedir, m)
	if err != nil {
		return "", err
	}
	mainWindow := ""
	var agentPane rigTmuxPane
	for _, p := range panes {
		if p.WindowRole != rigWindowMain {
			continue
		}
		mainWindow = p.WindowID
		if p.PaneRole == rigPaneAgent {
			agentPane = p
		}
	}
	if mainWindow == "" {
		return "", fmt.Errorf("rig session %s has no main window", session)
	}
	if agentPane.PaneID == "" {
		for _, p := range panes {
			if p.WindowID == mainWindow && p.PaneRole != rigPaneRecto {
				agentPane = p
				break
			}
		}
	}
	if agentPane.PaneID == "" {
		pane, err := tmuxSplitShell(mainWindow, paneCwd)
		if err != nil {
			return "", fmt.Errorf("restoring agent pane: %w", err)
		}
		agentPane = rigTmuxPane{PaneID: pane, WindowID: mainWindow, Command: filepath.Base(os.Getenv("SHELL"))}
	}
	if err := markRigMainWindow(mainWindow, repo); err != nil {
		return "", err
	}
	if err := tmuxRenameWindow(mainWindow, mainWindowName(repo)); err != nil {
		return "", err
	}
	if err := markRigPane(agentPane.PaneID, rigPaneAgent, repo); err != nil {
		return "", err
	}

	panes, err = tmuxRigPanes(session)
	if err != nil {
		return "", err
	}
	panes, err = ensureRepoRecto(session, basedir, repo, panes)
	if err != nil {
		return "", fmt.Errorf("starting %s recto: %w", repo, err)
	}
	if err := promoteRecto(session, basedir, repo, m); err != nil {
		return "", err
	}
	if err := ensureBackgroundRectos(session, basedir, repo, m); err != nil {
		return "", err
	}

	// When resume is invoked from the stopped agent's own pane, replace this
	// process directly. Sending keys there would feed this foreground command,
	// not the shell waiting underneath it.
	selfCaller := agentPane.PaneID == os.Getenv("TMUX_PANE")
	rigCaller := filepath.Base(strings.TrimSpace(agentPane.Command)) == filepath.Base(os.Args[0])
	if selfCaller && (rigCaller || isShellCommand(agentPane.Command)) {
		if err := os.Chdir(paneCwd); err != nil {
			return "", err
		}
		if err := syscall.Exec("/bin/sh", []string{"sh", "-c", "exec " + command}, os.Environ()); err != nil {
			return "", fmt.Errorf("resuming agent in current pane: %w", err)
		}
	}
	if isShellCommand(agentPane.Command) {
		line := "cd " + shellQuote(paneCwd) + " && " + command
		if err := tmuxSendKeys(agentPane.PaneID, line); err != nil {
			return "", fmt.Errorf("resuming agent: %w", err)
		}
	}
	if err := tmuxSelectPane(agentPane.PaneID); err != nil {
		return "", err
	}
	return session, nil
}

// ensureProjectRuntime repairs the agent-only session used by a project rig.
// It mirrors the agent half of ensureRigRuntime without inventing a fake repo
// or starting Recto in a directory that has no jj workspace.
func ensureProjectRuntime(basedir string, m manifest, prompt string) (string, error) {
	session := tmuxSessionName(basedir)
	command := rigResumeCommand(m, prompt)
	if !tmuxHasSession(session) {
		return spawnProjectSession(basedir, sessionSpec{agent: m.agentKind(), command: command})
	}

	panes, err := tmuxRigPanes(session)
	if err != nil {
		return "", err
	}
	var mainWindow, agentPane rigTmuxPane
	for _, p := range panes {
		if p.WindowRole == rigWindowMain || (mainWindow.WindowID == "" && p.WindowIdx == "0") {
			mainWindow = p
		}
		if p.PaneRole == rigPaneAgent {
			agentPane = p
		}
	}
	if mainWindow.WindowID == "" {
		return "", fmt.Errorf("project rig session %s has no main window", session)
	}
	if agentPane.PaneID == "" {
		for _, p := range panes {
			if p.WindowID == mainWindow.WindowID && p.PaneRole != rigPaneRecto {
				agentPane = p
				break
			}
		}
	}
	if agentPane.PaneID == "" {
		pane, err := tmuxSplitShell(mainWindow.WindowID, basedir)
		if err != nil {
			return "", fmt.Errorf("restoring project agent pane: %w", err)
		}
		agentPane = rigTmuxPane{PaneID: pane, WindowID: mainWindow.WindowID, Command: filepath.Base(os.Getenv("SHELL"))}
	}
	if err := markRigMainWindow(mainWindow.WindowID, ""); err != nil {
		return "", err
	}
	if err := tmuxRenameWindow(mainWindow.WindowID, mainWindowName("")); err != nil {
		return "", err
	}
	if err := markRigPane(agentPane.PaneID, rigPaneAgent, ""); err != nil {
		return "", err
	}

	selfCaller := agentPane.PaneID == os.Getenv("TMUX_PANE")
	rigCaller := filepath.Base(strings.TrimSpace(agentPane.Command)) == filepath.Base(os.Args[0])
	if selfCaller && (rigCaller || isShellCommand(agentPane.Command)) {
		if err := os.Chdir(basedir); err != nil {
			return "", err
		}
		if err := syscall.Exec("/bin/sh", []string{"sh", "-c", "exec " + command}, os.Environ()); err != nil {
			return "", fmt.Errorf("resuming project agent in current pane: %w", err)
		}
	}
	if isShellCommand(agentPane.Command) {
		line := "cd " + shellQuote(basedir) + " && " + command
		if err := tmuxSendKeys(agentPane.PaneID, line); err != nil {
			return "", fmt.Errorf("resuming project agent: %w", err)
		}
	}
	if err := tmuxSelectPane(agentPane.PaneID); err != nil {
		return "", err
	}
	return session, nil
}

func ensureBackgroundRectos(session, basedir, mainRepo string, m manifest) error {
	repos := make([]string, 0, len(m.Repos))
	for repo := range m.Repos {
		if repo != mainRepo && dirExists(filepath.Join(basedir, repo)) {
			repos = append(repos, repo)
		}
	}
	sort.Strings(repos)
	panes, err := tmuxRigPanes(session)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		panes, err = ensureRepoRecto(session, basedir, repo, panes)
		if err != nil {
			return fmt.Errorf("starting %s recto: %w", repo, err)
		}
	}
	return nil
}

func isShellCommand(command string) bool {
	command = filepath.Base(strings.TrimSpace(command))
	switch command {
	case "", "sh", "bash", "dash", "zsh", "fish", "nu":
		return true
	default:
		return false
	}
}
