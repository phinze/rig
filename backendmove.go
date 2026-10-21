package main

import (
	"fmt"
	"os"
	"time"
)

// runBackendMove moves a rig to another multiplexer on this machine: it
// records where the rig's conversation is, rewrites the manifest's backend,
// kills the old session, and builds a fresh one on the new backend that
// resumes the same conversation. Nothing else is tied to the session. The
// agent's history lives in its own store keyed by the rig's directory, Recto's
// state by the workspace, and the panes' marks are recreated on spawn.
//
// It's the escape hatch between tmux and Rex while both are in use: a rig that
// hits a wall in one can be moved to the other without losing its agent. The
// in-session rule is park's: a caller inside the session it would kill needs
// a terminal to pick where to land, so an agent can't move its own rig, but
// anything outside it (another rig's agent, a shell elsewhere) can.
func runBackendMove(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: rig backend tmux|rex [NAME]")
	}
	target, err := backendByName(args[0])
	if err != nil {
		return err
	}
	var basedir string
	if len(args) == 2 {
		rigs, err := listRigs()
		if err != nil {
			return err
		}
		local, localErr := downTarget(rigs, args[1])
		t, ok, err := exactRemote(args[1], localErr == nil)
		if err != nil {
			return err
		}
		if ok {
			return forward(t, "backend", args[0], t.row.ID)
		}
		if localErr != nil {
			return localErr
		}
		basedir = local
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		if basedir, err = findBasedir(cwd); err != nil {
			return err
		}
	}

	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	from := sessionFor(basedir, m)
	if from.b.Name() == target.Name() {
		fmt.Fprintf(os.Stderr, "rig: %s is already on %s\n", m.ID, target.Name())
		return nil
	}
	inside := insideSession(from)
	proceed, err := sessionExitHandoff(inside)
	if err != nil {
		return err
	}
	if !proceed {
		return nil
	}

	rs, live, err := moveRigBackend(basedir, target.Name())
	if err != nil {
		return err
	}
	switch {
	case !live:
		fmt.Fprintf(os.Stderr, "rig: %s will open on %s when it wakes\n", m.ID, target.Name())
	case stdinIsTTY() && !inside:
		// A person asked from a terminal: take them there. An agent elsewhere
		// (a chief of staff) moving a rig shouldn't move anyone's screen.
		return rs.attach()
	default:
		fmt.Fprintf(os.Stderr, "rig: %s moved to %s; `rig switch %s` to enter it\n", m.ID, target.Name(), m.ID)
	}
	return nil
}

// buildMovedRuntime builds a moved rig's new session; a var so a test can
// watch for it without a real multiplexer.
var buildMovedRuntime = ensureRigRuntime

// moveRigBackend switches basedir's rig to backend under the rig lock. A live
// rig's old session is killed only after its conversation is recorded, and
// the new one resumes it, so there's never a second agent on the same
// conversation. A parked rig only has its manifest changed. It reports the
// rig's new session and whether it was built.
func moveRigBackend(basedir, backend string) (rigSession, bool, error) {
	lock, err := acquireRigMutationLockMode(basedir, false)
	if err != nil {
		return rigSession{}, false, err
	}
	defer func() { _ = lock.Close() }()

	m, err := readManifest(basedir)
	if err != nil {
		return rigSession{}, false, fmt.Errorf("reading manifest: %w", err)
	}
	from := sessionFor(basedir, m)
	live := m.Parked.IsZero() && from.live()

	m.Touched = time.Now()
	captureRigRuntimeHints(basedir, &m, live)
	m.Backend = backend
	if err := writeManifest(basedir, m); err != nil {
		return rigSession{}, false, err
	}
	to := sessionFor(basedir, m)
	if !live {
		return to, false, nil
	}
	if err := from.b.KillSession(from.name); err != nil {
		return to, false, fmt.Errorf("killing %s session %s: %w", from.b.Name(), from.name, err)
	}
	if _, err := buildMovedRuntime(basedir, m); err != nil {
		return to, false, fmt.Errorf("building the %s session: %w", backend, err)
	}
	return to, true, nil
}
