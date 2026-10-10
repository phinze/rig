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
	if t := rig.remote; t != nil {
		return forward(*t, "dispatch", t.row.ID, "--", prompt)
	}
	return dispatchRig(*rig.local, prompt)
}

func resolveDispatchRig(query string) (rigTarget, error) {
	t, err := resolveTarget(query)
	if err != nil {
		return rigTarget{}, err
	}
	kind := ""
	if t.local != nil {
		kind = t.local.Kind
	} else {
		kind = t.remote.row.Kind
	}
	if kind == "project" {
		return rigTarget{}, fmt.Errorf("cannot dispatch a project overview rig")
	}
	return t, nil
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

	rs := sessionFor(r.Path, m)
	if rs.live() {
		panes, err := adoptLegacyRigPanes(rs, r.Path, m)
		if err != nil {
			return fmt.Errorf("inspecting %s agent: %w", m.ID, err)
		}
		for _, pane := range panes {
			if pane.Role == rigPaneAgent && !isShellCommand(pane.Command) {
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
