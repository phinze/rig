package main

import (
	"os"

	"github.com/phinze/rig/internal/mux"
	"github.com/phinze/rig/internal/mux/tmux"
)

// A portal is how a tmux server shows up in this machine's Rex: one Rex
// session per server, running a tmux client attached to it (over ssh when the
// server is on another host). It's the bridge while tmux and Rex coexist, at
// the scale of one view per tmux server rather than one per rig (a wrapper
// session per rig was sketched and dropped for exactly that). This machine's
// own tmux gets one too, so from Rex every tmux server is entered the same
// way, and a radar opened over a portal is a Rex layer over a Rex session
// rather than something that inherited TMUX and thinks it's inside tmux.
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

// localTmux is this machine's tmux as rig drives it everywhere: the plain
// tmux backend, with portal behaviour for when it's entered from Rex. Every
// place that means "the local tmux" goes through here, rig manifests that say
// tmux (or nothing) included, so none of them attaches a tmux client inside
// whatever Rex block happened to ask.
func localTmux() portalBackend { return portalBackend{tmux.Backend{}} }

// portalLabel is the portal's Rex session label. Labels are what the picker
// hop matches, and this one only needs to be unique on this machine.
func (p portalBackend) portalLabel() string { return "tmux-" + p.place() }

// place is where the portal's tmux server lives: the configured place for a
// surface elsewhere, "local" for this machine's own.
func (p portalBackend) place() string {
	if p.Place == "" {
		return "local"
	}
	return p.Place
}

func (p portalBackend) Attach(target string) error {
	adoptLocalPortal()
	// Only Rex can host a portal. From a bare terminal the tmux backend
	// attaches (ssh -t for a remote server); from inside this machine's tmux
	// the client to move is the one we're in, which switch-client already
	// handles, so a local server skips the portal there too.
	if os.Getenv("REX_SESSION") == "" || (p.Host == "" && os.Getenv("TMUX") != "") {
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
	if _, _, err := rx.NewCommandSession(label, p.place(), home, p.PortalCommand(target)); err != nil {
		return err
	}
	return rx.Attach(label)
}

// portalHost is what a portal needs from this machine's multiplexer: the
// ordinary backend plus a session that runs one command. Only Rex is one.
type portalHost interface {
	mux.Backend
	NewCommandSession(name, windowName, cwd, cmdline string) (pane, window string, err error)
	SessionID(name string) (string, error)
}

// adoptLocalPortal gives a process inside this machine's tmux the Rex
// session it's really on screen in, when that's the local portal. tmux hands
// its panes and popups TMUX but not REX_SESSION, so without this a radar
// opened with tmux's own leader-r inside tmux-local sees tmux and nothing
// else, and can't reach a portal to another host. It only adopts when this
// process's own tmux client is the one the local portal stamped, so a tmux in
// a plain terminal still looks like what it is.
func adoptLocalPortal() {
	if os.Getenv("REX_SESSION") != "" || os.Getenv("TMUX") == "" {
		return
	}
	rx := localRex()
	if rx == nil {
		return
	}
	lt := localTmux()
	tty, err := lt.PortalTTY()
	if err != nil || lt.ClientTTY() != tty {
		return
	}
	if id, err := rx.SessionID(lt.portalLabel()); err == nil {
		_ = os.Setenv("REX_SESSION", id)
	}
}

// portalSessions is the key each portal's Rex session would have: one per
// tmux server rig can reach, whether or not the portal exists yet.
func portalSessions() map[sessionKey]bool {
	rx := localRex()
	if rx == nil {
		return nil
	}
	out := map[sessionKey]bool{}
	for _, b := range knownBackends() {
		if p, ok := b.(portalBackend); ok {
			out[sessionKey{rx.Surface(), p.portalLabel()}] = true
		}
	}
	return out
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
