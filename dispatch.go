package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// runDispatch wakes or repairs a stopped task rig and supplies its resumed
// agent with a concrete next assignment. It never attaches or switches tmux;
// project overview agents use it as a background orchestration primitive.
func runDispatch(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: rig dispatch <rig-or-issue> <prompt>")
	}
	target := args[0]
	promptArgs := args[1:]
	if len(promptArgs) > 0 && promptArgs[0] == "--" {
		promptArgs = promptArgs[1:]
	}
	prompt := strings.TrimSpace(strings.Join(promptArgs, " "))
	if prompt == "" {
		return fmt.Errorf("rig dispatch needs a non-empty prompt")
	}
	rig, err := resolveDispatchRig(target)
	if err != nil {
		return err
	}
	return dispatchRig(rig, prompt)
}

func resolveDispatchRig(query string) (rigInfo, error) {
	rigs, err := listRigs()
	if err != nil {
		return rigInfo{}, err
	}
	var matches []rigInfo
	for _, r := range rigs {
		if strings.EqualFold(r.ID, query) || strings.EqualFold(r.Slug, query) ||
			(r.TrackerID != "" && strings.EqualFold(r.TrackerID, query)) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return rigInfo{}, fmt.Errorf("no rig matches %q", query)
	case 1:
		if matches[0].Kind == "project" {
			return rigInfo{}, fmt.Errorf("cannot dispatch a project overview rig")
		}
		return matches[0], nil
	default:
		return rigInfo{}, fmt.Errorf("rig query %q is ambiguous", query)
	}
}

func dispatchRig(r rigInfo, prompt string) error {
	lock, err := acquireRigMutationLock(r.Path)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	m, err := readManifest(r.Path)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	if m.isProject() {
		return fmt.Errorf("cannot dispatch a project overview rig")
	}

	session := tmuxSessionName(r.Path)
	if tmuxHasSession(session) {
		panes, err := adoptLegacyRigPanes(session, r.Path, m)
		if err != nil {
			return fmt.Errorf("inspecting %s agent: %w", m.ID, err)
		}
		for _, pane := range panes {
			if pane.PaneRole == rigPaneAgent && !isShellCommand(pane.Command) {
				return fmt.Errorf("%s already has a running agent; enter the rig to hand it more work", m.ID)
			}
		}
	}

	m.Parked = time.Time{}
	m.Touched = time.Now()
	captureRigRuntimeHints(r.Path, &m, false)
	if err := writeManifest(r.Path, m); err != nil {
		return err
	}
	if _, err := ensureRigRuntimeWithPrompt(r.Path, m, prompt); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rig: dispatched %s — agent resumed in background\n", m.ID)
	return nil
}
