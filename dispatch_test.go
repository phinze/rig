package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDispatchRigUsesTrackerIdentityAndRejectsProjects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspaces := filepath.Join(home, "workspaces")
	if err := os.MkdirAll(workspaces, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(dir string, m manifest) {
		t.Helper()
		path := filepath.Join(workspaces, dir)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeManifest(path, m); err != nil {
			t.Fatal(err)
		}
	}
	write("mir-1697-command", manifest{ID: "mir-1697", Tracker: "linear", TrackerID: "MIR-1697"})
	write("project-byoi", manifest{ID: "project-byoi", Kind: "project", Tracker: "linear", TrackerID: "project-uuid"})

	rig, err := resolveDispatchRig("MIR-1697")
	if err != nil || rig.ID != "mir-1697" {
		t.Fatalf("issue dispatch resolution = %+v, %v", rig, err)
	}
	if _, err := resolveDispatchRig("project-uuid"); err == nil {
		t.Fatal("expected project dispatch to be rejected")
	}
}
