package main

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// standInRig points serve's exec at a script that reports what it was given,
// standing in for a rig binary on the far host.
func standInRig(t *testing.T, body string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "rig")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := rigExecutable
	rigExecutable = func() (string, error) { return script, nil }
	t.Cleanup(func() { rigExecutable = prev })
}

func loopbackHost(t *testing.T) servedBackend {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(serveHandler(newServeGate([]string{"nobody"}, nil, true), func() serveDoc { return serveDoc{} }))
	t.Cleanup(srv.Close)
	b, err := newServedBackend("farhost", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A forwarded command's streams and exit code come back as if it ran here,
// and it learns it was forwarded, on whose behalf, and with which message id
// from its environment.
func TestExecForwardsStreamsAndExit(t *testing.T) {
	standInRig(t, `echo "args: $*"
echo "local=$RIG_LOCAL_ONLY sender=$RIG_FORWARDED_SENDER name=$RIG_FORWARDED_SENDER_NAME id=$RIG_FORWARDED_MESSAGE_ID reply=$RIG_FORWARDED_REPLY_TO"
echo "stdin: $(cat)"
echo "rig: something went wrong" >&2
exit 3
`)
	b := loopbackHost(t)
	var out, errOut bytes.Buffer
	err := b.exec(execRequest{
		Argv: []string{"send", "mir-1", "hello there"}, Stdin: []byte("piped"),
		From: "rig:cos-2026-10-10@spoofed", FromName: "cos-2026-10-10",
		MessageID: "m-1", ReplyTo: "m-0",
	}, &out, &errOut)

	exit, ok := errors.AsType[remoteExit](err)
	if !ok || exit.code != 3 || exit.place != "farhost" {
		t.Fatalf("exec err = %v, want remoteExit code 3 on farhost", err)
	}
	for _, want := range []string{
		"args: send mir-1 hello there",
		"local=1 sender=rig:cos-2026-10-10@loopback name=cos-2026-10-10@loopback id=m-1 reply=m-0",
		"stdin: piped",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(errOut.String(), "something went wrong") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// Only the verbs that act on this host's rigs can be asked for from another;
// the ones that configure or draw on this machine are refused before anything
// runs.
func TestExecRefusesUnlistedVerbs(t *testing.T) {
	standInRig(t, "echo ran\n")
	b := loopbackHost(t)
	for _, argv := range [][]string{{"config", "agent", "codex"}, {"serve"}, {"__teardown", "x"}, {"notify", "post"}, {}} {
		var out bytes.Buffer
		err := b.exec(execRequest{Argv: argv}, &out, &out)
		if err == nil || !strings.Contains(err.Error(), "refused") || strings.Contains(out.String(), "ran") {
			t.Errorf("%v: err = %v, out = %q; want refused without running", argv, err, out.String())
		}
	}
	var out bytes.Buffer
	if err := b.exec(execRequest{Argv: []string{"notify", "list"}}, &out, &out); err != nil || !strings.Contains(out.String(), "ran") {
		t.Errorf("notify list: %v %q", err, out.String())
	}
}

// The place on a sender's address is always the tailnet's, whatever the
// caller claimed, and a caller that claimed nothing is a person at a shell.
func TestStampSender(t *testing.T) {
	for _, c := range []struct{ addr, name, wantAddr, wantName string }{
		{"rig:mir-1", "mir-1", "rig:mir-1@fox", "mir-1@fox"},
		{"rig:mir-1@elsewhere", "mir-1@elsewhere", "rig:mir-1@fox", "mir-1@fox"},
		{"cli:phinze", "", "cli:phinze@fox", "phinze@fox"},
		{"", "", "cli:unknown@fox", "unknown@fox"},
	} {
		got := stampSender(c.addr, c.name, "fox")
		if got.addr != c.wantAddr || got.name != c.wantName {
			t.Errorf("stampSender(%q, %q) = %q %q, want %q %q", c.addr, c.name, got.addr, got.name, c.wantAddr, c.wantName)
		}
	}
}

// The forwarding markers are read once and removed, so nothing this process
// starts (a tmux server above all) inherits them.
func TestCaptureRemoteEnvUnsets(t *testing.T) {
	t.Setenv(envLocalOnly, "1")
	t.Setenv(envSenderAddr, "rig:x@fox")
	t.Setenv(envSenderName, "x@fox")
	t.Setenv(envMessageID, "m-9")
	t.Cleanup(func() { localOnly, forwardedSender, forwardedMessage.id, forwardedMessage.replyTo = false, nil, "", "" })
	captureRemoteEnv()
	if !localOnly || forwardedSender == nil || forwardedSender.addr != "rig:x@fox" || forwardedMessage.id != "m-9" {
		t.Fatalf("captured localOnly=%v sender=%v id=%q", localOnly, forwardedSender, forwardedMessage.id)
	}
	for _, k := range []string{envLocalOnly, envSenderAddr, envSenderName, envMessageID} {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s still set", k)
		}
	}
	if s := currentSender(); s.addr != "rig:x@fox" {
		t.Errorf("currentSender = %v, want the forwarded sender", s)
	}
}

func TestSplitPlace(t *testing.T) {
	for in, want := range map[string][2]string{
		"mir-1@foxtrotbase": {"mir-1", "foxtrotbase"},
		"mir-1":             {"mir-1", ""},
		"@fox":              {"@fox", ""},
		"a@B C":             {"a@B C", ""},
	} {
		id, place := splitPlace(in)
		if id != want[0] || place != want[1] {
			t.Errorf("splitPlace(%q) = %q %q, want %q %q", in, id, place, want[0], want[1])
		}
	}
}

// farHost stands up a served host on loopback whose board lists rigs, wires it
// into this process's [surfaces] as place "fx", and isolates HOME so the
// remote cache and local rigs are the test's own. It returns the local
// workspaces dir.
func farHost(t *testing.T, rigs ...rigStatus) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	doc := serveDoc{Version: serveVersion}
	for _, r := range rigs {
		doc.Rigs = append(doc.Rigs, serveRig{rigRowJSON: rigsForJSON([]rigStatus{r}, false)[0], Session: "~/workspaces/" + r.Slug})
	}
	srv := httptest.NewServer(serveHandler(newServeGate([]string{"nobody"}, nil, true), func() serveDoc { return doc }))
	t.Cleanup(srv.Close)
	if err := os.MkdirAll(filepath.Join(cfg, "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "rig", "surfaces.toml"), []byte("[surfaces]\nfx = \"rig+"+srv.URL+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(home, "workspaces")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

func mkLocalRig(t *testing.T, ws string, m manifest) string {
	t.Helper()
	dir := filepath.Join(ws, m.ID)
	if err := os.MkdirAll(filepath.Join(dir, ".rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if m.Created.IsZero() {
		m.Created = time.Now()
	}
	if err := writeManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Local wins a bare id, a place reaches past it, a miss here finds the rig
// elsewhere, and cos means the newest workday on any host, refusing a tie.
func TestResolveTargetAcrossHosts(t *testing.T) {
	ws := farHost(t,
		rigStatus{ID: "mir-9", Slug: "mir-9", Tracker: "linear", TrackerID: "MIR-9"},
		rigStatus{ID: "pers-22", Slug: "pers-22"},
		rigStatus{ID: "cos-2026-10-09", Slug: "cos-2026-10-09", Kind: "cos"},
	)
	mkLocalRig(t, ws, manifest{ID: "pers-22", Title: "here"})
	cosDir := mkLocalRig(t, ws, manifest{ID: "cos-2026-10-10", Kind: "cos"})

	check := func(q, wantLabel string, wantRemote bool) {
		t.Helper()
		got, err := resolveTarget(q)
		if err != nil {
			t.Fatalf("resolveTarget(%q): %v", q, err)
		}
		if got.label() != wantLabel || (got.remote != nil) != wantRemote {
			t.Errorf("resolveTarget(%q) = %s (remote %v), want %s (remote %v)", q, got.label(), got.remote != nil, wantLabel, wantRemote)
		}
	}
	check("mir-9", "mir-9@fx", true)
	check("MIR-9", "mir-9@fx", true)
	check("pers-22", "pers-22", false)
	check("pers-22@fx", "pers-22@fx", true)
	check("cos", "cos-2026-10-10", false)
	if _, err := resolveTarget("nope"); err == nil {
		t.Error("a query nothing matches resolved")
	}

	if err := os.RemoveAll(cosDir); err != nil {
		t.Fatal(err)
	}
	check("cos", "cos-2026-10-09@fx", true)

	mkLocalRig(t, ws, manifest{ID: "cos-2026-10-09", Kind: "cos"})
	if _, err := resolveTarget("cos"); err == nil || !strings.Contains(err.Error(), "same day") {
		t.Errorf("same-day cos on two hosts = %v, want a refusal naming both", err)
	}

	localOnly = true
	t.Cleanup(func() { localOnly = false })
	if _, err := resolveTarget("mir-9"); err == nil {
		t.Error("a forwarded lookup reached another host")
	}
}

// A send to a rig elsewhere is forwarded with an id minted here, which the
// sender's own log records on its out half; a reply to a message stamped with
// a place goes back through that place, correlated.
func TestCrossHostSendAndReply(t *testing.T) {
	ws := farHost(t, rigStatus{ID: "mir-9", Slug: "mir-9"})
	seen := filepath.Join(t.TempDir(), "seen")
	t.Setenv("SEEN", seen)
	standInRig(t, `echo "$* id=$RIG_FORWARDED_MESSAGE_ID reply=$RIG_FORWARDED_REPLY_TO from=$RIG_FORWARDED_SENDER" >> "$SEEN"`+"\n")

	dir := mkLocalRig(t, ws, manifest{ID: "cos-2026-10-10", Kind: "cos"})
	t.Chdir(dir)

	if err := runSend([]string{"mir-9", "status?"}); err != nil {
		t.Fatal(err)
	}
	thread := readRigMessages(dir)
	if len(thread) != 1 || thread[0].Dir != "out" || thread[0].To != "mir-9@fx" || !thread[0].Delivered {
		t.Fatalf("sender log = %+v, want one delivered out half to mir-9@fx", thread)
	}
	blob, _ := os.ReadFile(seen)
	if want := "send mir-9 status? id=" + thread[0].ID + " reply= from=rig:cos-2026-10-10@loopback"; !strings.Contains(string(blob), want) {
		t.Errorf("far side saw %q, want %q", blob, want)
	}

	// mir-9 answers; the far host would have stamped its address.
	in := rigMessage{V: 1, ID: "in-1", At: time.Now(), Dir: "in", From: "mir-9@fx", FromAddr: "rig:mir-9@fx", To: "cos-2026-10-10", Text: "done", Delivered: true}
	if err := appendRigMessage(dir, in); err != nil {
		t.Fatal(err)
	}
	if err := runReply([]string{"thanks"}); err != nil {
		t.Fatal(err)
	}
	blob, _ = os.ReadFile(seen)
	if !strings.Contains(string(blob), "send mir-9 thanks id=") || !strings.Contains(string(blob), "reply=in-1") {
		t.Errorf("reply reached the far side as %q, want a send to mir-9 answering in-1", blob)
	}
}

// Entering a parked rig on another host has its host wake it, and lands on
// the session that host reports rebuilding.
func TestRadarWakesParkedRemoteRig(t *testing.T) {
	farHost(t, rigStatus{ID: "pers-21", Slug: "pers-21", Parked: true})
	seen := filepath.Join(t.TempDir(), "seen")
	t.Setenv("SEEN", seen)
	standInRig(t, `echo "$*" >> "$SEEN"
echo '{"kind":"tmux","target":"~/workspaces/pers-21"}' >&3
`)
	host, ok := servedAt("fx")
	if !ok {
		t.Fatal("no surface fx")
	}
	parked := rigStatus{ID: "pers-21", Slug: "pers-21", Parked: true, Location: "fx",
		remote: &remoteRig{surface: host.Surface(), session: "stale"}}
	dest, err := radarPrepare(parked)
	if err != nil {
		t.Fatal(err)
	}
	if dest.session.name != "~/workspaces/pers-21" || dest.session.b.Surface() != host.Surface() {
		t.Errorf("destination = %q on %s, want the woken session through fx", dest.session.name, dest.session.b.Surface())
	}
	if blob, _ := os.ReadFile(seen); strings.TrimSpace(string(blob)) != "wake pers-21" {
		t.Errorf("far side ran %q, want wake pers-21", blob)
	}
}

// A forwarded command that would have attached hands its session back on the
// side channel, and the asking host gets it without it ever touching stdout.
func TestExecReportsAttach(t *testing.T) {
	standInRig(t, `echo '{"kind":"rex","target":"sess-1"}' >&3
echo "woke"
`)
	b := loopbackHost(t)
	var out bytes.Buffer
	attach, err := b.execAttach(execRequest{Argv: []string{"wake", "x"}}, &out, &out)
	if err != nil || attach == nil || attach.Kind != "rex" || attach.Target != "sess-1" {
		t.Fatalf("attach = %+v, %v", attach, err)
	}
	if strings.Contains(out.String(), "sess-1") {
		t.Errorf("attach frame leaked into output: %q", out.String())
	}
}

// Every other host's board joins this one's with its location, and a host
// that can't be reached is accounted for rather than silently absent.
func TestRemoteBoardDocs(t *testing.T) {
	standInRig(t, `echo '{"generatedAt":"2026-10-10T00:00:00Z","tier":"cheap","rigs":[{"id":"mir-9","slug":"mir-9","location":"local","repos":[]}]}'`+"\n")
	b := loopbackHost(t)
	dead, _ := newServedBackend("gone", "http://127.0.0.1:1")
	docs := remoteBoardDocs([]servedBackend{b, dead}, []string{"ls", "--format=json"})
	if len(docs) != 2 || !docs[0].host.Reachable || docs[0].doc == nil || len(docs[0].doc.Rigs) != 1 {
		t.Fatalf("docs = %+v", docs)
	}
	if docs[1].host.Reachable || docs[1].host.Error == "" {
		t.Errorf("unreachable host = %+v, want an account of it", docs[1].host)
	}
}
