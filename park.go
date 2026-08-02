package main

import (
	"fmt"
	"os"
	"time"
)

// runPark marks the current rig dormant: it stamps the park time into the
// manifest (so switch hides it and ls shows it as parked) and kills the tmux
// session, so a review-waiting rig also drops out of any tmux-native switcher.
// The basedir and its agent session history stay on disk untouched; `rig wake`
// stands the session back up at the same path. This is the "work's done, up for
// human review" state — no reason to keep the rig in your face until a review
// comes back, at which point you wake it (changes requested) or just merge and
// let reap collect it (approved).
func runPark(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: rig park")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	basedir, err := findBasedir(cwd)
	if err != nil {
		return err
	}
	return setRigParked(basedir, true, false, func(m manifest) {
		// Announce before killing the session: when park runs from inside the rig,
		// that final tmux operation also closes the command's own terminal.
		fmt.Fprintf(os.Stderr, "rig: parked %s — waiting for review; `rig wake %s` brings it back\n", m.ID, m.ID)
	})
}

// setRigParked owns the state transition and its matching session lifecycle.
// Parking stamps the manifest and kills the session; waking clears the stamp
// and ensures a session exists at the basedir. Radar uses a nonblocking lock so
// its TUI can report contention instead of freezing, while the CLI lifecycle
// commands retain their ordinary blocking behavior. afterWrite runs after the
// durable manifest change and before the session operation.
func setRigParked(basedir string, parked, nonblocking bool, afterWrite func(manifest)) error {
	lock, err := acquireRigMutationLockMode(basedir, nonblocking)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	currentlyParked := !m.Parked.IsZero()
	if parked != currentlyParked {
		if parked {
			m.Parked = time.Now()
		} else {
			m.Parked = time.Time{}
		}
		if err := writeManifest(basedir, m); err != nil {
			return err
		}
	}
	if afterWrite != nil {
		afterWrite(m)
	}

	session := tmuxSessionName(basedir)
	if parked {
		// Kill last so a caller running inside this session gets the manifest and
		// any announcement safely onto disk/the terminal first.
		if tmuxHasSession(session) {
			if err := tmuxKillSession(session); err != nil {
				return fmt.Errorf("tmux kill-session %s: %w", session, err)
			}
		}
		return nil
	}
	if !tmuxHasSession(session) {
		if err := tmuxNewSession(session, basedir); err != nil {
			return fmt.Errorf("tmux new-session: %w", err)
		}
	}
	return nil
}
