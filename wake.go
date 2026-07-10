package main

import (
	"fmt"
	"os"
	"sort"
	"time"
)

// runWake reverses rig park: it picks a parked rig (fzf when the arg is ambiguous
// or absent), clears the parked mark, stands the tmux session back up at the same
// basedir — so earlier agent sessions resume from the same cwd — and attaches.
// This is the "a review came back, back to work" path; a rig that was merged
// instead never needs waking, reap collects it.
func runWake(args []string) error {
	rigs, err := listRigs()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	statuses := rigStatuses(rigs, home, time.Now())

	parked := statuses[:0]
	for _, s := range statuses {
		if s.Parked {
			parked = append(parked, s)
		}
	}
	statuses = parked
	if len(statuses) == 0 {
		return fmt.Errorf("no parked rigs")
	}
	// Newest first, so the freshest parked rig tops the picker.
	sort.SliceStable(statuses, func(i, j int) bool {
		return statuses[i].Created.After(statuses[j].Created)
	})

	chosen, err := pickRigStatus(statuses, args, "wake rig: ")
	if err != nil {
		return err
	}
	if chosen == nil {
		return nil
	}

	m, err := readManifest(chosen.Path)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	m.Parked = time.Time{}
	if err := writeManifest(chosen.Path, m); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rig: woke %s\n", m.ID)

	session := tmuxSessionName(chosen.Path)
	if !tmuxHasSession(session) {
		// Park killed it; stand a bare one back up at the same path so the
		// earlier agent sessions are a resume away.
		if err := tmuxNewSession(session, chosen.Path); err != nil {
			return fmt.Errorf("tmux new-session: %w", err)
		}
	}
	return attachOrReport(session)
}
