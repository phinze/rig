package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
func (f *fakePortalHost) KillSession(name string) error {
	f.killed = append(f.killed, name)
	return nil
}
func (f *fakePortalHost) Attach(target string) error {
	f.attached = append(f.attached, target)
	return nil
}

// fakeSSH answers the remote tmux calls a portal makes: the stamped tty, the
// attached clients, and switch-client, logging each command line.
func fakeSSH(t *testing.T, stamped, clients string) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "ssh.log")
	script := `#!/bin/sh
for last in "$@"; do :; done
printf '%s\n' "$last" >> "` + log + `"
case "$last" in
*show-options*) echo '` + stamped + `' ;;
*list-clients*) printf '%s\n' ` + clients + ` ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
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
