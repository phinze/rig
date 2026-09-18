package main

import (
	"fmt"
	"github.com/phinze/rig/internal/mux"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	rigPaneAgent  = "agent"
	rigPaneRecto  = "recto"
	rigWindowMain = "main"
	rigWindowRepo = "repo"
)

func mainWindowName(repo string) string {
	if repo == "" {
		return "main"
	}
	return "main/" + repo
}

func markRigPane(pane, role, repo string) error {
	return backend.MarkPane(pane, role, repo)
}

func markRigMainWindow(window, repo string) error {
	return backend.MarkWindow(window, rigWindowMain, repo)
}

func markRigRepoWindow(window, repo string) error {
	return backend.MarkWindow(window, rigWindowRepo, repo)
}

func repoForWorkspacePath(basedir string, m manifest, path string) string {
	rel, err := filepath.Rel(basedir, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	repo, _, _ := strings.Cut(rel, string(filepath.Separator))
	if m.Repos[repo] == "" {
		return ""
	}
	return repo
}

// adoptLegacyRigPanes makes the carousel usable in sessions created before
// pane metadata existed. Process/path discovery is deliberately only a
// migration path; all newly created panes carry stable tmux user-options.
func adoptLegacyRigPanes(session, basedir string, m manifest) ([]mux.Pane, error) {
	panes, err := backend.Panes(session)
	if err != nil {
		return nil, err
	}
	mainWindow := ""
	mainRepo := ""
	for _, p := range panes {
		repo := p.Repo
		if repo == "" {
			repo = repoForWorkspacePath(basedir, m, p.Path)
		}
		if p.Role == "" && (p.Command == "recto" || strings.HasPrefix(p.Command, "recto")) {
			_ = markRigPane(p.PaneID, rigPaneRecto, repo)
		}
		if p.Role == "" && isAgentCommand(p.Command) {
			_ = markRigPane(p.PaneID, rigPaneAgent, repo)
		}
		if p.WindowRole == rigWindowMain || (mainWindow == "" && isAgentCommand(p.Command)) {
			mainWindow = p.WindowID
			mainRepo = repo
		}
	}
	if mainWindow == "" {
		for _, p := range panes {
			if p.WindowIdx == "0" {
				mainWindow = p.WindowID
				mainRepo = repoForWorkspacePath(basedir, m, p.Path)
				break
			}
		}
	}
	if mainWindow == "" {
		return nil, fmt.Errorf("cannot find main window in tmux session %s", session)
	}
	for _, p := range panes {
		if p.WindowID == mainWindow && p.Command == "recto" {
			if repo := repoForWorkspacePath(basedir, m, p.Path); repo != "" {
				mainRepo = repo
			}
		}
	}
	_ = markRigMainWindow(mainWindow, mainRepo)
	_ = backend.RenameWindow(mainWindow, mainWindowName(mainRepo))
	for _, p := range panes {
		if p.WindowID == mainWindow || p.WindowRole != "" {
			continue
		}
		if repo := repoForWorkspacePath(basedir, m, p.Path); repo != "" {
			_ = markRigRepoWindow(p.WindowID, repo)
		}
	}
	return backend.Panes(session)
}

func resolveRectoRepo(m manifest, arg string) (string, error) {
	if _, ok := m.Repos[arg]; ok {
		return arg, nil
	}
	var matches []string
	for repo, nwo := range m.Repos {
		_, short, _ := strings.Cut(nwo, "/")
		if arg == nwo || arg == short {
			matches = append(matches, repo)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("repo %q is not in this rig", arg)
	default:
		return "", fmt.Errorf("repo %q is ambiguous: %s", arg, strings.Join(matches, ", "))
	}
}

func findMainParts(panes []mux.Pane) (window, agent, recto mux.Pane, err error) {
	for _, p := range panes {
		if p.WindowRole != rigWindowMain {
			continue
		}
		window = p
		if p.Role == rigPaneAgent {
			agent = p
		}
		if p.Role == rigPaneRecto {
			recto = p
		}
	}
	if window.WindowID == "" || agent.PaneID == "" {
		return window, agent, recto, fmt.Errorf("rig session lacks a marked main/agent pane")
	}
	return window, agent, recto, nil
}

func findRepoWindow(panes []mux.Pane, repo, exceptWindow string) (mux.Pane, bool) {
	for _, p := range panes {
		if p.WindowID != exceptWindow && p.WindowRole == rigWindowRepo && p.WindowRepo == repo {
			return p, true
		}
	}
	return mux.Pane{}, false
}

func findRectoPane(panes []mux.Pane, repo string) (mux.Pane, bool) {
	for _, p := range panes {
		if p.Role == rigPaneRecto && p.Repo == repo {
			return p, true
		}
	}
	return mux.Pane{}, false
}

// rectoCommand is how every Recto in a rig starts, review or authoring. Recto
// opens at the branch point by default, which shows the task's whole stack
// rather than only the top commit. Keep the command centralized so creation,
// add, resume, and resurrection cannot drift apart again.
func rectoCommand() string {
	return "recto"
}

func ensureRepoRecto(session, basedir, repo string, panes []mux.Pane) ([]mux.Pane, error) {
	if _, ok := findRectoPane(panes, repo); ok {
		return panes, nil
	}
	repoDir := filepath.Join(basedir, repo)
	if w, ok := findRepoWindow(panes, repo, ""); ok {
		pane, err := backend.SplitCommand(w.PaneID, repoDir, rectoCommand())
		if err != nil {
			return nil, err
		}
		if err := markRigPane(pane, rigPaneRecto, repo); err != nil {
			return nil, err
		}
	} else {
		pane, window, err := backend.NewCommandWindow(session, repo, repoDir, rectoCommand())
		if err != nil {
			return nil, err
		}
		if err := markRigPane(pane, rigPaneRecto, repo); err != nil {
			return nil, err
		}
		if err := markRigRepoWindow(window, repo); err != nil {
			return nil, err
		}
	}
	return backend.Panes(session)
}

func promoteRecto(session, basedir, repo string, m manifest) error {
	panes, err := adoptLegacyRigPanes(session, basedir, m)
	if err != nil {
		return err
	}
	panes, err = ensureRepoRecto(session, basedir, repo, panes)
	if err != nil {
		return fmt.Errorf("starting %s recto: %w", repo, err)
	}
	main, agent, current, err := findMainParts(panes)
	if err != nil {
		return err
	}
	target, ok := findRectoPane(panes, repo)
	if !ok {
		return fmt.Errorf("cannot find %s recto pane", repo)
	}
	if target.WindowID == main.WindowID {
		_ = markRigMainWindow(main.WindowID, repo)
		return backend.RenameWindow(main.WindowID, mainWindowName(repo))
	}

	if current.PaneID != "" {
		outgoing := current.Repo
		if outgoing == "" {
			outgoing = repoForWorkspacePath(basedir, m, current.Path)
		}
		if parking, ok := findRepoWindow(panes, outgoing, main.WindowID); ok {
			if err := backend.JoinPane(current.PaneID, parking.PaneID); err != nil {
				return fmt.Errorf("parking %s recto: %w", outgoing, err)
			}
			_ = markRigRepoWindow(parking.WindowID, outgoing)
			_ = backend.RenameWindow(parking.WindowID, outgoing)
		} else {
			window, err := backend.BreakPane(current.PaneID, outgoing)
			if err != nil {
				return fmt.Errorf("parking %s recto: %w", outgoing, err)
			}
			if err := markRigRepoWindow(window, outgoing); err != nil {
				return err
			}
		}
	}

	if err := backend.JoinPane(target.PaneID, agent.PaneID); err != nil {
		return fmt.Errorf("promoting %s recto: %w", repo, err)
	}
	if err := markRigMainWindow(main.WindowID, repo); err != nil {
		return err
	}
	if err := backend.RenameWindow(main.WindowID, mainWindowName(repo)); err != nil {
		return err
	}
	return backend.SelectPane(agent.PaneID)
}

// runRecto promotes a repository's persistent viewer into main's right-hand
// hot seat. Any remaining arguments are delegated to Recto from that repo, so
// agents have one semantic operation for both "show cloud" and "focus this
// cloud span" without learning the tmux choreography underneath.
func runRecto(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: rig recto <repo> [ping|focus|annotate|clear ...]")
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
	repo, err := resolveRectoRepo(m, args[0])
	if err != nil {
		return err
	}
	session := rigSessionName(basedir)
	if !backend.HasSession(session) {
		return fmt.Errorf("rig session is not running")
	}
	if err := promoteRecto(session, basedir, repo, m); err != nil {
		return err
	}
	if len(args) == 1 {
		return nil
	}
	rectoArgs := append([]string{"-R", filepath.Join(basedir, repo)}, args[1:]...)
	cmd := exec.Command("recto", rectoArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// forgetRectoWorkspace asks Recto to remove its own authored state. The
// executable is optional and the storage contract stays entirely on Recto's
// side of this CLI boundary.
func forgetRectoWorkspace(workspace string) error {
	if _, err := exec.LookPath("recto"); err != nil {
		return nil
	}
	out, err := exec.Command(
		"recto", "state", "forget", "--workspace-root", workspace,
	).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return fmt.Errorf("%w: %s", err, detail)
		}
		return err
	}
	return nil
}
