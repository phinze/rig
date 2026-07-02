package main

import (
	"fmt"
	"os"
	"time"
)

// runPark marks the current rig dormant: it stamps the park time into the
// manifest (so switch hides it and ls shows it as parked) and kills the tmux
// session, so a review-waiting rig also drops out of any tmux-native switcher.
// The basedir and its claude session history stay on disk untouched; `rig wake`
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
	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}

	if m.Parked.IsZero() {
		m.Parked = time.Now()
		if err := writeManifest(basedir, m); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "rig: parked %s — waiting for review; `rig wake %s` brings it back\n", m.ID, m.ID)

	// Kill the session last: if we're running inside it, the manifest is already
	// written by the time the SIGHUP closes our terminal.
	session := tmuxSessionName(basedir)
	if tmuxHasSession(session) {
		if err := tmuxKillSession(session); err != nil {
			return fmt.Errorf("tmux kill-session %s: %w", session, err)
		}
	}
	return nil
}
