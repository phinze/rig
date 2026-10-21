package main

import (
	"testing"
	"time"

	"github.com/phinze/rig/internal/mux"
)

// killableBackend is a fakeBackend that records local session kills too.
type killableBackend struct {
	*fakeBackend
	order *[]string
}

func (k killableBackend) KillSession(name string) error {
	*k.order = append(*k.order, "kill "+k.name+" "+name)
	return nil
}

// A live rig moves by recording its conversation, killing the old session,
// and building the new one on the target, in that order, so no two agents
// share a conversation. A parked rig only has its manifest changed.
func TestMoveRigBackend(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	basedir := t.TempDir()
	name := rigSessionName(basedir)

	var order []string
	from := killableBackend{&fakeBackend{name: "fakefrom", sessions: []mux.Session{{Surface: "fakefrom", Name: name}}}, &order}
	to := killableBackend{&fakeBackend{name: "faketo"}, &order}
	saved := extraBackends
	extraBackends = append(append([]mux.Backend{}, saved...), from, to)
	t.Cleanup(func() { extraBackends = saved })

	origBuild := buildMovedRuntime
	buildMovedRuntime = func(basedir string, m manifest) (rigSession, error) {
		order = append(order, "build "+m.Backend)
		return sessionFor(basedir, m), nil
	}
	t.Cleanup(func() { buildMovedRuntime = origBuild })

	if err := writeManifest(basedir, manifest{ID: "a", Backend: "fakefrom", Created: time.Unix(100, 0)}); err != nil {
		t.Fatal(err)
	}
	rs, live, err := moveRigBackend(basedir, "faketo")
	if err != nil {
		t.Fatal(err)
	}
	if !live || rs.b.Name() != "faketo" {
		t.Errorf("moved to %s (live %v), want a live faketo session", rs.b.Name(), live)
	}
	if want := []string{"kill fakefrom " + name, "build faketo"}; len(order) != 2 || order[0] != want[0] || order[1] != want[1] {
		t.Errorf("order = %v, want %v", order, want)
	}
	m, _ := readManifest(basedir)
	if m.Backend != "faketo" {
		t.Errorf("manifest backend = %q, want faketo", m.Backend)
	}

	// Parked: the manifest changes, nothing else does.
	order = nil
	m.Parked = time.Now()
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}
	if _, live, err := moveRigBackend(basedir, "fakefrom"); err != nil || live {
		t.Fatalf("parked move = live %v, %v; want only the manifest", live, err)
	}
	if len(order) != 0 {
		t.Errorf("a parked move touched sessions: %v", order)
	}
	if m, _ := readManifest(basedir); m.Backend != "fakefrom" {
		t.Errorf("parked manifest backend = %q, want fakefrom", m.Backend)
	}
}
