package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestCodexSessionActivity(t *testing.T) {
	home := t.TempDir()
	basedir := filepath.Join(home, "workspaces", "fake-1")
	root := filepath.Join(home, ".codex", "sessions", "2026", "07", "10")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, cwd string, mtime time.Time) {
		t.Helper()
		body := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"cwd\":%q}}\n{}\n", cwd)
		path := filepath.Join(root, name+".jsonl")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	fresh := time.Now().Add(-time.Hour).Truncate(time.Second)
	write("root", basedir, old)
	write("repo", filepath.Join(basedir, "repo"), fresh)
	write("prefix-collision", basedir+"2", time.Now())

	if got := codexSessionActivity(home, basedir); got != fresh.Unix() {
		t.Errorf("codexSessionActivity = %d, want %d", got, fresh.Unix())
	}
}

func TestAntigravitySessionActivity(t *testing.T) {
	home := t.TempDir()
	basedir := filepath.Join(home, "workspaces", "fake-1")
	root := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(
		"{\"timestamp\":100000,\"workspace\":%q}\n{\"timestamp\":200000,\"workspace\":%q,\"conversationId\":\"active\"}\n{\"timestamp\":900000,\"workspace\":%q}\n",
		basedir, filepath.Join(basedir, "repo"), basedir+"2",
	)
	if err := os.WriteFile(filepath.Join(root, "history.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	conversations := filepath.Join(root, "conversations")
	if err := os.MkdirAll(conversations, 0o755); err != nil {
		t.Fatal(err)
	}
	conversation := filepath.Join(conversations, "active.db")
	if err := os.WriteFile(conversation, []byte("opaque"), 0o644); err != nil {
		t.Fatal(err)
	}
	active := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(conversation, active, active); err != nil {
		t.Fatal(err)
	}
	if got := antigravitySessionActivity(home, basedir); got != active.Unix() {
		t.Errorf("antigravitySessionActivity = %d, want %d", got, active.Unix())
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

func TestClaudeSessionTitle(t *testing.T) {
	home := t.TempDir()
	basedir := filepath.Join(home, "workspaces", "fake-1")
	dir := filepath.Join(home, ".claude", "projects", claudeProjectDirName(basedir)+"-fakerepo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Claude Code re-emits the record every time it refines the title, so the
	// last one wins. Padding pushes the early ones out of the tail window,
	// which is exactly what a long real session does.
	var b strings.Builder
	b.WriteString(`{"type":"ai-title","aiTitle":"First guess","sessionId":"s"}` + "\n")
	for b.Len() < claudeTitleTail*2 {
		b.WriteString(`{"type":"user","message":{"content":"` + strings.Repeat("x", 4000) + `"}}` + "\n")
	}
	b.WriteString(`{"type":"assistant","message":{"content":"no title here"}}` + "\n")
	b.WriteString(`{"type":"ai-title","aiTitle":"Add authentication to the registry","sessionId":"s"}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, want := claudeSessionTitle(home, basedir), "Add authentication to the registry"; got != want {
		t.Errorf("claudeSessionTitle = %q, want %q", got, want)
	}

	// A session that never got named, and a rig with no session at all, both
	// have to come back empty so the subject column falls through cleanly
	// rather than rendering a stray brace.
	untitled := filepath.Join(home, "workspaces", "fake-2")
	d2 := filepath.Join(home, ".claude", "projects", claudeProjectDirName(untitled))
	if err := os.MkdirAll(d2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d2, "a.jsonl"), []byte("{\"type\":\"user\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := claudeSessionTitle(home, untitled); got != "" {
		t.Errorf("claudeSessionTitle for an unnamed session = %q, want empty", got)
	}
	if got := claudeSessionTitle(home, filepath.Join(home, "workspaces", "nope")); got != "" {
		t.Errorf("claudeSessionTitle for unknown rig = %q, want empty", got)
	}
}
