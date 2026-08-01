package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	rigPaneRoleOption   = "@rig-pane-role"
	rigPaneRepoOption   = "@rig-pane-repo"
	rigWindowRoleOption = "@rig-window-role"
	rigWindowRepoOption = "@rig-window-repo"

	rigPaneAgent  = "agent"
	rigPaneRecto  = "recto"
	rigWindowMain = "main"
	rigWindowRepo = "repo"
)

type rigTmuxPane struct {
	PaneID     string
	WindowID   string
	WindowIdx  string
	WindowName string
	PaneRole   string
	PaneRepo   string
	WindowRole string
	WindowRepo string
	Command    string
	Path       string
}

func mainWindowName(repo string) string {
	if repo == "" {
		return "main"
	}
	return "main/" + repo
}

func markRigPane(pane, role, repo string) error {
	if err := tmuxSetPaneOption(pane, rigPaneRoleOption, role); err != nil {
		return err
	}
	return tmuxSetPaneOption(pane, rigPaneRepoOption, repo)
}

func markRigMainWindow(window, repo string) error {
	if err := tmuxSetWindowOption(window, rigWindowRoleOption, rigWindowMain); err != nil {
		return err
	}
	return tmuxSetWindowOption(window, rigWindowRepoOption, repo)
}

func markRigRepoWindow(window, repo string) error {
	if err := tmuxSetWindowOption(window, rigWindowRoleOption, rigWindowRepo); err != nil {
		return err
	}
	return tmuxSetWindowOption(window, rigWindowRepoOption, repo)
}

func tmuxRigPanes(session string) ([]rigTmuxPane, error) {
	format := strings.Join([]string{
		"#{pane_id}", "#{window_id}", "#{window_index}", "#{window_name}",
		"#{@rig-pane-role}", "#{@rig-pane-repo}",
		"#{@rig-window-role}", "#{@rig-window-repo}",
		"#{pane_current_command}", "#{pane_current_path}",
	}, "\t")
	cmd := exec.Command("tmux", "list-panes", "-s", "-t", session, "-F", format)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var panes []rigTmuxPane
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 10)
		if len(f) != 10 {
			continue
		}
		panes = append(panes, rigTmuxPane{
			PaneID: f[0], WindowID: f[1], WindowIdx: f[2], WindowName: f[3],
			PaneRole: f[4], PaneRepo: f[5], WindowRole: f[6], WindowRepo: f[7],
			Command: f[8], Path: f[9],
		})
	}
	return panes, nil
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
func adoptLegacyRigPanes(session, basedir string, m manifest) ([]rigTmuxPane, error) {
	panes, err := tmuxRigPanes(session)
	if err != nil {
		return nil, err
	}
	mainWindow := ""
	mainRepo := ""
	for _, p := range panes {
		repo := p.PaneRepo
		if repo == "" {
			repo = repoForWorkspacePath(basedir, m, p.Path)
		}
		if p.PaneRole == "" && (p.Command == "recto" || strings.HasPrefix(p.Command, "recto")) {
			_ = markRigPane(p.PaneID, rigPaneRecto, repo)
		}
		if p.PaneRole == "" && isAgentCommand(p.Command) {
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
	_ = tmuxRenameWindow(mainWindow, mainWindowName(mainRepo))
	for _, p := range panes {
		if p.WindowID == mainWindow || p.WindowRole != "" {
			continue
		}
		if repo := repoForWorkspacePath(basedir, m, p.Path); repo != "" {
			_ = markRigRepoWindow(p.WindowID, repo)
		}
	}
	return tmuxRigPanes(session)
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

func findMainParts(panes []rigTmuxPane) (window, agent, recto rigTmuxPane, err error) {
	for _, p := range panes {
		if p.WindowRole != rigWindowMain {
			continue
		}
		window = p
		if p.PaneRole == rigPaneAgent {
			agent = p
		}
		if p.PaneRole == rigPaneRecto {
			recto = p
		}
	}
	if window.WindowID == "" || agent.PaneID == "" {
		return window, agent, recto, fmt.Errorf("rig session lacks a marked main/agent pane")
	}
	return window, agent, recto, nil
}

func findRepoWindow(panes []rigTmuxPane, repo, exceptWindow string) (rigTmuxPane, bool) {
	for _, p := range panes {
		if p.WindowID != exceptWindow && p.WindowRole == rigWindowRepo && p.WindowRepo == repo {
			return p, true
		}
	}
	return rigTmuxPane{}, false
}

func findRectoPane(panes []rigTmuxPane, repo string) (rigTmuxPane, bool) {
	for _, p := range panes {
		if p.PaneRole == rigPaneRecto && p.PaneRepo == repo {
			return p, true
		}
	}
	return rigTmuxPane{}, false
}

// rectoCommand is how every Recto in a rig starts, review or authoring. `--pr`
// picks the trunk merge-base as the opening base instead of `@-`, which is the
// rig's own unit of attention: a rig is one task, and the task's diff is the
// whole stack that becomes the PR, not just whatever commit is on top right now.
// The two agree at rig birth (a fresh workspace sits on trunk, so merge-base ==
// `@-` == trunk) and only diverge once the stack is two deep — exactly when you
// want the wider view. It costs nothing to be wrong about: `--pr` moves the
// starting index within Recto's base ring rather than changing the ring, so `b`
// cycles back to `@-` in one keystroke. And it's the richer starting point, not
// merely the wider one — Recto's per-rev narrowing enumerates `base..@`, so from
// the merge-base you can drill into any commit in the stack, while from `@-` you
// have one rev and must cycle the base to see anything else.
func rectoCommand() string {
	return "recto --pr"
}

func ensureRepoRecto(session, basedir, repo string, panes []rigTmuxPane) ([]rigTmuxPane, error) {
	if _, ok := findRectoPane(panes, repo); ok {
		return panes, nil
	}
	repoDir := filepath.Join(basedir, repo)
	if w, ok := findRepoWindow(panes, repo, ""); ok {
		pane, err := tmuxSplitHID(w.PaneID, repoDir, rectoCommand())
		if err != nil {
			return nil, err
		}
		if err := markRigPane(pane, rigPaneRecto, repo); err != nil {
			return nil, err
		}
	} else {
		pane, window, err := tmuxNewCommandWindow(session, repo, repoDir, rectoCommand())
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
	return tmuxRigPanes(session)
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
		return tmuxRenameWindow(main.WindowID, mainWindowName(repo))
	}

	if current.PaneID != "" {
		outgoing := current.PaneRepo
		if outgoing == "" {
			outgoing = repoForWorkspacePath(basedir, m, current.Path)
		}
		if parking, ok := findRepoWindow(panes, outgoing, main.WindowID); ok {
			if err := tmuxJoinPane(current.PaneID, parking.PaneID); err != nil {
				return fmt.Errorf("parking %s recto: %w", outgoing, err)
			}
			_ = markRigRepoWindow(parking.WindowID, outgoing)
			_ = tmuxRenameWindow(parking.WindowID, outgoing)
		} else {
			window, err := tmuxBreakPane(current.PaneID, outgoing)
			if err != nil {
				return fmt.Errorf("parking %s recto: %w", outgoing, err)
			}
			if err := markRigRepoWindow(window, outgoing); err != nil {
				return err
			}
		}
	}

	if err := tmuxJoinPane(target.PaneID, agent.PaneID); err != nil {
		return fmt.Errorf("promoting %s recto: %w", repo, err)
	}
	if err := markRigMainWindow(main.WindowID, repo); err != nil {
		return err
	}
	if err := tmuxRenameWindow(main.WindowID, mainWindowName(repo)); err != nil {
		return err
	}
	return tmuxSelectPane(agent.PaneID)
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
	session := tmuxSessionName(basedir)
	if !tmuxHasSession(session) {
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
