package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTombstoneRoundTrip covers the store's contract: a tombstone survives a
// write/read cycle intact, ids that repeat don't collide, and anything past the
// regret window is pruned by the act of listing.
func TestTombstoneRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now()

	fresh := &tombstone{
		Version:    tombstoneVersion,
		ID:         "mir-1",
		Title:      "do the thing",
		Basedir:    "/home/x/workspaces/mir-1",
		Agent:      string(agentCodex),
		Tracker:    "linear",
		TrackerID:  "MIR-1",
		TrackerURL: "https://linear.app/example/issue/MIR-1",
		Died:       now.Add(-2 * time.Hour),
		Repos:      map[string]string{"runtime": "mirendev/runtime"},
		Sources:    map[string]string{"runtime": "/home/x/src/runtime"},
		Session:    &sessionRef{Agent: "codex", ID: "019fc0a3"},
	}
	if err := writeTombstone(fresh); err != nil {
		t.Fatal(err)
	}

	// Same id, different basedir: a re-upped ticket must not erase the record of
	// the rig it replaced while that one is still recoverable.
	reused := *fresh
	reused.Basedir = "/home/x/workspaces/mir-1-second"
	reused.Died = now.Add(-1 * time.Hour)
	if err := writeTombstone(&reused); err != nil {
		t.Fatal(err)
	}

	stale := *fresh
	stale.ID = "mir-old"
	stale.Basedir = "/home/x/workspaces/mir-old"
	stale.Died = now.Add(-tombstoneRetention - time.Hour)
	if err := writeTombstone(&stale); err != nil {
		t.Fatal(err)
	}

	got, err := listTombstones(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 live tombstones, got %d", len(got))
	}
	// Newest death first.
	if got[0].Basedir != "/home/x/workspaces/mir-1-second" {
		t.Errorf("wrong order, got %s first", got[0].Basedir)
	}
	if got[0].Session == nil || got[0].Session.ID != "019fc0a3" {
		t.Errorf("session ref lost in round trip: %+v", got[0].Session)
	}
	if got[0].Tracker != fresh.Tracker || got[0].TrackerID != fresh.TrackerID || got[0].TrackerURL != fresh.TrackerURL {
		t.Errorf("tracker identity lost in round trip: %+v", got[0])
	}
	if !got[0].resurrectable() {
		t.Error("tombstone with a session id should be resurrectable")
	}

	// The expired one is gone from disk, not merely filtered out.
	if _, err := findTombstone("mir-old", now); err != nil {
		t.Fatal(err)
	} else if f, _ := findTombstone("mir-old", now); f != nil {
		t.Error("expired tombstone still findable")
	}
}

// TestTombstoneWithoutSessionIsStillKept pins that a rig with no recoverable
// conversation still leaves a record. It's the difference between "this rig is
// gone and here's what it was" and no evidence it ever existed.
func TestTombstoneWithoutSessionIsStillKept(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now()

	if err := writeTombstone(&tombstone{
		Version: tombstoneVersion,
		ID:      "no-session",
		Basedir: "/home/x/workspaces/no-session",
		Died:    now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := listTombstones(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want the sessionless tombstone kept, got %d", len(got))
	}
	if got[0].resurrectable() {
		t.Error("a tombstone with no session must not claim to be resurrectable")
	}
	if got[0].subject() != "no-session" {
		t.Errorf("subject should fall back to the id, got %q", got[0].subject())
	}
}

// TestCodexNewestSessionMatchesRigCwd covers the resolver that mattered most in
// practice: codex files rollouts by date, not by cwd, so finding a rig's
// session means walking and reading session_meta. Getting this wrong is silent
// — you'd record no session and only find out when you tried to come back.
func TestCodexNewestSessionMatchesRigCwd(t *testing.T) {
	home := t.TempDir()
	basedir := filepath.Join(home, "workspaces", "my-rig")
	dir := filepath.Join(home, ".codex", "sessions", "2026", "08", "01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(name, cwd, id string, mod time.Time) {
		path := filepath.Join(dir, name)
		line := `{"type":"session_meta","payload":{"cwd":"` + cwd + `","id":"` + id + `"}}` + "\n"
		if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	base := time.Now().Add(-24 * time.Hour)
	write("rollout-other.jsonl", filepath.Join(home, "workspaces", "different-rig"), "wrong-rig", base.Add(3*time.Hour))
	write("rollout-old.jsonl", filepath.Join(basedir, "runtime"), "older", base)
	write("rollout-new.jsonl", filepath.Join(basedir, "runtime"), "newest", base.Add(time.Hour))

	path, id := codexNewestSession(home, basedir)
	if id != "newest" {
		t.Errorf("want the newest in-rig session, got id %q (path %s)", id, path)
	}

	// And the generic entry point should agree, tagged with the right agent.
	ref := agentSessionRef(home, basedir, agentCodex)
	if ref == nil || ref.ID != "newest" || ref.Agent != string(agentCodex) {
		t.Errorf("agentSessionRef disagreed: %+v", ref)
	}
	if agentSessionRef(home, filepath.Join(home, "workspaces", "nobody"), agentCodex) != nil {
		t.Error("a rig with no session should resolve to nil, not an empty ref")
	}
}

// TestResumeCommands pins the invocations against what the installed CLIs
// actually accept. These were verified by hand once; the test is here so a
// future edit can't quietly turn a resume into a fresh session, which would
// look like success and lose the context anyway. Ids arrive shell-quoted
// because they travel to the pane by send-keys, same as every other launch.
func TestResumeCommands(t *testing.T) {
	for _, tc := range []struct {
		agent agentKind
		want  string
	}{
		{agentClaude, "claude --dangerously-skip-permissions --resume 'abc123'"},
		{agentCodex, "codex --dangerously-bypass-approvals-and-sandbox resume 'abc123'"},
		{agentAntigravity, "agy --dangerously-skip-permissions --conversation 'abc123'"},
	} {
		if got := tc.agent.resumeCommand("abc123"); got != tc.want {
			t.Errorf("%s resume: got %q want %q", tc.agent, got, tc.want)
		}
	}
}

func TestHumanAge(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
	} {
		if got := humanAge(tc.d); got != tc.want {
			t.Errorf("humanAge(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
