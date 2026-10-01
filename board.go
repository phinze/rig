package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The board is what `rig ls` reads: the cheap core from rigStatuses (identity,
// session state, agent attention) plus the local facts a per-rig manifest walk
// yields (branches, unaccounted work, tracker links). The full tier adds PR
// state on top and, with it, each rig's disposition.
//
// Both tiers go through here so a board fact lands in exactly one place. The
// radar keeps its own cached PRs and calls enrichWithPRs directly; everything
// else reaches these through runLs.

// boardPRTTL is how long a cached PR answer is trusted for the full tier. Much
// shorter than the radar's own TTL: the board consumer is a sweep that acts on
// what it reads, so it wants data this side of a CI run.
const boardPRTTL = time.Minute

// enrichBoardLocal fills the facts that need no network: each repo's branches
// and whether it holds unaccounted work, plus the rig's tracker links. One rig
// with a broken workspace must not blank the whole board, so every read here is
// degrade-quiet — see the mir-822 note on rigDirtyRepos. No PRs: the cheap tier
// leaves rigStatus.PRs nil, which the encoder renders as null per repo.
func enrichBoardLocal(statuses []rigStatus) {
	peers := boardPeers()
	for i := range statuses {
		enrichRowLocal(&statuses[i], peers)
	}
}

// boardPeers is the one docker call a board read makes for iso peers, shared
// by every row. Degrade-quiet like the rest of the local tier: a daemon that
// won't answer costs the peers lists, not the board.
func boardPeers() map[string][]isoPeer {
	peers, err := isoPeersByWorkspace()
	if err != nil {
		return nil
	}
	return peers
}

// enrichRowLocal is enrichBoardLocal's per-row half, also used by the full tier
// before the PR pass so both tiers agree on branches, WIP, links, and peers.
// peers is boardPeers' answer, keyed by resolved workspace path.
func enrichRowLocal(s *rigStatus, peers map[string][]isoPeer) {
	s.Links = rigLinks{URL: s.TrackerURL}
	if s.TrackerID != "" {
		id := s.TrackerID
		switch s.Tracker {
		case "linear":
			s.Links.Linear = &id
		case "vikunja", "tasks":
			// "tasks" is the older spelling some manifests still carry; both
			// mean a personal Vikunja task.
			s.Links.Vikunja = &id
		case "github":
			s.Links.GitHub = &id
		}
	}

	m, err := readManifest(s.Path)
	if err != nil {
		// A rig whose manifest won't read still lists with the repo names ls
		// already resolved. No branches or WIP — names only.
		s.LastActivity = boardLastActivity(*s)
		return
	}

	subdirs := make([]string, 0, len(m.Repos))
	for sub := range m.Repos {
		subdirs = append(subdirs, sub)
	}
	sort.Strings(subdirs)

	repos := make([]rigRepo, 0, len(subdirs))
	byName := make(map[string]int, len(subdirs)) // rigRepo index per owner/repo
	for _, sub := range subdirs {
		repo := rigRepo{Name: m.Repos[sub], Peers: peers[resolvePath(filepath.Join(s.Path, sub))]}
		if branches, err := repoBranches(m, sub, filepath.Join(s.Path, sub)); err == nil {
			repo.Branches = branches
		}
		byName[m.Repos[sub]] = len(repos)
		repos = append(repos, repo)
	}

	// rigDirtyRepos needs each repo's PR heads to tell pushed work from WIP. In
	// the cheap tier there are no PRs, so pushed-but-unqueried work reads as WIP
	// — conservative on purpose.
	for _, d := range rigDirtyRepos(s.Path, m, s.PRs) {
		uncertain := strings.HasSuffix(d, "?")
		sub := strings.TrimSuffix(d, "?")
		if j, ok := byName[m.Repos[sub]]; ok {
			repos[j].WIP = true
			repos[j].WIPUncertain = uncertain
		}
	}

	// The names-only repos rigStatuses built are replaced wholesale by the
	// manifest-ordered ones so the JSON reads subdir-sorted.
	s.Repos = repos
	s.LastActivity = boardLastActivity(*s)
}

// enrichBoardPRs is the full tier: local facts first, then PR state. It reuses
// the radar's PR cache for answers younger than boardPRTTL and fans one
// enrichWithPRs pass over whatever is stale or absent. Fresh answers go back
// into the same cache, so a board read warms the radar and vice versa. Returns
// the timestamp of the oldest answer used; fresh answers count as now.
func enrichBoardPRs(statuses []rigStatus, refresh bool, now time.Time) *time.Time {
	peers := boardPeers()
	for i := range statuses {
		enrichRowLocal(&statuses[i], peers)
	}

	cache := map[string]radarCacheEntry{}
	if !refresh {
		// loadRadarCache degrades to nil on a missing or torn file, which is a
		// fine read but panics on the first write below — normalize it away.
		if loaded := loadRadarCache(); loaded != nil {
			cache = loaded
		}
	}

	oldest := now
	var stale []int
	for i := range statuses {
		e, ok := cache[statuses[i].Slug]
		if ok && now.Sub(e.At) < boardPRTTL {
			statuses[i].PRs = e.PRs
			if e.At.Before(oldest) {
				oldest = e.At
			}
			continue
		}
		stale = append(stale, i)
	}
	if len(stale) > 0 {
		// enrichWithPRs clears and repopulates the PRs it's handed, so the stale
		// rigs go in as their own slice and their answers copy back.
		sub := make([]rigStatus, len(stale))
		for j, i := range stale {
			sub[j] = statuses[i]
		}
		enrichWithPRs(sub)
		for j, i := range stale {
			statuses[i].PRs = sub[j].PRs
			cache[statuses[i].Slug] = radarCacheEntry{At: now, PRs: statuses[i].PRs}
		}

		// Write the merged map back whole: saveRadarCache replaces the file with
		// what it's given, so passing only the fresh rigs would evict every cache
		// entry this run happened to reuse.
		prs := make(map[string][]rigPR, len(cache))
		at := make(map[string]time.Time, len(cache))
		for slug, e := range cache {
			prs[slug] = e.PRs
			at[slug] = e.At
		}
		saveRadarCache(prs, at)
	}

	for i := range statuses {
		statuses[i].Disposition = parkedDisposition(statuses[i].PRs)
	}
	return &oldest
}

// boardLastActivity is max(agent turn, message log mtime). The message log
// belongs to the messaging milestone; stat anyway so the field's meaning does
// not drift when the log lands.
func boardLastActivity(s rigStatus) *time.Time {
	latest := s.LastActive
	if info, err := os.Stat(filepath.Join(s.Path, ".rig", "messages.jsonl")); err == nil {
		if t := info.ModTime(); latest == nil || t.After(*latest) {
			latest = &t
		}
	}
	return latest
}

// boardInboxFor condenses a rig's pinned notifications to a count and the
// loudest level, mirroring the badge ls puts on the row. Full bodies stay in
// `rig notify list`, which is where a sweep that cares should go next.
func boardInboxFor(pinned []notification) rigInbox {
	box := rigInbox{NotifyCount: len(pinned)}
	for _, n := range pinned {
		if box.WorstLevel == "" || notifyLevels[n.Level] > notifyLevels[box.WorstLevel] {
			box.WorstLevel = n.Level
		}
	}
	return box
}
