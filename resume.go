package main

import (
	"fmt"
	"github.com/phinze/rig/internal/mux"
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

	rs, err := resumeRigRuntime(basedir, false, true)
	if err != nil {
		return err
	}
	return rs.attach()
}

// resumeRigRuntime owns repair of an active rig. Explicit resume refreshes the
// durable conversation hint before inspecting tmux; ordinary switching reuses
// an existing hint and resolves one only when a legacy manifest has none.
func resumeRigRuntime(basedir string, nonblocking, refreshSession bool) (rigSession, error) {
	lock, err := acquireRigMutationLockMode(basedir, nonblocking)
	if err != nil {
		return rigSession{}, err
	}
	defer func() { _ = lock.Close() }()

	m, err := readManifest(basedir)
	if err != nil {
		return rigSession{}, fmt.Errorf("reading manifest: %w", err)
	}
	if !m.Parked.IsZero() {
		return rigSession{}, fmt.Errorf("%s is parked; use `rig wake %s`", m.ID, m.ID)
	}
	m.Touched = time.Now()
	captureRigRuntimeHints(basedir, &m, refreshSession)
	if err := writeManifest(basedir, m); err != nil {
		return rigSession{}, err
	}
	return ensureRigRuntime(basedir, m)
}

// captureRigRuntimeHints records the parts tmux cannot recover after its
// session dies. The current carousel repo comes from tmux metadata. Conversation
// discovery is comparatively expensive for Codex, so ordinary activation only
// does it when no id is recorded; park and explicit resume ask for a refresh.
func captureRigRuntimeHints(basedir string, m *manifest, refreshSession bool) {
	rs := sessionFor(basedir, *m)
	if rs.live() {
		if panes, err := rs.panes(); err == nil {
			if repo := mainRepoFromPanes(basedir, *m, panes); repo != "" {
				m.MainRepo = repo
			}
		}
	}
	// Only replace the recorded main repo with one we actually found. A rig
	// whose create was interrupted has no workspace on disk to fall back to, and
	// blanking the field there destroys the only surviving record of which repo
	// it was being built around — while running on the very path that is
	// supposed to be repairing it. Each failed retry used to make the rig a
	// little less recoverable.
	if m.MainRepo == "" || m.Repos[m.MainRepo] == "" {
		if repo := firstRigRepo(basedir, *m); repo != "" {
			m.MainRepo = repo
		}
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

func mainRepoFromPanes(basedir string, m manifest, panes []mux.Pane) string {
	for _, p := range panes {
		if p.WindowRole == rigWindowMain && m.Repos[p.WindowRepo] != "" {
			return p.WindowRepo
		}
	}
	for _, p := range panes {
		if p.WindowRole != rigWindowMain {
			continue
		}
		if m.Repos[p.Repo] != "" {
			return p.Repo
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
func ensureRigRuntime(basedir string, m manifest) (rigSession, error) {
	return ensureRigRuntimeWithPrompt(basedir, m, "")
}

func ensureRigRuntimeWithPrompt(basedir string, m manifest, prompt string) (rigSession, error) {
	if m.isProject() {
		return ensureProjectRuntime(basedir, m, prompt)
	}
	repo := m.MainRepo
	if m.Repos[repo] == "" || !dirExists(filepath.Join(basedir, repo)) {
		repo = firstRigRepo(basedir, m)
	}
	if repo == "" {
		// `up` finishes a half-built rig rather than landing here, but switch,
		// wake, resume, and the radar all share this path and none of them can
		// build a workspace. Name the way out instead of stating the symptom:
		// this is the error you hit while wondering why a rig you can see is a
		// rig you cannot enter.
		if rigCreationInterrupted(m) {
			return rigSession{}, fmt.Errorf("rig %s never finished being created: %s", m.ID, finishRigHint(m))
		}
		return rigSession{}, fmt.Errorf("rig %s has no available repo workspace", m.ID)
	}
	paneCwd := filepath.Join(basedir, repo)
	rs := sessionFor(basedir, m)
	command := rigResumeCommand(m, prompt)

	if !rs.live() {
		err := spawnSession(rs, paneCwd, sessionSpec{
			rectoCmd: rectoCommand(), repo: repo, agent: m.agentKind(), command: command,
		})
		if err != nil {
			return rigSession{}, err
		}
		if err := ensureBackgroundRectos(rs, basedir, repo, m); err != nil {
			return rigSession{}, err
		}
		return rs, nil
	}

	panes, err := adoptLegacyRigPanes(rs, basedir, m)
	if err != nil {
		return rigSession{}, err
	}
	mainWindow := ""
	var agentPane mux.Pane
	for _, p := range panes {
		if p.WindowRole != rigWindowMain {
			continue
		}
		mainWindow = p.WindowID
		if p.Role == rigPaneAgent {
			agentPane = p
		}
	}
	if mainWindow == "" {
		return rigSession{}, fmt.Errorf("rig session %s has no main window", rs.name)
	}
	if agentPane.PaneID == "" {
		for _, p := range panes {
			if p.WindowID == mainWindow && p.Role != rigPaneRecto {
				agentPane = p
				break
			}
		}
	}
	if agentPane.PaneID == "" {
		pane, err := rs.b.SplitShell(mainWindow, paneCwd)
		if err != nil {
			return rigSession{}, fmt.Errorf("restoring agent pane: %w", err)
		}
		agentPane = mux.Pane{PaneID: pane, WindowID: mainWindow, Command: filepath.Base(os.Getenv("SHELL"))}
	}
	if err := rs.markMainWindow(mainWindow, repo); err != nil {
		return rigSession{}, err
	}
	if err := rs.b.RenameWindow(mainWindow, mainWindowName(repo)); err != nil {
		return rigSession{}, err
	}
	if err := rs.markPane(agentPane.PaneID, rigPaneAgent, repo); err != nil {
		return rigSession{}, err
	}

	panes, err = rs.panes()
	if err != nil {
		return rigSession{}, err
	}
	panes, err = ensureRepoRecto(rs, basedir, repo, panes)
	if err != nil {
		return rigSession{}, fmt.Errorf("starting %s recto: %w", repo, err)
	}
	if err := promoteRecto(rs, basedir, repo, m); err != nil {
		return rigSession{}, err
	}
	if err := ensureBackgroundRectos(rs, basedir, repo, m); err != nil {
		return rigSession{}, err
	}

	// When resume is invoked from the stopped agent's own pane, replace this
	// process directly. Sending keys there would feed this foreground command,
	// not the shell waiting underneath it.
	selfCaller := agentPane.PaneID == rs.b.CurrentPane()
	rigCaller := filepath.Base(strings.TrimSpace(agentPane.Command)) == filepath.Base(os.Args[0])
	if selfCaller && (rigCaller || isShellCommand(agentPane.Command)) {
		if err := os.Chdir(paneCwd); err != nil {
			return rigSession{}, err
		}
		if err := syscall.Exec("/bin/sh", []string{"sh", "-c", "exec " + command}, os.Environ()); err != nil {
			return rigSession{}, fmt.Errorf("resuming agent in current pane: %w", err)
		}
	}
	if isShellCommand(agentPane.Command) {
		line := "cd " + shellQuote(paneCwd) + " && " + command
		if err := rs.b.SendKeys(agentPane.PaneID, line); err != nil {
			return rigSession{}, fmt.Errorf("resuming agent: %w", err)
		}
	}
	if err := rs.b.SelectPane(agentPane.PaneID); err != nil {
		return rigSession{}, err
	}
	return rs, nil
}

// ensureProjectRuntime repairs the agent-only session used by a project rig.
// It mirrors the agent half of ensureRigRuntime without inventing a fake repo
// or starting Recto in a directory that has no jj workspace.
func ensureProjectRuntime(basedir string, m manifest, prompt string) (rigSession, error) {
	rs := sessionFor(basedir, m)
	command := rigResumeCommand(m, prompt)
	if !rs.live() {
		return rs, spawnProjectSession(rs, basedir, sessionSpec{agent: m.agentKind(), command: command})
	}

	panes, err := rs.panes()
	if err != nil {
		return rigSession{}, err
	}
	var mainWindow, agentPane mux.Pane
	for _, p := range panes {
		if p.WindowRole == rigWindowMain || (mainWindow.WindowID == "" && p.WindowIdx == "0") {
			mainWindow = p
		}
		if p.Role == rigPaneAgent {
			agentPane = p
		}
	}
	if mainWindow.WindowID == "" {
		return rigSession{}, fmt.Errorf("project rig session %s has no main window", rs.name)
	}
	if agentPane.PaneID == "" {
		for _, p := range panes {
			if p.WindowID == mainWindow.WindowID && p.Role != rigPaneRecto {
				agentPane = p
				break
			}
		}
	}
	if agentPane.PaneID == "" {
		pane, err := rs.b.SplitShell(mainWindow.WindowID, basedir)
		if err != nil {
			return rigSession{}, fmt.Errorf("restoring project agent pane: %w", err)
		}
		agentPane = mux.Pane{PaneID: pane, WindowID: mainWindow.WindowID, Command: filepath.Base(os.Getenv("SHELL"))}
	}
	if err := rs.markMainWindow(mainWindow.WindowID, ""); err != nil {
		return rigSession{}, err
	}
	if err := rs.b.RenameWindow(mainWindow.WindowID, mainWindowName("")); err != nil {
		return rigSession{}, err
	}
	if err := rs.markPane(agentPane.PaneID, rigPaneAgent, ""); err != nil {
		return rigSession{}, err
	}

	selfCaller := agentPane.PaneID == rs.b.CurrentPane()
	rigCaller := filepath.Base(strings.TrimSpace(agentPane.Command)) == filepath.Base(os.Args[0])
	if selfCaller && (rigCaller || isShellCommand(agentPane.Command)) {
		if err := os.Chdir(basedir); err != nil {
			return rigSession{}, err
		}
		if err := syscall.Exec("/bin/sh", []string{"sh", "-c", "exec " + command}, os.Environ()); err != nil {
			return rigSession{}, fmt.Errorf("resuming project agent in current pane: %w", err)
		}
	}
	if isShellCommand(agentPane.Command) {
		line := "cd " + shellQuote(basedir) + " && " + command
		if err := rs.b.SendKeys(agentPane.PaneID, line); err != nil {
			return rigSession{}, fmt.Errorf("resuming project agent: %w", err)
		}
	}
	if err := rs.b.SelectPane(agentPane.PaneID); err != nil {
		return rigSession{}, err
	}
	return rs, nil
}

func ensureBackgroundRectos(rs rigSession, basedir, mainRepo string, m manifest) error {
	repos := make([]string, 0, len(m.Repos))
	for repo := range m.Repos {
		if repo != mainRepo && dirExists(filepath.Join(basedir, repo)) {
			repos = append(repos, repo)
		}
	}
	sort.Strings(repos)
	panes, err := rs.panes()
	if err != nil {
		return err
	}
	for _, repo := range repos {
		panes, err = ensureRepoRecto(rs, basedir, repo, panes)
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
