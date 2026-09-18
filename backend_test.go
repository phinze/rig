package main

import (
	"testing"

	"github.com/phinze/rig/internal/mux"
	"github.com/phinze/rig/internal/mux/tmux"
)

// fakeBackend is a second multiplexer for routing tests: it answers for the
// sessions and panes it was given and records what it was asked to kill.
type fakeBackend struct {
	tmux.Backend // unimplemented methods fall through to tmux; the tests never reach them
	name         string
	sessions     []mux.Session
	panes        []mux.Pane
	killed       []string
}

func (f *fakeBackend) Name() string { return f.name }
func (f *fakeBackend) HasSession(name string) bool {
	for _, s := range f.sessions {
		if s.Name == name {
			return true
		}
	}
	return false
}
func (f *fakeBackend) Sessions() []mux.Session       { return f.sessions }
func (f *fakeBackend) AllPanes() ([]mux.Pane, error) { return f.panes, nil }
func (f *fakeBackend) KillSessionAt(name, endpoint string) error {
	f.killed = append(f.killed, name+"@"+endpoint)
	return nil
}

func registerFakeBackend(t *testing.T, f *fakeBackend) {
	t.Helper()
	saved := extraBackends
	extraBackends = append(append([]mux.Backend{}, saved...), f)
	t.Cleanup(func() { extraBackends = saved })
}

// A rig's session lives on the backend its manifest names, not on whichever
// one the preference ladder would pick for a new rig; a name the binary doesn't
// know falls back to tmux; and the cross-machine listings union every backend
// with each row tagged by where it came from.
func TestBackendRoutesPerRig(t *testing.T) {
	fake := &fakeBackend{
		name:     "fake",
		sessions: []mux.Session{{Backend: "fake", Name: "~-workspaces-on-fake", LastAttached: 7}},
		panes: []mux.Pane{{
			Backend: "fake", Session: "~-workspaces-on-fake", WindowIdx: "0", PaneIdx: "0",
			WindowName: "main/repo", Target: "block:1", Command: "claude", Title: "✳ Task",
		}},
	}
	registerFakeBackend(t, fake)

	if _, err := backendByName("fake"); err != nil {
		t.Fatalf("registered backend not resolvable: %v", err)
	}
	if _, err := backendByName("screen"); err == nil {
		t.Error("unknown backend should not resolve")
	}

	onFake := sessionFor("/w/on-fake", manifest{Backend: "fake"})
	if onFake.b.Name() != "fake" {
		t.Errorf("session backend = %s, want fake", onFake.b.Name())
	}
	if legacy := sessionFor("/w/legacy", manifest{}); legacy.b.Name() != "tmux" {
		t.Errorf("empty backend resolved to %s, want tmux", legacy.b.Name())
	}
	if gone := sessionFor("/w/gone", manifest{Backend: "screen"}); gone.b.Name() != "tmux" {
		t.Errorf("unknown recorded backend resolved to %s, want tmux", gone.b.Name())
	}

	var tagged int
	for _, s := range allSessions() {
		if s.Backend == "fake" && s.Name == "~-workspaces-on-fake" {
			tagged++
		}
	}
	if tagged != 1 {
		t.Errorf("fake session appeared %d times in the union, want once", tagged)
	}
	kids := liveAgentChildren()["~-workspaces-on-fake"]
	if len(kids) != 1 || kids[0].Backend != "fake" || kids[0].Target != "block:1" {
		t.Errorf("agent children from the fake = %+v, want one tagged fake at block:1", kids)
	}

	// The teardown job kills on the backend it recorded, which is the case the
	// Linux systemd worker hits with none of the invoking shell's preferences.
	job := &teardownJob{Session: "~-workspaces-on-fake", Backend: "fake", TmuxSocket: "ep"}
	if err := job.backend().KillSessionAt(job.Session, job.TmuxSocket); err != nil {
		t.Fatal(err)
	}
	if len(fake.killed) != 1 || fake.killed[0] != "~-workspaces-on-fake@ep" {
		t.Errorf("kill routed to %v, want the fake", fake.killed)
	}
}
