package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The envelope is a wire contract with a vendor we don't control, so the
// test pins the exact bytes the lab captured (modulo id): one JSON object,
// fixed kv order as marshaled, content wrapped in the cross-session-message
// shape with literal newlines.
func TestBuildClaudeEnvelope(t *testing.T) {
	e := buildClaudeEnvelope("msg-1", "rig:pers-22", "pers-22", "cloud #304 merged, re-run pop")
	blob, err := marshalWire(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"msgV":1,"msg_id":"msg-1","type":"user","message":{"role":"user","content":"<cross-session-message from=\"rig:pers-22\" from-name=\"pers-22\" from-mode=\"bypass\">\ncloud #304 merged, re-run pop\n</cross-session-message>"},"priority":"next","from":"rig:pers-22"}`
	if string(blob) != want {
		t.Errorf("envelope =\n%s\nwant\n%s", blob, want)
	}
}

func TestParseProcessTableAndDescendant(t *testing.T) {
	out := `
    100      1 tmux   tmux: server
    110    100 fish   fish
    120    110 claude /home/p/.nix-profile/bin/claude --settings /r/.rig/claude-settings.json --name mir-75 --dangerously-skip-permissions
    121    120 rg     rg foo
    130    110 claude-helper /tmp/claude-helper
    140    121 claude /usr/bin/claude
    150    100 .claude-unwrapp /home/p/.nix-profile/bin/claude
`
	procs := parseProcessTable(out)
	if len(procs) != 7 {
		t.Fatalf("parsed %d procs, want 7", len(procs))
	}
	// The pane's own claude (120) is shallowest; the nested one under its
	// child (140) must not win.
	pid, ok := descendantPID(procs, 110, "claude")
	if !ok || pid != 120 {
		t.Errorf("descendantPID = %d,%v, want 120,true", pid, ok)
	}
	if _, ok := descendantPID(procs, 110, "codex"); ok {
		t.Error("found codex where none runs")
	}
	// A pane running claude directly (no shell) is its own answer.
	if pid, ok := descendantPID(procs, 100, "tmux"); !ok || pid != 100 {
		t.Errorf("root itself = %d,%v, want 100,true", pid, ok)
	}
	// The nix-wrapper case: comm is truncated to ".claude-unwrapp", and only
	// argv0's basename still says claude.
	if pid, ok := descendantPID(procs, 150, "claude"); !ok || pid != 150 {
		t.Errorf("wrapped root = %d,%v, want 150,true", pid, ok)
	}
}

func TestRigMessageLogRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Round(time.Second)
	msgs := []rigMessage{
		{V: 1, ID: "a", At: now, Dir: "in", From: "pers-22", FromAddr: "rig:pers-22", To: "mir-75", Text: "hi", Transport: "claude-socket", Delivered: true},
		{V: 1, ID: "b", At: now.Add(time.Minute), Dir: "out", From: "mir-75", To: "pers-22", Text: "yo", Transport: "claude-socket", ReplyTo: "a", Delivered: false, Error: "no live session"},
	}
	for _, m := range msgs {
		if err := appendRigMessage(dir, m); err != nil {
			t.Fatal(err)
		}
	}
	got := readRigMessages(dir)
	if len(got) != 2 {
		t.Fatalf("read %d messages, want 2", len(got))
	}
	if got[0].ID != "a" || got[1].ReplyTo != "a" || got[1].Delivered {
		t.Errorf("round trip mismatch: %+v", got)
	}

	// A torn final line (appender killed mid-write) drops quietly instead of
	// blanking the thread.
	f, err := os.OpenFile(messagesPath(dir), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"v":1,"id":"torn`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if got := readRigMessages(dir); len(got) != 2 {
		t.Errorf("after torn line: read %d, want 2", len(got))
	}
}

// Codex's queue has no cross-session envelope, so the sender attribution is
// a literal prefix. Pin it: without it a codex message loses the provenance
// the rig envelope exists to carry.
func TestCodexMessageCarriesSender(t *testing.T) {
	s := rigSender{name: "pers-22", addr: "rig:pers-22"}
	got := codexMessage(s, "cloud #304 merged, re-run pop")
	want := "[rig message from pers-22 (rig:pers-22)]\ncloud #304 merged, re-run pop"
	if got != want {
		t.Errorf("codexMessage =\n%q\nwant\n%q", got, want)
	}
}

// The thread id is resolved from the rollout's session_meta by cwd, the same
// probe resume uses — codex's auto-labels aren't unique enough to address by.
func TestCodexThreadFor(t *testing.T) {
	home := t.TempDir()
	basedir := filepath.Join(home, "workspaces", "mir-75-slug")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(home, ".codex", "sessions", "2026", "09", "30", "rollout-x.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"type":"session_meta","payload":{"cwd":` + strconv.Quote(basedir) + `,"id":"01a0f314-669d-7192-9c04-9626f73d9cc6"}}`
	if err := os.WriteFile(rollout, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := codexThreadFor(home, basedir); got != "01a0f314-669d-7192-9c04-9626f73d9cc6" {
		t.Errorf("codexThreadFor = %q, want the rollout's session id", got)
	}
	// A rig with no codex rollout resolves to empty, which the probe reports
	// as its own loud failure rather than queueing into nothing.
	if got := codexThreadFor(home, filepath.Join(home, "elsewhere")); got != "" {
		t.Errorf("codexThreadFor elsewhere = %q, want empty", got)
	}
}

// The daemon-down failure is the one this transport exists to make loud:
// `codex queue` exits 0 with the daemon stopped, so the socket is the real
// reachability signal. postCodexMessage must surface queue's own error text.
func TestPostCodexMessageSurfacesFailure(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'Error: no active session found' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := postCodexMessage("01a0f314-669d-7192-9c04-9626f73d9cc6", "hi")
	if err == nil || !strings.Contains(err.Error(), "no active session found") {
		t.Errorf("postCodexMessage err = %v, want queue's message", err)
	}
}

func TestPostCodexMessageSuccess(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'Queued message abc for thread def'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := postCodexMessage("01a0f314-669d-7192-9c04-9626f73d9cc6", "hi"); err != nil {
		t.Errorf("postCodexMessage = %v, want nil", err)
	}
}

func TestResolveRig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	mk := func(m manifest) {
		dir := filepath.Join(home, "workspaces", m.ID+"-slug")
		if err := os.MkdirAll(filepath.Join(dir, ".rig"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeManifest(dir, m); err != nil {
			t.Fatal(err)
		}
	}
	mk(manifest{ID: "mir-75", Title: "x", Tracker: "linear", TrackerID: "MIR-75", Created: time.Now()})
	mk(manifest{ID: "pers-22", Title: "y", Created: time.Now()})

	byID, err := resolveRig("mir-75")
	if err != nil || byID.ID != "mir-75" {
		t.Errorf("by id: %v %v", byID, err)
	}
	bySlug, err := resolveRig("pers-22-slug")
	if err != nil || bySlug.ID != "pers-22" {
		t.Errorf("by slug: %v %v", bySlug, err)
	}
	byTracker, err := resolveRig("MIR-75")
	if err != nil || byTracker.ID != "mir-75" {
		t.Errorf("by tracker id: %v %v", byTracker, err)
	}
	if _, err := resolveRig("nope"); err == nil || !strings.Contains(err.Error(), "no rig matches") {
		t.Errorf("unknown: %v", err)
	}
}

func TestCurrentSenderRig(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(dir, manifest{ID: "mir-75", Title: "x"}); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "repo")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	s := currentSender()
	if s.rig == nil || s.name != "mir-75" || s.addr != "rig:mir-75" {
		t.Errorf("sender = %+v, want rig mir-75", s)
	}
}

// --- pi socket transport ----------------------------------------------------

// writeRigPeerRecord drops a presence fixture into dir.
func writeRigPeerRecord(t *testing.T, dir, name, cwd string, pid int, sock, token, heartbeat string) {
	t.Helper()
	rec := rigPeerPresence{
		V: 1, InstanceID: strings.TrimSuffix(name, ".json"), PID: pid,
		SessionID: "sess-" + name, Cwd: cwd, Sock: sock, Token: token,
		StartedAt: "2026-09-30T16:00:00Z", HeartbeatAt: heartbeat, Status: "idle",
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// deadPID borrows a completed process's pid: guaranteed gone, practically
// unrecycled for the life of the test.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// liveSock stands up a throwaway unix socket so findRigPeer's stat check
// sees the real thing, and returns its path.
func liveSock(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return path
}

func TestRigPeerDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := rigPeerDir("/home/x"); got != "/run/user/1000/rig-peer" {
		t.Errorf("rigPeerDir with XDG = %q", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	want := filepath.Join("/home/x", ".pi", "agent", "rig-peer", "run")
	if got := rigPeerDir("/home/x"); got != want {
		t.Errorf("rigPeerDir without XDG = %q, want %q", got, want)
	}
}

// The probe's contract: match by workspace cwd, require a live pid and a
// real socket, and take the freshest heartbeat when two sessions share a
// root. Dead-pid records are SIGKILL litter and get reaped on the way by.
func TestFindRigPeer(t *testing.T) {
	dir := t.TempDir()
	basedir := t.TempDir()
	live := liveSock(t, dir, "live.sock")
	writeRigPeerRecord(t, dir, "live.json", basedir, os.Getpid(), live, "tok-live", "2026-09-30T16:00:00Z")
	dead := deadPID(t)
	writeRigPeerRecord(t, dir, "dead.json", basedir, dead, filepath.Join(dir, "dead.sock"), "tok-dead", "2026-09-30T15:00:00Z")
	writeRigPeerRecord(t, dir, "other.json", filepath.Join(basedir, "elsewhere"), os.Getpid(), liveSock(t, dir, "other.sock"), "tok-other", "2026-09-30T17:00:00Z")

	p, err := findRigPeer(dir, basedir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Token != "tok-live" {
		t.Errorf("findRigPeer picked token %q, want tok-live", p.Token)
	}
	if _, err := os.Stat(filepath.Join(dir, "dead.json")); !os.IsNotExist(err) {
		t.Errorf("dead record not reaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.json")); err != nil {
		t.Errorf("other-cwd record should be left alone: %v", err)
	}
}

func TestFindRigPeerFreshestWins(t *testing.T) {
	dir := t.TempDir()
	basedir := t.TempDir()
	writeRigPeerRecord(t, dir, "old.json", basedir, os.Getpid(), liveSock(t, dir, "old.sock"), "tok-old", "2026-09-30T15:00:00Z")
	writeRigPeerRecord(t, dir, "new.json", basedir, os.Getpid(), liveSock(t, dir, "new.sock"), "tok-new", "2026-09-30T17:00:00Z")
	p, err := findRigPeer(dir, basedir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Token != "tok-new" {
		t.Errorf("findRigPeer picked %q, want the fresher heartbeat", p.Token)
	}
}

func TestFindRigPeerNoMatch(t *testing.T) {
	dir := t.TempDir()
	writeRigPeerRecord(t, dir, "other.json", t.TempDir(), os.Getpid(), liveSock(t, dir, "other.sock"), "tok", "2026-09-30T17:00:00Z")
	_, err := findRigPeer(dir, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no rig-peer presence for") {
		t.Errorf("err = %v, want the no-presence failure", err)
	}
}

// rigPeerExchange captures what the Go client wrote, for the fake-server
// tests to assert on after the fact.
type rigPeerExchange struct {
	hello   map[string]any
	message map[string]any
}

// serveRigPeer plays the extension's side of one connection: read hello,
// check the token, answer ready, read message, send receipt. Bad token gets
// the extension's error frame and a dropped conn instead.
func serveRigPeer(t *testing.T, ln net.Listener, wantToken string, receipt map[string]any) chan rigPeerExchange {
	got := make(chan rigPeerExchange, 1)
	go func() {
		defer close(got)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		enc := json.NewEncoder(conn)
		var ex rigPeerExchange
		read := func(dst *map[string]any) bool {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return false
			}
			return json.Unmarshal(line, dst) == nil
		}
		if !read(&ex.hello) {
			got <- ex
			return
		}
		if ex.hello["token"] != wantToken {
			_ = enc.Encode(map[string]any{"type": "error", "reason": "auth"})
			got <- ex
			return
		}
		_ = enc.Encode(map[string]any{"type": "ready", "v": 1})
		if !read(&ex.message) {
			got <- ex
			return
		}
		if receipt != nil {
			_ = enc.Encode(receipt)
		}
		got <- ex
	}()
	return got
}

func newRigPeerServer(t *testing.T) (net.Listener, rigPeerPresence) {
	t.Helper()
	dir := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(dir, "peer.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	pres := rigPeerPresence{V: 1, InstanceID: "i1", PID: os.Getpid(), SessionID: "s1", Sock: ln.Addr().String(), Token: "tok-1"}
	return ln, pres
}

func TestPostPiMessageSubmitted(t *testing.T) {
	ln, pres := newRigPeerServer(t)
	receipt := map[string]any{"type": "receipt", "id": "m-1", "status": "submitted"}
	got := serveRigPeer(t, ln, pres.Token, receipt)
	record := rigMessage{ID: "m-1", To: "mir-75", Text: "cloud #304 merged, re-run pop", ReplyTo: "m-0"}
	sender := rigSender{name: "pers-22", addr: "rig:pers-22"}
	if err := postPiMessage(pres, record, sender); err != nil {
		t.Fatalf("postPiMessage = %v", err)
	}
	ex := <-got
	if ex.hello["v"].(float64) != 1 || ex.hello["type"] != "hello" {
		t.Errorf("hello = %v", ex.hello)
	}
	m := ex.message
	if m["type"] != "message" || m["id"] != "m-1" || m["to"] != "mir-75" ||
		m["from"] != "pers-22" || m["fromAddr"] != "rig:pers-22" ||
		m["text"] != record.Text || m["replyTo"] != "m-0" {
		t.Errorf("message frame = %v", m)
	}
}

func TestPostPiMessageBusyIsTheAnswer(t *testing.T) {
	ln, pres := newRigPeerServer(t)
	receipt := map[string]any{"type": "receipt", "id": "m-2", "status": "busy", "reason": "agent mid-run; nothing was queued"}
	serveRigPeer(t, ln, pres.Token, receipt)
	err := postPiMessage(pres, rigMessage{ID: "m-2", To: "mir-75", Text: "hi"}, rigSender{name: "pers-22", addr: "rig:pers-22"})
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("err = %v, want the busy refusal", err)
	}
}

func TestPostPiMessageBadToken(t *testing.T) {
	ln, pres := newRigPeerServer(t)
	serveRigPeer(t, ln, "the-right-token", nil)
	err := postPiMessage(pres, rigMessage{ID: "m-3", To: "mir-75", Text: "hi"}, rigSender{name: "pers-22", addr: "rig:pers-22"})
	if err == nil || !strings.Contains(err.Error(), "refused the handshake") {
		t.Errorf("err = %v, want handshake refusal", err)
	}
}
