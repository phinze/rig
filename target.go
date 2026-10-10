package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/phinze/rig/internal/mux"
)

// A target is the rig a verb acts on, wherever it lives. Resolution is local
// first, so every query that worked before this file existed means exactly
// what it meant, and a rig on another host is reached two ways: by a query
// nothing here matches, or by naming the place outright (`mir-12@foxtrotbase`).
// The second is how you reach past a local rig that shares an id, which local
// always wins: a review rig for the same PR on two hosts, say.
//
// `cos` is the one query that looks everywhere even when it matches here,
// because the alias means the newest chief of staff and today's may be on the
// other host while yesterday's is still up on this one.

type remoteTarget struct {
	host  servedBackend
	place string
	row   rigStatus // as its host described it
}

// addr is how other hosts spell this rig.
func (t remoteTarget) addr() string { return t.row.ID + "@" + t.place }

type rigTarget struct {
	local  *rigInfo
	remote *remoteTarget
}

func (t rigTarget) label() string {
	if t.remote != nil {
		return t.remote.addr()
	}
	return t.local.ID
}

// splitPlace separates `id@place`. Rig ids never carry an @, so only a suffix
// that reads as a place counts; anything else is left for matching to refuse.
func splitPlace(query string) (id, place string) {
	i := strings.LastIndex(query, "@")
	if i <= 0 || !validPlace(query[i+1:]) {
		return query, ""
	}
	return query[:i], query[i+1:]
}

// matchRigs is the shared notion of "this rig answers to that query": id,
// directory slug, or tracker id, case-insensitively.
func matchRigs[T any](rows []T, query string, fields func(T) (id, slug, tracker string)) []T {
	var out []T
	for _, r := range rows {
		id, slug, tracker := fields(r)
		if strings.EqualFold(id, query) || strings.EqualFold(slug, query) ||
			(tracker != "" && strings.EqualFold(tracker, query)) {
			out = append(out, r)
		}
	}
	return out
}

func infoFields(r rigInfo) (string, string, string) { return r.ID, r.Slug, r.TrackerID }
func remoteFields(t remoteTarget) (string, string, string) {
	return t.row.ID, t.row.Slug, t.row.TrackerID
}

// thisPlace is the name other hosts know this one by: its short hostname,
// which is also its tailnet node name on every machine rig runs on.
func thisPlace() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	return h
}

// servedAt is the rig surface for a place: by its name in the surfaces table,
// else by its endpoint's host, since the tailnet stamps senders by node name
// and a table may have named the same host something friendlier.
func servedAt(place string) (servedBackend, bool) {
	var byHost *servedBackend
	for _, s := range servedSurfaces() {
		b, ok := s.(servedBackend)
		if !ok {
			continue
		}
		if surfacePlace(b.Surface()) == place {
			return b, true
		}
		if host, _, _ := strings.Cut(b.Host, "."); host == place && byHost == nil {
			byHost = &b
		}
	}
	if byHost != nil {
		return *byHost, true
	}
	return servedBackend{}, false
}

// remoteRigs is every rig the served surfaces describe, answered from the
// remote cache when serve has it fresh and asked live otherwise, with the
// places that couldn't answer named so a miss can say it might not be one.
func remoteRigs(only []servedBackend) (rigs []remoteTarget, down []string) {
	if localOnly {
		return nil, nil
	}
	var served []boardSource
	if only != nil {
		for _, b := range only {
			served = append(served, b)
		}
	} else {
		served = servedSurfaces()
	}
	if len(served) == 0 {
		return nil, nil
	}
	now := time.Now()
	cache := loadRemoteCache()
	var ask []boardSource
	answers := map[string]remoteCacheEntry{}
	for _, b := range served {
		if e, ok := cache[b.Endpoint()]; ok && now.Sub(e.At) < remoteFreshFor {
			answers[b.Endpoint()] = e
			continue
		}
		ask = append(ask, b)
	}
	for url, e := range pollServed(ask, now, false) {
		answers[url] = e
	}
	for _, s := range served {
		b, ok := s.(servedBackend)
		if !ok {
			continue
		}
		place := surfacePlace(b.Surface())
		e := answers[b.Endpoint()]
		if e.Doc == nil {
			down = append(down, place)
			continue
		}
		for _, row := range b.lift(*e.Doc).rigs {
			rigs = append(rigs, remoteTarget{host: b, place: place, row: row})
		}
	}
	return rigs, down
}

// resolveTarget answers "which rig, on which host, is <query>".
func resolveTarget(query string) (rigTarget, error) {
	id, place := splitPlace(query)
	if place != "" && place != thisPlace() {
		if localOnly {
			return rigTarget{}, fmt.Errorf("%s isn't on this host", query)
		}
		host, ok := servedAt(place)
		if !ok {
			return rigTarget{}, fmt.Errorf("no rig surface for %s (add it to [surfaces] as rig+http://%s)", place, place)
		}
		rows, down := remoteRigs([]servedBackend{host})
		if len(down) > 0 {
			return rigTarget{}, fmt.Errorf("can't reach %s", place)
		}
		return pickRemote(query, matchRigs(rows, id, remoteFields), nil, id == cosAddress, rows)
	}

	rigs, err := listRigs()
	if err != nil {
		return rigTarget{}, err
	}
	local := matchRigs(rigs, id, infoFields)
	switch {
	case len(local) == 1:
		return rigTarget{local: &local[0]}, nil
	case len(local) > 1:
		return rigTarget{}, fmt.Errorf("rig query %q is ambiguous", query)
	}

	if strings.EqualFold(id, cosAddress) && place == "" {
		return resolveCoSAcross(rigs)
	}
	if place != "" { // named this host outright
		return rigTarget{}, fmt.Errorf("no rig matches %q", query)
	}
	rows, down := remoteRigs(nil)
	return pickRemote(query, matchRigs(rows, id, remoteFields), down, false, rows)
}

func pickRemote(query string, matches []remoteTarget, down []string, cos bool, all []remoteTarget) (rigTarget, error) {
	if len(matches) == 0 && cos {
		if t, ok := newestCoS(all); ok {
			return rigTarget{remote: &t}, nil
		}
	}
	switch len(matches) {
	case 1:
		return rigTarget{remote: &matches[0]}, nil
	case 0:
		msg := fmt.Sprintf("no rig matches %q", query)
		if len(down) > 0 {
			msg += fmt.Sprintf(" here or on any host that answered (couldn't reach %s)", strings.Join(down, ", "))
		}
		return rigTarget{}, fmt.Errorf("%s", msg)
	default:
		var names []string
		for _, m := range matches {
			names = append(names, m.addr())
		}
		return rigTarget{}, fmt.Errorf("rig query %q is ambiguous: %s", query, strings.Join(names, ", "))
	}
}

func newestCoS(rows []remoteTarget) (remoteTarget, bool) {
	var best *remoteTarget
	for i, r := range rows {
		if r.row.Kind == "cos" && (best == nil || r.row.ID > best.row.ID) {
			best = &rows[i]
		}
	}
	if best == nil {
		return remoteTarget{}, false
	}
	return *best, true
}

// resolveCoSAcross is the cos alias over every host: the newest workday wins,
// and the same workday on two hosts is refused by name, since two chiefs of
// staff on one day is a mistake and quietly picking one would split the day's
// reports between them.
func resolveCoSAcross(rigs []rigInfo) (rigTarget, error) {
	localBest, haveLocal := resolveCoS(rigs)
	rows, down := remoteRigs(nil)
	remoteBest, haveRemote := newestCoS(rows)
	switch {
	case haveLocal && haveRemote && localBest.ID == remoteBest.row.ID:
		return rigTarget{}, fmt.Errorf("two chiefs of staff for the same day: %s here and %s; send to one by name", localBest.ID, remoteBest.addr())
	case haveLocal && (!haveRemote || localBest.ID > remoteBest.row.ID):
		return rigTarget{local: &localBest}, nil
	case haveRemote:
		return rigTarget{remote: &remoteBest}, nil
	}
	msg := "no chief-of-staff rig is up (start one with `rig cos`)"
	if len(down) > 0 {
		msg += fmt.Sprintf("; couldn't reach %s", strings.Join(down, ", "))
	}
	return rigTarget{}, fmt.Errorf("%s", msg)
}

// remoteHosts is every rig surface, for the verbs that fan out to all of them.
func remoteHosts() []servedBackend {
	if localOnly {
		return nil
	}
	var out []servedBackend
	for _, s := range servedSurfaces() {
		if b, ok := s.(servedBackend); ok {
			out = append(out, b)
		}
	}
	return out
}

// eachHost runs f against every rig surface at once, in surface order.
func eachHost[T any](hosts []servedBackend, f func(servedBackend) T) []T {
	backends := make([]mux.Backend, len(hosts))
	for i, h := range hosts {
		backends[i] = h
	}
	return eachBackend(backends, func(b mux.Backend) T { return f(b.(servedBackend)) })
}
