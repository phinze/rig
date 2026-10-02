package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func remoteFixtureDoc() serveDoc {
	return serveDoc{
		Version: serveVersion,
		Icon:    "",
		Rigs:    []serveRig{{rigsForJSON([]rigStatus{{ID: "PERS-20", Slug: "pers-20", Title: "cross-host link"}}, false)[0], "~/workspaces/pers-20"}},
		Sessions: []serveSession{
			{Kind: "tmux", Name: "~/workspaces/pers-20"},
			{Kind: "tmux", Name: "notes", Path: "/home/p/notes"},
		},
		Panes: []servePane{{Kind: "tmux", Session: "~/workspaces/pers-20", Target: "%3", Command: "claude", Title: "Plan it"}},
	}
}

func fixtureServed(t *testing.T, place, endpoint string) servedBackend {
	t.Helper()
	b, err := newServedBackend(place, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// What the cache can vouch for depends only on its age: fresh is the live
// answer, older is a faint memory that still gets asked, past the cap is
// nothing, and only fresh reachability is ever repeated.
func TestRemoteCacheFreshness(t *testing.T) {
	now := time.Now()
	doc := remoteFixtureDoc()
	fresh := fixtureServed(t, "fresh", "http://fresh:1")
	old := fixtureServed(t, "old", "http://old:1")
	ancient := fixtureServed(t, "ancient", "http://ancient:1")
	down := fixtureServed(t, "down", "http://down:1")
	wasDown := fixtureServed(t, "wasdown", "http://wasdown:1")
	unknown := fixtureServed(t, "unknown", "http://unknown:1")
	cache := map[string]remoteCacheEntry{
		fresh.URL:   {At: now.Add(-time.Second), Doc: &doc},
		old.URL:     {At: now.Add(-2 * time.Minute), Doc: &doc},
		ancient.URL: {At: now.Add(-2 * remoteStaleCap), Doc: &doc},
		down.URL:    {At: now.Add(-time.Second), Down: true},
		wasDown.URL: {At: now.Add(-2 * time.Minute), Down: true},
	}
	served := []boardSource{fresh, old, ancient, down, wasDown, unknown}

	var seed radarRemoteMsg
	ask := seed.addCached(served, cache, now, true)
	seed.derive(now)
	asked := map[string]bool{}
	for _, b := range ask {
		asked[surfacePlace(b.Surface())] = true
	}
	for place, want := range map[string]bool{"fresh": false, "old": true, "ancient": true, "down": false, "wasdown": true, "unknown": true} {
		if asked[place] != want {
			t.Errorf("%s asked = %v, want %v", place, asked[place], want)
		}
	}
	if len(seed.rigs) != 2 || len(seed.sessions) != 4 {
		t.Errorf("seed has %d rigs / %d sessions, want fresh's and old's (2 / 4)", len(seed.rigs), len(seed.sessions))
	}
	if !seed.stale["old"] || seed.stale["fresh"] {
		t.Errorf("stale = %v, want old alone", seed.stale)
	}
	if len(seed.down) != 1 || seed.down[0] != "down" {
		t.Errorf("down = %v, want only the freshly recorded outage", seed.down)
	}
	if seed.icons["fresh"] != "" {
		t.Errorf("icons = %v, want the cached host's glyph", seed.icons)
	}
	if got := seed.agents[sessionKey{"tmux@fresh", "~/workspaces/pers-20"}]; len(got) != 1 {
		t.Errorf("fresh agents = %+v, want its cached pane", got)
	}

	// The live pass never paints from memory: old answers are asked, not shown.
	var live radarRemoteMsg
	live.addCached(served, cache, now, false)
	if len(live.rigs) != 1 || len(live.stale) != 0 {
		t.Errorf("live pass took %d rigs, stale %v; want fresh's alone", len(live.rigs), live.stale)
	}
}

// Asking records the answer, and a host that stops answering takes its board
// out of the file rather than leaving rows for the next open to paint.
func TestPollServedRecords(t *testing.T) {
	isolateRadarCache(t)
	doc := remoteFixtureDoc()
	srv := httptest.NewServer(serveHandler(newServeGate([]string{"nobody"}, nil, true), func() serveDoc { return doc }))
	b := fixtureServed(t, "fx", srv.URL)
	other := "http://elsewhere:1"
	recordRemote(map[string]remoteCacheEntry{other: {At: time.Now(), Doc: &doc}}, time.Now())

	pollServed([]boardSource{b}, time.Now())
	cache := loadRemoteCache()
	if e := cache[b.URL]; e.Doc == nil || len(e.Doc.Rigs) != 1 || e.Down {
		t.Fatalf("entry = %+v, want the board", e)
	}
	if _, ok := cache[other]; !ok {
		t.Error("recording one host erased another's entry")
	}

	srv.Close()
	pollServed([]boardSource{b}, time.Now())
	if e := loadRemoteCache()[b.URL]; e.Doc != nil || !e.Down {
		t.Errorf("entry after the host went away = %+v, want no board and down", e)
	}
}

// Entries past the cap leave the file on the next write.
func TestRecordRemotePrunes(t *testing.T) {
	isolateRadarCache(t)
	now := time.Now()
	recordRemote(map[string]remoteCacheEntry{"http://gone:1": {At: now.Add(-2 * remoteStaleCap)}}, now)
	recordRemote(map[string]remoteCacheEntry{"http://here:1": {At: now}}, now)
	cache := loadRemoteCache()
	if _, ok := cache["http://gone:1"]; ok || len(cache) != 1 {
		t.Errorf("cache = %+v, want only the recent entry", cache)
	}
}

// A board painted from memory marks its rows stale, rigs and plain sessions
// alike, and the live answer clears them.
func TestRadarStaleRemoteRows(t *testing.T) {
	registerFakeBackend(t, &fakeBackend{name: "tmux", surface: "tmux@fx"})
	now := time.Now()
	doc := remoteFixtureDoc()
	b := fixtureServed(t, "fx", "http://fx:1")
	cache := map[string]remoteCacheEntry{b.URL: {At: now.Add(-time.Minute), Doc: &doc}}

	var seed radarRemoteMsg
	seed.addCached([]boardSource{b}, cache, now, true)
	seed.derive(now)
	m := radarModel{home: "/home/p", prs: map[string][]rigPR{}, pending: map[string]bool{}, fetchedAt: map[string]time.Time{}, remote: seed}
	m.apply(radarScanMsg{statuses: []rigStatus{}, attached: map[sessionKey]int64{}})
	if len(m.inflight) != 1 || !m.inflight[0].stale || len(m.sessions) != 1 || !m.sessions[0].stale {
		t.Fatalf("inflight %+v / sessions %+v, want one stale rig and one stale session", m.inflight, m.sessions)
	}

	var live radarRemoteMsg
	live.add(b, remoteCacheEntry{At: now, Doc: &doc}, false)
	live.derive(now)
	next, _ := m.Update(live)
	m = next.(radarModel)
	if m.inflight[0].stale || m.sessions[0].stale {
		t.Error("rows still stale after the live answer")
	}
}

// Serve's cadence backs off from the radar's tick to the ceiling with the
// time since a radar last touched its heartbeat.
func TestServePollCadence(t *testing.T) {
	isolateRadarCache(t)
	if !lastRadarHeartbeat().IsZero() {
		t.Error("heartbeat before any radar touched it")
	}
	now := time.Now().Truncate(time.Second)
	touchRadarHeartbeat(now)
	if got := lastRadarHeartbeat(); !got.Equal(now) {
		t.Errorf("heartbeat = %v, want %v", got, now)
	}
	touchRadarHeartbeat(now.Add(time.Minute))
	if got := lastRadarHeartbeat(); !got.Equal(now.Add(time.Minute)) {
		t.Errorf("second touch left the heartbeat at %v", got)
	}

	for _, c := range []struct {
		since, want time.Duration
	}{
		{0, servePollEvery},
		{time.Minute, time.Minute},
		{10 * time.Minute, 10 * time.Minute},
		{time.Hour, servePollCeiling},
	} {
		if got := servePollInterval(c.since); got != c.want {
			t.Errorf("interval %v after looking = %v, want %v", c.since, got, c.want)
		}
	}

	last := now.Add(-servePollEvery)
	if !servePollDue(now, last, now) {
		t.Error("watched: want a poll every tick")
	}
	if servePollDue(now, last, now.Add(-time.Minute)) {
		t.Error("a minute away: polled two seconds after the last pass")
	}
	if !servePollDue(now, now.Add(-time.Minute), now.Add(-time.Minute)) {
		t.Error("a minute away: want a poll a minute after the last one")
	}
	if !servePollDue(now, time.Time{}, time.Time{}) {
		t.Error("never looked: want the first pass anyway")
	}
	if servePollDue(now, now.Add(-time.Minute), time.Time{}) {
		t.Error("never looked: want the ceiling, not a poll a minute on")
	}
}
