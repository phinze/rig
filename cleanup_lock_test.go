package main

import (
	"errors"
	"testing"
)

// TestMutationLockNonblockingReportsBusy is the regression guard for the radar
// hang: when a rig's lock is already held (a `down` mid-teardown, say), the
// nonblocking mutation lock must return errRigBusy rather than parking forever.
// The radar's Enter path runs after its TUI has torn down, so a blocking wait
// there is an unexplained frozen terminal.
func TestMutationLockNonblockingReportsBusy(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	basedir := t.TempDir()

	held, err := acquireRigLock(basedir, false)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	defer func() { _ = held.Close() }()

	if _, err := acquireRigMutationLockMode(basedir, true); !errors.Is(err, errRigBusy) {
		t.Fatalf("held lock: want errRigBusy, got %v", err)
	}
}

// TestMutationLockNonblockingSucceedsWhenFree confirms the nonblocking path is a
// normal acquire when nobody holds the lock — the busy report is contention
// only, not a blanket refusal.
func TestMutationLockNonblockingSucceedsWhenFree(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	basedir := t.TempDir()

	lock, err := acquireRigMutationLockMode(basedir, true)
	if err != nil {
		t.Fatalf("free lock: want success, got %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
