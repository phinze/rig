package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeProjectDirName(t *testing.T) {
	got := claudeProjectDirName("/home/u/workspaces/fake-1-do.the_thing")
	want := "-home-u-workspaces-fake-1-do-the-thing"
	if got != want {
		t.Errorf("claudeProjectDirName = %q, want %q", got, want)
	}
}

func TestClaudeSessionActivity(t *testing.T) {
	home := t.TempDir()
	basedir := filepath.Join(home, "workspaces", "fake-1")
	root := filepath.Join(home, ".claude", "projects")

	old := time.Now().Add(-48 * time.Hour)
	fresh := time.Now().Add(-1 * time.Hour)
	write := func(dir, name string, mtime time.Time) {
		t.Helper()
		d := filepath.Join(root, dir)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(d, name)
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	mangled := claudeProjectDirName(basedir)
	write(mangled, "a.jsonl", old)               // session in the basedir itself
	write(mangled+"-fakerepo", "b.jsonl", fresh) // session in a repo workspace
	// Another rig whose slug shares a prefix must not bleed in: fake-12 vs
	// fake-1 — the mangled+"-" guard distinguishes them.
	write(claudeProjectDirName(basedir+"2"), "c.jsonl", time.Now())
	write(mangled+"-fakerepo", "notes.txt", time.Now()) // non-jsonl ignored

	got := claudeSessionActivity(home, basedir)
	if got != fresh.Unix() {
		t.Errorf("claudeSessionActivity = %d, want %d (fresh jsonl in repo workspace)", got, fresh.Unix())
	}

	if got := claudeSessionActivity(home, filepath.Join(home, "workspaces", "nope")); got != 0 {
		t.Errorf("claudeSessionActivity for unknown rig = %d, want 0", got)
	}
}
