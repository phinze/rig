package main

import (
	"fmt"
	"os"
	"testing"
)

// TestMain keeps every test, including subprocess e2e tests and their
// systemd-run teardown workers, away from the host's real user cgroup tree.
// A dedicated tmux socket is not enough isolation: transient tmux pane scopes
// still live together under the user manager.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "rig-test-cgroup-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "rig tests: create cgroup sandbox: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("RIG_CGROUP_ROOT", root); err != nil {
		fmt.Fprintf(os.Stderr, "rig tests: set cgroup sandbox: %v\n", err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	// CLAUDE_CONFIG_DIR outranks HOME when rig finds ~/.claude.json, so a
	// developer who sets it would have every test that fakes HOME seeding and
	// dropping trust in their real claude config instead.
	if err := os.Unsetenv("CLAUDE_CONFIG_DIR"); err != nil {
		fmt.Fprintf(os.Stderr, "rig tests: unset CLAUDE_CONFIG_DIR: %v\n", err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}

	// The suite must never reach another host. Remote resolution finds hosts
	// through the [surfaces] table, so config points at an empty dir unless a
	// test sets its own; otherwise a developer with XDG_CONFIG_HOME set would
	// have every lookup miss fall through to their real devbox.
	cfg, err := os.MkdirTemp("", "rig-test-config-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "rig tests: %v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv("XDG_CONFIG_HOME", cfg)

	// State gets the same treatment, because the radar reads the real notify
	// store on every scan. A live notification on the developer's machine drew
	// an inbox banner that pushed the board two rows down, and the mouse tests
	// failed for as long as nix-config-sync had something to say.
	state, err := os.MkdirTemp("", "rig-test-state-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "rig tests: %v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv("XDG_STATE_HOME", state)

	code := m.Run()
	_ = os.RemoveAll(root)
	_ = os.RemoveAll(cfg)
	_ = os.RemoveAll(state)
	os.Exit(code)
}
