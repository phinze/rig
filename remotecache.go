package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/phinze/rig/internal/mux"
)

// The remote cache is how a radar paints other hosts' rigs on its first
// frame. Every popup is a fresh process, so on its own it starts with no
// remote board and no memory of which hosts were down, and pays a tailnet
// round trip (or a whole timeout) before either shows. `rig serve` runs all
// day, so it polls the configured rig surfaces and writes what it heard here;
// a radar reads the file instead of the network. A radar on a machine without
// serve asks for itself and writes the same file, so its next open still
// paints from the last answer.
//
// One file, two writers, and freshness decides who a reader believes. An
// entry younger than remoteFreshFor was written by something still polling,
// so it is the live answer, reachability included, and the radar doesn't ask.
// An older one is only a memory: its rows paint faint while the radar asks,
// and its reachability is never repeated, since a host that was down an hour
// ago may well be up now and one that was up may be gone.

const remoteCacheFile = "radar-remote.json"

// servePollEvery is serve's pass over the surfaces it can see while a radar
// is open, at the radar's own tick so a radar reading the file sees what it
// would have asked for. It's also the floor of the backoff below.
const servePollEvery = radarTickEvery

// servePollCeiling is the slowest serve ever polls: what an afk machine
// spends, and so the oldest board the next open's faint first frame can show.
const servePollCeiling = 30 * time.Minute

// radarHeartbeatFile is touched on every radar tick, so serve can tell a
// board someone is watching from one nobody is. Its mtime is the whole
// message.
const radarHeartbeatFile = "radar-open"

// remoteFreshFor is how long an entry counts as the live answer: a few of
// serve's passes, so one slow fetch doesn't send every open radar to the
// network.
const remoteFreshFor = 5 * servePollEvery

// remoteStaleCap is the oldest answer still worth painting at all, faint. It
// is generous because the usual old answer is the board from before a laptop
// slept, and rigs outlive a night: painting it costs a second of faint rows,
// and leaving it out costs the first frame. A host that's actually gone is
// handled elsewhere, since a failed poll removes its board outright.
const remoteStaleCap = 24 * time.Hour

// remoteCacheEntry is one rig surface's last answer, keyed in the file by its
// serve URL (the breaker's key too). A surface that answered stores its board
// in wire form, so reading it back goes through the same lift as a fetch. One
// that didn't stores no board at all: the rows a dead host last showed are
// exactly the ones a reader must not paint.
type remoteCacheEntry struct {
	At   time.Time `json:"at"`
	Doc  *serveDoc `json:"doc,omitempty"`
	Down bool      `json:"down,omitempty"` // couldn't connect, as opposed to refused
}

func loadRemoteCache() map[string]remoteCacheEntry {
	var entries map[string]remoteCacheEntry
	readCacheFile(remoteCacheFile, &entries)
	return entries
}

// recordRemote merges fresh answers into the file. It rereads first so two
// writers asking about different hosts don't erase each other, and it drops
// whatever has aged past the cap, which is how a surface removed from the
// config eventually leaves.
func recordRemote(answers map[string]remoteCacheEntry, now time.Time) {
	if len(answers) == 0 {
		return
	}
	entries := loadRemoteCache()
	if entries == nil {
		entries = map[string]remoteCacheEntry{}
	}
	for url, e := range answers {
		entries[url] = e
	}
	for url, e := range entries {
		if now.Sub(e.At) > remoteStaleCap {
			delete(entries, url)
		}
	}
	writeCacheFile(remoteCacheFile, entries)
}

// poll asks the surface now, in cache form.
func (b servedBackend) poll(now time.Time) remoteCacheEntry {
	doc, err := b.fetch()
	if err != nil {
		return remoteCacheEntry{At: now, Down: b.Unreachable()}
	}
	return remoteCacheEntry{At: now, Doc: &doc}
}

// addCached splits the served surfaces into those the cache can answer for and
// those that have to be asked. What it can answer for is folded into msg:
// fresh entries as the live board, older ones (when stale is allowed) as
// faint rows. Only a fresh entry ever names a host as down.
func (msg *radarRemoteMsg) addCached(served []boardSource, cache map[string]remoteCacheEntry, now time.Time, stale bool) (ask []boardSource) {
	for _, b := range served {
		e, ok := cache[b.Endpoint()]
		switch age := now.Sub(e.At); {
		case ok && age < remoteFreshFor:
			msg.add(b, e, false)
		case ok && stale && age < remoteStaleCap && e.Doc != nil:
			msg.add(b, e, true)
			ask = append(ask, b)
		default:
			ask = append(ask, b)
		}
	}
	return ask
}

// add folds one surface's answer into the pass.
func (msg *radarRemoteMsg) add(b boardSource, e remoteCacheEntry, stale bool) {
	place := surfacePlace(b.Surface())
	if e.Doc == nil {
		if e.Down && !stale {
			msg.down = append(msg.down, place)
		}
		return
	}
	board := b.lift(*e.Doc)
	if msg.icons == nil {
		msg.icons = map[string]string{}
	}
	if board.icon != "" {
		msg.icons[place] = board.icon
	}
	if stale {
		if msg.stale == nil {
			msg.stale = map[string]bool{}
		}
		msg.stale[place] = true
	}
	msg.sessions = append(msg.sessions, board.sessions...)
	msg.rigs = append(msg.rigs, board.rigs...)
	msg.panes = append(msg.panes, board.panes...)
}

// servedSurfaces is the configured surfaces that run `rig serve`.
func servedSurfaces() []boardSource {
	var out []boardSource
	for _, b := range surfaceBackends() {
		if s, ok := b.(boardSource); ok {
			out = append(out, s)
		}
	}
	return out
}

// pollServed asks every served surface at once and records the answers.
func pollServed(served []boardSource, now time.Time) map[string]remoteCacheEntry {
	backends := make([]mux.Backend, len(served))
	for i, b := range served {
		backends[i] = b
	}
	polled := eachBackend(backends, func(b mux.Backend) remoteCacheEntry {
		return b.(boardSource).poll(now)
	})
	answers := make(map[string]remoteCacheEntry, len(served))
	for i, b := range served {
		answers[b.Endpoint()] = polled[i]
	}
	recordRemote(answers, now)
	return answers
}

// touchRadarHeartbeat says a radar is open on this machine.
func touchRadarHeartbeat(now time.Time) {
	path, err := cacheFilePath(radarHeartbeatFile)
	if err != nil {
		return
	}
	if os.Chtimes(path, now, now) == nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o755) == nil && os.WriteFile(path, nil, 0o644) == nil {
		_ = os.Chtimes(path, now, now)
	}
}

// lastRadarHeartbeat is when a radar on this machine last ticked, zero if
// none ever has.
func lastRadarHeartbeat() time.Time {
	path, err := cacheFilePath(radarHeartbeatFile)
	if err != nil {
		return time.Time{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// servePollInterval backs off with how long it's been since anyone looked:
// wait that long again, between the radar's tick and the ceiling. That doubles
// the gap on every pass (2s, 4s, 8s, ...), so serve is at the ceiling within
// the hour and a night afk costs a couple dozen polls, most of them in the
// minutes after the popup closed, when you're likeliest to open it again.
func servePollInterval(sinceLooked time.Duration) time.Duration {
	return min(max(sinceLooked, servePollEvery), servePollCeiling)
}

// servePollDue is whether serve's loop should poll on this tick.
func servePollDue(now, last, looked time.Time) bool {
	since := servePollCeiling * 10 // never looked: the ceiling
	if !looked.IsZero() {
		since = now.Sub(looked)
	}
	return now.Sub(last) >= servePollInterval(since)
}

// pollServedSurfaces is serve's half: keep the remote cache fresh for every
// radar on this machine, on the heartbeat's backoff. It wakes every tick but
// only stats the heartbeat unless a poll is due. The surface list is reread each pass so a
// config change doesn't need a restart. Nothing here feeds /v1/board, which
// describes this host alone; folding what it heard back in would have two
// hosts re-exporting each other's boards forever.
func pollServedSurfaces(ctx context.Context, tick time.Duration) {
	var last time.Time
	for {
		// Wall time, not monotonic: a Mac's monotonic clock stops while it
		// sleeps, so a night away would otherwise read as a moment.
		if now := time.Now().Round(0); servePollDue(now, last, lastRadarHeartbeat()) {
			pollServed(servedSurfaces(), now)
			last = now
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(tick):
		}
	}
}
