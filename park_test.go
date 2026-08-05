package main

import (
	"testing"
	"time"
)

func TestSetRigParkedAdvancesLastTouch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	basedir := t.TempDir()
	old := time.Unix(100, 0)
	if err := writeManifest(basedir, manifest{ID: "a", Created: old, Touched: old}); err != nil {
		t.Fatal(err)
	}

	if err := setRigParked(basedir, true, false, nil); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(basedir)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Touched.After(old) {
		t.Fatalf("touched = %v, want newer than %v", m.Touched, old)
	}
	if !m.Touched.Equal(m.Parked) {
		t.Fatalf("touch and park stamps differ: touched=%v parked=%v", m.Touched, m.Parked)
	}
}
