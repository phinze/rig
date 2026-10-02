package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phinze/rig/internal/mux"
)

// The gate answers only the allow list. A tagged node's login is a
// placeholder that names nobody, so allowing it would let in every tagged
// device on the tailnet; it has to be named by node or tag instead.
func TestServeGate(t *testing.T) {
	ids := map[string]whoisIdentity{
		"100.0.0.1": {Login: "me@github", Node: "laptop"},
		"100.0.0.2": {Login: "tagged-devices", Node: "foxtrotbase", Tags: []string{"tag:terraform"}},
		"100.0.0.3": {Login: "someone@github", Node: "theirs"},
		"100.0.0.4": {Login: "tagged-devices", Node: "printer", Tags: []string{"tag:iot"}},
	}
	calls := 0
	whois := func(_ context.Context, ip string) (whoisIdentity, error) {
		calls++
		id, ok := ids[ip]
		if !ok {
			return whoisIdentity{}, errors.New("no such peer")
		}
		return id, nil
	}
	g := newServeGate([]string{"me@github", "foxtrotbase", "tagged-devices"}, whois, false)
	for addr, want := range map[string]bool{
		"100.0.0.1:5000": true,  // by login
		"100.0.0.2:5000": true,  // tagged, by node name
		"100.0.0.3:5000": false, // someone else
		"100.0.0.4:5000": false, // tagged, and "tagged-devices" on the list names nobody
		"100.0.0.9:5000": false, // whois can't place it
		"127.0.0.1:5000": false, // loopback, on a tailnet listener
	} {
		if got := g.check(context.Background(), addr) == nil; got != want {
			t.Errorf("%s allowed = %v, want %v", addr, got, want)
		}
	}

	before := calls
	_ = g.check(context.Background(), "100.0.0.1:6000")
	_ = g.check(context.Background(), "100.0.0.3:6000")
	if calls != before {
		t.Errorf("whois ran %d more times for addresses already answered, want 0", calls-before)
	}
	_ = g.check(context.Background(), "100.0.0.9:6000")
	if calls != before+1 {
		t.Errorf("a failed whois was cached; want it asked again")
	}

	if tg := newServeGate([]string{"tag:terraform"}, whois, false); tg.check(context.Background(), "100.0.0.2:1") != nil {
		t.Error("tag:terraform on the list didn't admit a node carrying it")
	}
	if lg := newServeGate([]string{"me@github"}, whois, true); lg.check(context.Background(), "127.0.0.1:1") != nil {
		t.Error("a loopback listener refused a loopback peer")
	}
}

// What serve writes, a rig surface reads back as this machine's types: rigs
// become remote rig rows with their PRs flattened and their session as the
// host named it, every row is tagged with the surface, and rows from a
// multiplexer the surface can't enter are dropped.
func TestServedRoundTrip(t *testing.T) {
	pr := rigPR{Repo: "phinze/rig", Branch: "b", prInfo: prInfo{Number: 7, State: "OPEN", Checks: "passing"}}
	looked := rigStatus{ID: "PERS-20", Slug: "pers-20", Title: "cross-host link", Path: "/home/phinze/workspaces/pers-20",
		Repos: []rigRepo{{Name: "phinze/rig"}}, PRs: []rigPR{pr}, Parked: true}
	unlooked := rigStatus{ID: "PERS-21", Slug: "pers-21", Title: "next", Path: "/home/phinze/workspaces/pers-21",
		Repos: []rigRepo{{Name: "phinze/rig"}}}
	rexRig := rigStatus{ID: "PERS-22", Slug: "pers-22", Path: "/home/phinze/workspaces/pers-22", Backend: "rex"}
	doc := serveDoc{
		Version: serveVersion,
		Icon:    "\uf233",
		Rigs: []serveRig{
			{rigsForJSON([]rigStatus{looked}, true)[0], "~/workspaces/pers-20"},
			{rigsForJSON([]rigStatus{unlooked}, false)[0], "~/workspaces/pers-21"},
			{rigsForJSON([]rigStatus{rexRig}, false)[0], "~/workspaces/pers-22"},
		},
		Sessions: []serveSession{
			{Kind: "tmux", Name: "~/workspaces/pers-21", Path: "/home/phinze/workspaces/pers-21", LastAttached: 42},
			{Kind: "rex", Name: "elsewhere"},
		},
		Panes: []servePane{
			{Kind: "tmux", Session: "~/workspaces/pers-21", Target: "%3", Command: "claude", Title: "Plan it"},
			{Kind: "rex", Session: "elsewhere", Target: "b1", Command: "claude"},
		},
	}

	gate := newServeGate([]string{"nobody"}, nil, true)
	srv := httptest.NewServer(serveHandler(gate, func() serveDoc { return doc }))
	defer srv.Close()

	b, err := newServedBackend("fx", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	board, err := b.Board()
	if err != nil {
		t.Fatal(err)
	}
	if len(board.rigs) != 2 {
		t.Fatalf("rigs = %+v, want the two tmux rigs", board.rigs)
	}
	got := board.rigs[0]
	if got.remote == nil || got.remote.surface != "tmux@fx" || got.remote.session != "~/workspaces/pers-20" || !got.remote.prsLooked {
		t.Errorf("remote = %+v, want tmux@fx, the host's session name, PRs looked", got.remote)
	}
	if len(got.PRs) != 1 || got.PRs[0].Number != 7 || got.PRs[0].Checks != "passing" || !got.Parked {
		t.Errorf("rig = %+v, want PR #7 passing, parked", got)
	}
	if board.rigs[1].remote.prsLooked || board.rigs[1].PRs != nil {
		t.Errorf("unlooked rig = %+v, want no PR answer", board.rigs[1])
	}
	if len(board.sessions) != 1 || board.sessions[0].Surface != "tmux@fx" || board.sessions[0].LastAttached != 42 {
		t.Errorf("sessions = %+v, want the tmux one, tagged", board.sessions)
	}
	if len(board.panes) != 1 || board.panes[0].Surface != "tmux@fx" || board.panes[0].Target != "%3" {
		t.Errorf("panes = %+v, want the tmux agent, tagged", board.panes)
	}
	if board.icon != "\uf233" || got.remote.icon != "\uf233" {
		t.Errorf("icon = %q / %q, want the host's on the board and its rigs", board.icon, got.remote.icon)
	}
	if b.Unreachable() {
		t.Error("a surface that answered reads as unreachable")
	}
}

// A refused request is an answer, not an outage: it shouldn't trip the
// breaker, and nothing it carried should land on the board.
func TestServedRefusalIsNotUnreachable(t *testing.T) {
	stranger := func(context.Context, string) (whoisIdentity, error) {
		return whoisIdentity{}, errors.New("not on the tailnet")
	}
	gate := newServeGate([]string{"me@github"}, stranger, false) // tailnet gate, loopback peer
	srv := httptest.NewServer(serveHandler(gate, func() serveDoc { t.Fatal("built a doc for a refused peer"); return serveDoc{} }))
	defer srv.Close()
	b, err := newServedBackend("fx", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Board(); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want a 403", err)
	}
	if b.Unreachable() {
		t.Error("a refusal tripped the breaker")
	}
}

func TestParseRigSurface(t *testing.T) {
	b, err := parseSurface("foxtrotbase", "rig+http://foxtrotbase")
	if err != nil {
		t.Fatal(err)
	}
	if b.Surface() != "tmux@foxtrotbase" || b.Endpoint() != "http://foxtrotbase:7744" {
		t.Errorf("surface %s at %s, want tmux@foxtrotbase at the default port", b.Surface(), b.Endpoint())
	}
	if _, ok := b.(boardSource); !ok {
		t.Error("a rig surface can't describe its rigs")
	}
	if _, err := parseSurface("fx", "rig+ssh://fx"); err == nil {
		t.Error("rig+ssh parsed; want http only")
	}
}

// A remote rig is a real rig row, apart from a local rig with the same slug,
// carrying its host's agents, and viewed rather than changed from here.
func TestRadarDrawsRemoteRigs(t *testing.T) {
	registerFakeBackend(t, &fakeBackend{name: "tmux", surface: "tmux@fx"})
	remote := rigStatus{ID: "PERS-20", Slug: "pers-20", Title: "cross-host link", Path: "/home/phinze/workspaces/pers-20",
		remote: &remoteRig{surface: "tmux@fx", session: "~/workspaces/pers-20", prsLooked: true}}
	parked := rigStatus{ID: "PERS-21", Slug: "pers-21", Title: "later", Path: "/home/phinze/workspaces/pers-21", Parked: true,
		remote: &remoteRig{surface: "tmux@fx", session: "~/workspaces/pers-21"}}
	local := rigStatus{ID: "PERS-20", Slug: "pers-20", Title: "same slug here", Path: "/work/pers-20"}
	far := sessionKey{"tmux@fx", "~/workspaces/pers-20"}

	m := radarModel{prs: map[string][]rigPR{"pers-20": {{prInfo: prInfo{Number: 1}}}}, pending: map[string]bool{}, fetchedAt: map[string]time.Time{}}
	m.remote = radarRemoteMsg{
		rigs:     []rigStatus{remote, parked},
		sessions: []mux.Session{{Surface: far.surface, Name: far.name}},
		agents:   map[sessionKey][]agentChild{far: {{Surface: far.surface, Target: "%3", Context: "Plan it"}}},
	}
	m.apply(radarScanMsg{statuses: []rigStatus{local}, attached: map[sessionKey]int64{}})

	if len(m.sessions) != 0 {
		t.Errorf("bare rows = %+v, want the remote rig's session folded into its rig", m.sessions)
	}
	if len(m.inflight) != 2 || len(m.parked) != 1 {
		t.Fatalf("inflight %d / parked %d, want 2 / 1", len(m.inflight), len(m.parked))
	}
	var r rigStatus
	for _, s := range m.inflight {
		if s.remote != nil {
			r = s
		}
	}
	if len(r.agents) != 1 || r.agents[0].Context != "Plan it" {
		t.Errorf("remote rig agents = %+v, want its host's agent", r.agents)
	}
	if len(r.PRs) != 0 {
		t.Errorf("remote rig took the local pers-20's cached PRs: %+v", r.PRs)
	}
	if rowKey(r) == rowKey(local) {
		t.Error("remote and local rigs with one slug share a row key")
	}
	if got := radarRowTitle(r); got != "fx: cross-host link" {
		t.Errorf("title = %q, want the place in front", got)
	}
	if cmds := m.fetchMissing(); len(cmds) != 1 {
		t.Errorf("PR fetches = %d, want only the local rig's", len(cmds))
	}

	dest, err := radarPrepare(r)
	if err != nil || dest.session.name != "~/workspaces/pers-20" || dest.session.b.Surface() != "tmux@fx" {
		t.Errorf("enter = %+v, %v; want the host's session through its surface", dest.session, err)
	}
	if _, err := radarPrepare(m.parked[0]); err == nil || !strings.Contains(err.Error(), "wake it there") {
		t.Errorf("enter on a parked remote rig = %v, want a refusal", err)
	}
}

// A host that chose an icon is drawn by it, on its rigs and its plain
// sessions alike, and still found by its name.
func TestRadarDrawsHostIcons(t *testing.T) {
	registerFakeBackend(t, &fakeBackend{name: "tmux", surface: "tmux@fx"})
	r := rigStatus{ID: "PERS-20", Slug: "pers-20", Title: "cross-host link",
		remote: &remoteRig{surface: "tmux@fx", session: "~/workspaces/pers-20", icon: ""}}
	if got := radarRowTitle(r); got != " cross-host link" {
		t.Errorf("rig title = %q, want the icon in front", got)
	}

	m := radarModel{home: "/home/me", prs: map[string][]rigPR{}}
	m.remote = radarRemoteMsg{
		sessions: []mux.Session{{Surface: "tmux@fx", Name: "notes", Path: "/home/p/notes"}},
		icons:    map[string]string{"fx": ""},
	}
	m.apply(radarScanMsg{attached: map[sessionKey]int64{}})
	if len(m.sessions) != 1 || m.sessions[0].Title != " ~/notes" {
		t.Fatalf("bare rows = %+v, want the icon in place of fx:", m.sessions)
	}
	m.setFilter("fx")
	if rows := m.rows(); len(rows) != 1 {
		t.Errorf("filtering by place found %d rows, want the fx session", len(rows))
	}
}
