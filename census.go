package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// census is the board as data: one JSON document covering every rig, meant
// for the chief-of-staff skill and any other consumer that would otherwise
// scrape `rig ls` tables. `rig ls --full` remains the human table over the
// same facts; census exists because the reader here is a program.
//
// Two tiers answer the cost question the sketch raised. The full tier (the
// default) includes PR and CI state, leaning on the radar's PR cache so a
// sweep that polls is not one gh fan-out per poll. The cheap tier (--cheap)
// is board state only — no gh calls at all — for the frequent pass, at the
// price of null PRs and a conservative WIP read.
//
// The shape follows memex/Projects/Ideas/chief-of-staff.md. Fields the sketch
// pinned to the messaging milestone (agent pid, reachability, message
// timestamps) are absent until `rig send` exists, deliberately: emitting them
// now would pin guesses as API.

// censusPRTTL is how long a cached PR answer is trusted for the full tier.
// Much shorter than the radar's own TTL: the census consumer is a sweep that
// acts on what it reads, so it wants data this side of a CI run.
const censusPRTTL = time.Minute

type censusDoc struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Tier        string    `json:"tier"` // full | cheap
	// PRsAsOf is the oldest PR answer in the document, full tier only, so the
	// reader can judge how stale the most-cached row is allowed to be.
	PRsAsOf *time.Time `json:"prsAsOf,omitempty"`
	// LooseNotifications are the inbox entries pinned to no rig — what ls and
	// the radar draw as a banner. They're top-level because a stalled cron
	// bears on the whole sweep, not on one row.
	LooseNotifications []notification `json:"looseNotifications,omitempty"`
	Rigs               []censusRig    `json:"rigs"`
}

type censusRig struct {
	ID       string      `json:"id"`
	Slug     string      `json:"slug"`
	Title    string      `json:"title"`
	Kind     string      `json:"kind,omitempty"` // review | project; absent means authoring
	Created  time.Time   `json:"created"`
	Age      string      `json:"age"` // "2d" convenience twin of Created
	State    string      `json:"state"`
	Backend  string      `json:"backend"`
	Location string      `json:"location"`
	Agent    censusAgent `json:"agent"`
	// Repos is a possibly-empty list, never null, same contract as ls: a
	// repositoryless project rig reads as [].
	Repos        []censusRepo `json:"repos"`
	LastActivity *time.Time   `json:"lastActivity,omitempty"`
	Links        censusLinks  `json:"links"`
	// Disposition is parkedDisposition's vocabulary ("waiting", "approved",
	// ...) computed over every rig, not just parked ones: it is the "what
	// does this rig want" answer sweep and waiting share. Full tier only —
	// it needs PR state — so cheap reads as absent.
	Disposition string      `json:"disposition,omitempty"`
	Inbox       censusInbox `json:"inbox"`
}

type censusAgent struct {
	Type string `json:"type"` // claude | codex | antigravity | pi
}

// censusLinks is the rig's identity in its trackers: the identifier as a
// plain string when the rig is tracked there, null when it isn't. The nulls
// stay explicit (rather than omitted) because "this slot exists and is
// empty" is exactly what an unticketed rig needs to say.
type censusLinks struct {
	Linear  *string `json:"linear"`
	Vikunja *string `json:"vikunja"`
	GitHub  *string `json:"github"`
	URL     string  `json:"url,omitempty"`
}

type censusRepo struct {
	Name     string   `json:"name"` // owner/repo
	Branches []string `json:"branches,omitempty"`
	// PRs is null in the cheap tier ("not looked") and a possibly-empty list
	// in the full tier; the pointer is what keeps those apart.
	PRs *[]censusPR `json:"prs"`
	// WIP is rigDirtyRepos' verdict: work nothing accounts for yet. In the
	// cheap tier it is computed without PR heads, so pushed-but-unqueried
	// work reads as WIP — conservative on purpose.
	WIP bool `json:"wip"`
	// WIPUncertain marks the "sub?" case: jj couldn't read the workspace, so
	// even the existence of work is a guess (the mir-822 lesson — name a
	// broken state rather than silently reporting clean).
	WIPUncertain bool `json:"wipUncertain,omitempty"`
}

type censusPR struct {
	Number int      `json:"number"`
	State  string   `json:"state"` // open | closed | merged
	URL    string   `json:"url"`
	Branch string   `json:"branch"`
	Review string   `json:"review,omitempty"` // approved | changes_requested | review_required
	CI     censusCI `json:"ci"`
}

type censusCI struct {
	Status      string   `json:"status,omitempty"` // passing | failing | pending; absent for no CI
	FailingJobs []string `json:"failingJobs,omitempty"`
}

type censusInbox struct {
	NotifyCount int    `json:"notifyCount"`
	WorstLevel  string `json:"worstLevel,omitempty"` // info | warn | error
}

func runCensus(args []string) error {
	cheap := false
	refresh := false
	for _, a := range args {
		switch a {
		case "--cheap":
			cheap = true
		case "--refresh":
			refresh = true
		case "--format=json", "--json":
			// Accepted for symmetry with ls, but there's no table behind the
			// flags: census is JSON-only and ls --full is the human table.
		default:
			return fmt.Errorf("usage: rig census [--cheap] [--refresh] [--format=json]")
		}
	}

	rigs, err := listRigs()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	now := time.Now()
	statuses := rigStatuses(rigs, home, now)

	doc := censusDoc{GeneratedAt: now, Tier: "full", Rigs: []censusRig{}}
	if cheap {
		doc.Tier = "cheap"
	} else {
		doc.PRsAsOf = censusEnrichPRs(statuses, refresh, now)
	}

	inbox := activeNotifications()
	doc.LooseNotifications = looseNotifications(inbox)

	for i := range statuses {
		doc.Rigs = append(doc.Rigs, censusRigFor(statuses[i], notificationsForRig(inbox, statuses[i].ID), cheap))
	}

	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(blob))
	return nil
}

// censusEnrichPRs fills every rig's PRs for the full tier, reusing the
// radar's PR cache for answers younger than censusPRTTL and fanning one
// enrichWithPRs pass over whatever is stale or absent. Fresh answers go back
// into the same cache, so a census run warms the radar and vice versa.
// Returns the timestamp of the oldest answer used; fresh answers count as
// now.
func censusEnrichPRs(statuses []rigStatus, refresh bool, now time.Time) *time.Time {
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
		if ok && now.Sub(e.At) < censusPRTTL {
			statuses[i].PRs = e.PRs
			if e.At.Before(oldest) {
				oldest = e.At
			}
			continue
		}
		stale = append(stale, i)
	}
	if len(stale) == 0 {
		return &oldest
	}

	// enrichWithPRs clears and repopulates the slice it's handed, so the
	// stale rigs go in as their own slice and their answers copy back.
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
	return &oldest
}

// censusRigFor folds one rig's status, manifest, and inbox into its census
// row. Every local read beyond ls's own is degrade-quiet, because one rig
// with a broken workspace must not blank the whole board — see the mir-822
// note on rigDirtyRepos.
func censusRigFor(s rigStatus, pinned []notification, cheap bool) censusRig {
	row := censusRig{
		ID:       s.ID,
		Slug:     s.Slug,
		Title:    s.Title,
		Kind:     s.Kind,
		Created:  s.Created,
		Age:      age(s.Created),
		State:    censusState(s),
		Backend:  s.Backend,
		Location: "local", // rigs are local by construction; see surface.go and PERS-17
		Repos:    []censusRepo{},
		Agent:    censusAgent{Type: string(agentClaude)},
	}
	if row.Backend == "" {
		row.Backend = "tmux"
	}

	m, err := readManifest(s.Path)
	if err != nil {
		// A rig whose manifest won't read still lists, with the repos ls
		// already resolved. No links, branches, or WIP — names only.
		for _, nwo := range s.Repos {
			row.Repos = append(row.Repos, censusRepo{Name: nwo})
		}
	} else {
		row.Agent.Type = string(m.agentKind())
		row.Links = censusLinksFor(m)
		subdirs := make([]string, 0, len(m.Repos))
		for sub := range m.Repos {
			subdirs = append(subdirs, sub)
		}
		sort.Strings(subdirs)
		bySlug := make(map[string]int, len(subdirs)) // censusRepo index per owner/repo
		for _, sub := range subdirs {
			repo := censusRepo{Name: m.Repos[sub]}
			if branches, err := repoBranches(m, sub, filepath.Join(s.Path, sub)); err == nil {
				repo.Branches = branches
			}
			bySlug[m.Repos[sub]] = len(row.Repos)
			row.Repos = append(row.Repos, repo)
		}
		for _, d := range rigDirtyRepos(s.Path, m, s.PRs) {
			uncertain := strings.HasSuffix(d, "?")
			sub := strings.TrimSuffix(d, "?")
			if i, ok := bySlug[m.Repos[sub]]; ok {
				row.Repos[i].WIP = true
				row.Repos[i].WIPUncertain = uncertain
			}
		}
	}

	// Cheap tier leaves every repo's PRs nil, which marshals as null: "not
	// looked", never to be read as "no PR".
	if !cheap {
		for i := range row.Repos {
			own := []censusPR{}
			for _, pr := range s.PRs {
				if strings.EqualFold(pr.Repo, row.Repos[i].Name) {
					own = append(own, censusPRFor(pr))
				}
			}
			row.Repos[i].PRs = &own
		}
		row.Disposition = parkedDisposition(s.PRs)
	}

	row.LastActivity = censusLastActivity(s)
	row.Inbox = censusInboxFor(pinned)
	return row
}

// censusState is ls's agent column widened into an enum: parked and
// half-built outrank liveness (a parked rig with a warm transcript is still
// parked), then a live session splits working from idle by the usual
// activity window, and anything left has no session at all.
func censusState(s rigStatus) string {
	switch {
	case s.Parked:
		return "parked"
	case s.Building != "":
		return "building"
	case !s.SessionLive:
		return "stopped"
	case s.Agent == "working":
		return "working"
	default:
		return "idle"
	}
}

// censusLinksFor maps the manifest's tracker triple onto the named link
// slots. A rig tracks at most one thing, so at most one slot is non-null.
func censusLinksFor(m manifest) censusLinks {
	links := censusLinks{URL: m.TrackerURL}
	if m.TrackerID == "" {
		return links
	}
	id := m.TrackerID
	switch m.Tracker {
	case "linear":
		links.Linear = &id
	case "tasks":
		links.Vikunja = &id
	case "github":
		links.GitHub = &id
	}
	return links
}

// censusPRFor lowers a rigPR into the census vocabulary: states and review
// decisions go lowercase, both because the feed is an API (stable tokens,
// not GitHub's display casing) and because the sketch's JSON reads that way.
func censusPRFor(pr rigPR) censusPR {
	return censusPR{
		Number: pr.Number,
		State:  strings.ToLower(pr.State),
		URL:    pr.URL,
		Branch: pr.Branch,
		Review: strings.ToLower(pr.Review),
		CI: censusCI{
			Status:      pr.Checks,
			FailingJobs: pr.FailingChecks,
		},
	}
}

// censusLastActivity is max(agent turn, message log mtime). The message log
// belongs to the messaging milestone and won't exist before it does; stat
// anyway so the field's meaning doesn't drift when the log lands.
func censusLastActivity(s rigStatus) *time.Time {
	latest := s.LastActive
	if info, err := os.Stat(filepath.Join(s.Path, ".rig", "messages.jsonl")); err == nil {
		if t := info.ModTime(); latest == nil || t.After(*latest) {
			latest = &t
		}
	}
	return latest
}

// censusInboxFor condenses a rig's pinned notifications to a count and the
// loudest level, mirroring the badge ls puts on the row. Full bodies stay in
// `rig notify list`, which is where a sweep that cares should go next.
func censusInboxFor(pinned []notification) censusInbox {
	box := censusInbox{NotifyCount: len(pinned)}
	for _, n := range pinned {
		if box.WorstLevel == "" || notifyLevels[n.Level] > notifyLevels[box.WorstLevel] {
			box.WorstLevel = n.Level
		}
	}
	return box
}
