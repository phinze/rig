package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phinze/rig/internal/mux"
	"github.com/phinze/rig/internal/mux/tmux"
)

// fakePortalHost stands in for this machine's Rex: it records what the portal
// asked of it instead of driving the app.
type fakePortalHost struct {
	fakeBackend
	created  []string // label + " :: " + cmdline
	killed   []string
	attached []string
}

func (f *fakePortalHost) NewCommandSession(name, windowName, cwd, cmdline string) (string, string, error) {
	f.created = append(f.created, name+" :: "+cmdline)
	return "block:1", "window:1", nil
}
func (f *fakePortalHost) SessionID(name string) (string, error) {
	if f.HasSession(name) {
		return "session:fake-" + name, nil
	}
	return "", os.ErrNotExist
}
func (f *fakePortalHost) KillSession(name string) error {
	f.killed = append(f.killed, name)
	return nil
}
func (f *fakePortalHost) Attach(target string) error {
	f.attached = append(f.attached, target)
	return nil
}

// fakeSSH answers the tmux calls a portal makes, whether they arrive over ssh
// (one quoted command line) or straight to a local tmux (separate args): the
// stamped tty, the attached clients, and switch-client, logging each call.
func fakeSSH(t *testing.T, stamped, clients string) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "calls.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + log + `"
case "$*" in
*show-options*) echo '` + stamped + `' ;;
*display-message*) echo '` + stamped + `' ;;
*list-clients*) printf '%s\n' ` + clients + ` ;;
esac
`
	for _, name := range []string{"ssh", "tmux"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return log
}

func withPortalHost(t *testing.T, h *fakePortalHost) {
	t.Helper()
	saved := localRex
	localRex = func() portalHost { return h }
	t.Cleanup(func() { localRex = saved })
	t.Setenv("REX_SESSION", "session:here")
}

func TestPortalUpsert(t *testing.T) {
	p := portalBackend{tmux.Backend{Host: "fox", Place: "fox"}}
	const target = "~/workspaces/foo:0.1"

	t.Run("missing portal is created attached to the target", func(t *testing.T) {
		h := &fakePortalHost{}
		withPortalHost(t, h)
		fakeSSH(t, "", "")
		if err := p.Attach(target); err != nil {
			t.Fatal(err)
		}
		if len(h.created) != 1 || !strings.HasPrefix(h.created[0], "tmux-fox :: ssh -t 'fox' ") ||
			!strings.Contains(h.created[0], "attach-session") || !strings.Contains(h.created[0], target) ||
			!strings.Contains(h.created[0], "@rig-portal-tty") {
			t.Errorf("created %v, want one portal attaching to %s and stamping its tty", h.created, target)
		}
		if len(h.attached) != 1 || h.attached[0] != "tmux-fox" {
			t.Errorf("hopped to %v, want the portal", h.attached)
		}
	})

	t.Run("live portal is switched, not recreated", func(t *testing.T) {
		h := &fakePortalHost{fakeBackend: fakeBackend{sessions: sessionsNamed("tmux-fox")}}
		withPortalHost(t, h)
		log := fakeSSH(t, "/dev/pts/7", "/dev/pts/1 /dev/pts/7")
		if err := p.Attach(target); err != nil {
			t.Fatal(err)
		}
		if len(h.created) != 0 || len(h.killed) != 0 {
			t.Errorf("created %v killed %v, want the live portal reused", h.created, h.killed)
		}
		raw, _ := os.ReadFile(log)
		if !strings.Contains(string(raw), "'switch-client' '-c' '/dev/pts/7' '-t' '"+target+"'") {
			t.Errorf("remote calls:\n%s\nwant switch-client of the stamped tty to the target", raw)
		}
		if len(h.attached) != 1 || h.attached[0] != "tmux-fox" {
			t.Errorf("hopped to %v, want the portal", h.attached)
		}
	})

	t.Run("dead portal is replaced", func(t *testing.T) {
		h := &fakePortalHost{fakeBackend: fakeBackend{sessions: sessionsNamed("tmux-fox")}}
		withPortalHost(t, h)
		fakeSSH(t, "/dev/pts/7", "/dev/pts/1")
		if err := p.Attach(target); err != nil {
			t.Fatal(err)
		}
		if len(h.killed) != 1 || len(h.created) != 1 {
			t.Errorf("killed %v created %v, want the dead portal replaced once", h.killed, h.created)
		}
	})
}

// This machine's own tmux gets a portal too, entered the same way from Rex,
// while Enter from inside that tmux keeps switching the client you're in.
func TestLocalPortal(t *testing.T) {
	p := localTmux()
	const target = "~-workspaces-foo"

	t.Run("from Rex, a missing local portal is created", func(t *testing.T) {
		h := &fakePortalHost{}
		withPortalHost(t, h)
		t.Setenv("TMUX", "")
		fakeSSH(t, "", "")
		if err := p.Attach(target); err != nil {
			t.Fatal(err)
		}
		if len(h.created) != 1 || !strings.HasPrefix(h.created[0], "tmux-local :: tmux '-u' 'attach-session' '-t' '"+target+"'") ||
			strings.Contains(h.created[0], "ssh") {
			t.Errorf("created %v, want tmux-local running a plain tmux attach", h.created)
		}
		if len(h.attached) != 1 || h.attached[0] != "tmux-local" {
			t.Errorf("hopped to %v, want tmux-local", h.attached)
		}
	})

	t.Run("from inside tmux, the client we're in is switched", func(t *testing.T) {
		h := &fakePortalHost{}
		withPortalHost(t, h)
		t.Setenv("TMUX", "/tmp/tmux-501/default,1,0")
		log := fakeSSH(t, "", "")
		if err := p.Attach(target); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(log)
		if len(h.created) != 0 || len(h.attached) != 0 || !strings.Contains(string(raw), "switch-client -t "+target) {
			t.Errorf("created %v hopped %v calls:\n%s\nwant a plain switch-client", h.created, h.attached, raw)
		}
	})
}

// A portal's own Rex session is the way into other rows, so the board leaves
// it out, except as the CURRENT context when that's where you are.
func TestRadarHidesPortalSessions(t *testing.T) {
	portal := sessionKey{"rex", "tmux-local"}
	scan := radarScanMsg{
		sessions: []mux.Session{{Surface: "rex", Name: "tmux-local"}, {Surface: "rex", Name: "notes"}},
		attached: map[sessionKey]int64{},
		portals:  map[sessionKey]bool{portal: true},
	}
	m := radarModel{prs: map[string][]rigPR{}}
	m.apply(scan)
	if len(m.sessions) != 1 || m.sessions[0].session.name != "notes" {
		t.Errorf("rows = %+v, want the portal left out", m.sessions)
	}
	m = radarModel{prs: map[string][]rigPR{}, current: portal}
	m.apply(scan)
	if m.currentRow == nil || m.currentRow.session.name != "tmux-local" {
		t.Errorf("current row = %+v, want the portal you're in", m.currentRow)
	}
}

// Inside the laptop's tmux, shown in Rex through tmux-local, a process has
// TMUX and no REX_SESSION. When its own client is the one the local portal
// stamped, it's on screen in Rex, so a remote row goes through the Rex
// portal; a tmux in a plain terminal still refuses.
func TestRemoteRowFromInsideTheLocalPortal(t *testing.T) {
	p := portalBackend{tmux.Backend{Host: "fox", Place: "fox"}}
	const target = "~/workspaces/foo"

	t.Run("through tmux-local, the remote portal is used", func(t *testing.T) {
		h := &fakePortalHost{fakeBackend: fakeBackend{sessions: sessionsNamed("tmux-local", "tmux-fox")}}
		withPortalHost(t, h)
		t.Setenv("REX_SESSION", "")
		t.Setenv("TMUX", "/tmp/tmux-501/default,1,0")
		log := fakeSSH(t, "/dev/ttys018", "/dev/ttys018")
		if err := p.Attach(target); err != nil {
			t.Fatal(err)
		}
		if got := os.Getenv("REX_SESSION"); got != "session:fake-tmux-local" {
			t.Errorf("REX_SESSION = %q, want the local portal's session adopted", got)
		}
		raw, _ := os.ReadFile(log)
		if !strings.Contains(string(raw), "'switch-client' '-c' '/dev/ttys018' '-t' '"+target+"'") || len(h.attached) != 1 || h.attached[0] != "tmux-fox" {
			t.Errorf("hopped %v, calls:\n%s\nwant the fox portal switched and hopped to", h.attached, raw)
		}
	})

	t.Run("a tmux client that isn't the portal still refuses", func(t *testing.T) {
		h := &fakePortalHost{fakeBackend: fakeBackend{sessions: sessionsNamed("tmux-local")}}
		withPortalHost(t, h)
		t.Setenv("REX_SESSION", "")
		t.Setenv("TMUX", "/tmp/tmux-501/default,1,0")
		fakeSSH(t, "/dev/ttys018", "/dev/ttys018 /dev/ttys002")
		// The fake answers display-message with the stamp, so pin the client
		// to a different tty by stamping one the display won't match.
		saved := os.Getenv("PATH")
		bin := t.TempDir()
		script := "#!/bin/sh\ncase \"$*\" in *display-message*) echo /dev/ttys002 ;; *show-options*) echo /dev/ttys018 ;; *list-clients*) printf '%s\\n' /dev/ttys018 /dev/ttys002 ;; esac\n"
		if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+":"+saved)
		if err := p.Attach(target); err == nil {
			t.Error("attach from a non-portal tmux client succeeded, want ErrNoClientSwitch")
		}
		if got := os.Getenv("REX_SESSION"); got != "" {
			t.Errorf("REX_SESSION = %q, want nothing adopted", got)
		}
	})
}
