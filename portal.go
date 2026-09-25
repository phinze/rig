package main

import (
	"os"

	"github.com/phinze/rig/internal/mux"
	"github.com/phinze/rig/internal/mux/tmux"
)

// A portal is how a tmux server on another host shows up in this machine's
// Rex: one Rex session per host, running an ssh into that host's tmux. It's
// the bridge for a host that keeps tmux while this one moves to Rex, at the
// scale of one view per tmux server rather than one per rig (a wrapper
// session per rig was sketched and dropped for exactly that).
//
// Entering a row on such a surface is an upsert. If the portal is there and
// its client is still attached, rig moves that client with switch-client -c,
// which tmux allows from anywhere, and hops Rex to the portal. If it's
// missing, or its ssh has exited, rig (re)creates it already attached to the
// target, and hops. You never have to set one up by hand, and a dead one
// heals on the next Enter.
type portalBackend struct {
	tmux.Backend
}

// portalLabel is the portal's Rex session label. Labels are what the picker
// hop matches, and this one only needs to be unique on this machine.
func (p portalBackend) portalLabel() string { return "tmux-" + p.Place }

func (p portalBackend) Attach(target string) error {
	// Only Rex can host a portal. From a bare terminal the remote attach is
	// just ssh -t, and from inside tmux the client to move is somewhere else.
	if os.Getenv("REX_SESSION") == "" {
		return p.Backend.Attach(target)
	}
	rx := localRex()
	if rx == nil {
		return mux.ErrNoClientSwitch
	}
	label := p.portalLabel()
	if rx.HasSession(label) {
		if tty, err := p.PortalTTY(); err == nil {
			if err := p.SwitchClient(tty, target); err != nil {
				return err
			}
			return rx.Attach(label)
		}
		// The session outlived its client: the ssh exited, or the host
		// restarted. Replace it rather than hop to a dead terminal.
		if err := rx.KillSession(label); err != nil {
			return err
		}
	}
	home, _ := os.UserHomeDir()
	if _, _, err := rx.NewCommandSession(label, p.Place, home, p.PortalCommand(target)); err != nil {
		return err
	}
	return rx.Attach(label)
}

// portalHost is what a portal needs from this machine's multiplexer: the
// ordinary backend plus a session that runs one command. Only Rex is one.
type portalHost interface {
	mux.Backend
	NewCommandSession(name, windowName, cwd, cmdline string) (pane, window string, err error)
}

// localRex is this machine's own Rex backend, or nil where there isn't one.
// A var so a test can hand the portal a fake instead of the real app.
var localRex = func() portalHost {
	for _, b := range localBackends() {
		if h, ok := b.(portalHost); ok && b.Name() == "rex" {
			return h
		}
	}
	return nil
}
