package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/phinze/rig/internal/mux"
	"github.com/phinze/rig/internal/mux/tmux"
)

// fakeBackend is a second multiplexer for routing tests: it answers for the
// sessions and panes it was given and records what it was asked to kill.
type fakeBackend struct {
	tmux.Backend // unimplemented methods fall through to tmux; the tests never reach them
	name         string
	surface      string // "" means the local instance, spelled as the kind
	sessions     []mux.Session
	panes        []mux.Pane
	killed       []string
}

func (f *fakeBackend) Name() string { return f.name }
func (f *fakeBackend) Surface() string {
	if f.surface != "" {
		return f.surface
	}
	return f.name
}
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
		sessions: []mux.Session{{Surface: "fake", Name: "~-workspaces-on-fake", LastAttached: 7}},
		panes: []mux.Pane{{
			Surface: "fake", Session: "~-workspaces-on-fake", WindowIdx: "0", PaneIdx: "0",
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
	for _, s := range allSessions(knownBackends()) {
		if s.Surface == "fake" && s.Name == "~-workspaces-on-fake" {
			tagged++
		}
	}
	if tagged != 1 {
		t.Errorf("fake session appeared %d times in the union, want once", tagged)
	}
	kids := liveAgentChildren(knownBackends())[sessionKey{"fake", "~-workspaces-on-fake"}]
	if len(kids) != 1 || kids[0].Surface != "fake" || kids[0].Target != "block:1" {
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

// Two hosts slug ~/workspaces/foo identically, so a remote surface can list a
// session with exactly a local rig's name. The board must keep them apart: the
// rig keeps its row and its own agents, and the remote session is a plain row
// of its own that routes back to the surface it came from.
func TestRadarKeepsCollidingSurfacesApart(t *testing.T) {
	remote := &fakeBackend{name: "tmux", surface: "tmux@elsewhere"}
	registerFakeBackend(t, remote)

	path := "/work/foo"
	name := rigSessionName(path)
	m := radarModel{prs: map[string][]rigPR{}}
	m.apply(radarScanMsg{
		statuses: []rigStatus{{Slug: "foo", ID: "MIR-1", Title: "local foo", Path: path}},
		sessions: []mux.Session{
			{Surface: "tmux", Name: name, Path: path},
			{Surface: "tmux@elsewhere", Name: name, Path: path},
		},
		agents: map[sessionKey][]agentChild{
			tk(name):                 {{Surface: "tmux", Target: name + ":0", Context: "local agent"}},
			{"tmux@elsewhere", name}: {{Surface: "tmux@elsewhere", Target: name + ":1", Context: "remote agent"}},
		},
		attached: map[sessionKey]int64{},
	})

	if len(m.inflight) != 1 || len(m.inflight[0].agents) != 1 || m.inflight[0].agents[0].Context != "local agent" {
		t.Fatalf("rig row = %+v, want the local rig with only its own agent", m.inflight)
	}
	if len(m.sessions) != 1 {
		t.Fatalf("bare rows = %+v, want the remote session alone", m.sessions)
	}
	bare := m.sessions[0]
	if bare.Title != "elsewhere:/work/foo" {
		t.Errorf("remote row title = %q, want its place in front", bare.Title)
	}
	if bare.session.b.Surface() != "tmux@elsewhere" {
		t.Errorf("remote row routes to %s, want tmux@elsewhere", bare.session.b.Surface())
	}
	if len(bare.agents) != 1 || bare.agents[0].Context != "remote agent" {
		t.Errorf("remote row agents = %+v, want the remote agent", bare.agents)
	}
	if got := surfaceNamed("tmux@gone").Surface(); got != "tmux" {
		t.Errorf("unknown remote surface fell back to %s, want the local tmux", got)
	}
}

// Remote rows arrive on their own message, after the board has already drawn
// from local state, and they stay folded into every local rescan until the
// next remote pass replaces them.
func TestRadarFoldsRemoteRowsIntoLocalScans(t *testing.T) {
	remote := &fakeBackend{name: "rex", surface: "rex@devbox"}
	registerFakeBackend(t, remote)

	local := radarScanMsg{
		sessions: []mux.Session{{Surface: "tmux", Name: "here", Path: "/home/me/here"}},
		attached: map[sessionKey]int64{tk("here"): 100},
	}
	m := radarModel{home: "/home/me", prs: map[string][]rigPR{}, remoteBusy: true}
	m.apply(local)
	if len(m.sessions) != 1 {
		t.Fatalf("local rows = %+v, want one", m.sessions)
	}

	far := sessionKey{"rex@devbox", "~-workspaces-foo"}
	next, _ := m.Update(radarRemoteMsg{
		sessions: []mux.Session{{Surface: far.surface, Name: far.name, Path: "/home/me/workspaces/foo", LastAttached: 200}},
		agents:   map[sessionKey][]agentChild{far: {{Surface: far.surface, Target: "block:1", Context: "remote work"}}},
	})
	m = next.(radarModel)
	if m.remoteBusy {
		t.Error("remote pass still marked in flight after it answered")
	}
	titles := func() []string {
		var out []string
		for _, s := range m.sessions {
			out = append(out, s.Title)
		}
		return out
	}
	if got := titles(); len(got) != 2 || !slices.Contains(got, "devbox:~/workspaces/foo") {
		t.Fatalf("rows after the remote pass = %v, want the devbox row beside the local one", got)
	}

	m.apply(local)
	if got := titles(); len(got) != 2 {
		t.Fatalf("rows after a local rescan = %v, want the remote row kept", got)
	}
	for _, s := range m.sessions {
		if s.session.b.Surface() == "rex@devbox" && (len(s.agents) != 1 || s.agents[0].Context != "remote work") {
			t.Errorf("remote row agents = %+v, want its own", s.agents)
		}
	}
}

// A surface that couldn't be reached says so above the board and counts as
// prompt furniture, so clicks still land on the rows they're drawn over.
func TestRadarNamesUnreachableSurfaces(t *testing.T) {
	m := radarModel{home: "/home/me", prs: map[string][]rigPR{}}
	m.apply(radarScanMsg{attached: map[sessionKey]int64{}})
	before, _ := m.viewportChrome()
	next, _ := m.Update(radarRemoteMsg{down: []string{"devbox"}})
	m = next.(radarModel)
	if line := m.surfaceLine(); !strings.Contains(line, "unreachable: devbox") {
		t.Errorf("surface line = %q, want devbox named", line)
	}
	if after, _ := m.viewportChrome(); after != before+2 {
		t.Errorf("prompt rows %d -> %d, want the banner and its blank counted", before, after)
	}
	next, _ = m.Update(radarRemoteMsg{})
	if line := next.(radarModel).surfaceLine(); line != "" {
		t.Errorf("surface line after recovery = %q, want none", line)
	}
}

func sessionsNamed(names ...string) []mux.Session {
	out := make([]mux.Session, 0, len(names))
	for _, n := range names {
		out = append(out, mux.Session{Name: n})
	}
	return out
}
