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

	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
