package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// runAdd brings another repo into the rig you're currently in (cwd-derived).
// It clones the repo if needed, colocates jj, drops a workspace at trunk(), and
// opens a persistent, full-window Recto for it in the rig's session.
func runAdd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: rig add <owner/repo>")
	}
	owner, repo, ok := strings.Cut(args[0], "/")
	if !ok || owner == "" || repo == "" {
		return fmt.Errorf("expected owner/repo, got %q", args[0])
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	basedir, err := findBasedir(cwd)
	if err != nil {
		return err
	}
	lock, err := acquireRigMutationLock(basedir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	if m.isProject() {
		return fmt.Errorf("project rigs do not own repository workspaces")
	}

	repoPath, err := ensureGhqClone(owner, repo)
	if err != nil {
		return err
	}
	if err := ensureJJColocated(repoPath); err != nil {
		return fmt.Errorf("colocating jj on %s: %w", repoPath, err)
	}

	ref := repoRef{Owner: owner, Name: repo, Path: repoPath}
	// No branch hint for an added repo — start it on trunk() and leave the
	// branch unrecorded, so pr/ls fall back to the bookmark heuristic once the
	// user creates one.
	repoDest, err := addRepoWorkspace(basedir, m.ID, ref, "trunk()", "")
	if err != nil {
		return err
	}

	// Best-effort: give the new repo a persistent Recto in its own background
	// window. `rig recto <repo>` pulls that pane beside the main agent; until
	// then the window is also a useful full-screen diff. A shell is deliberately
	// absent: tmux's normal split bindings can grow one from the Recto's repo cwd
	// for the occasional poke without making empty shells permanent furniture.
	rs := sessionFor(basedir, m)
	if rs.live() {
		if pane, window, err := rs.b.NewCommandWindow(rs.name, repo, repoDest, rectoCommand()); err == nil {
			_ = rs.markPane(pane, rigPaneRecto, repo)
			_ = rs.markRepoWindow(window, repo)
		}
	}

	fmt.Fprintf(os.Stderr, "rig: added %s → %s\n", ref.nameWithOwner(), repoDest)
	return nil
}

// runLs lists the rigs currently in flight under ~/workspaces. The default
// table is the human call-sheet; --format=json exposes the same board as a
// stable document API for tmux statuslines, FleetView-style boards, and the
// chief-of-staff skill. Adding --full turns on the PR tier: it fetches PR/CI
// state (a gh round-trip per repo, cached like the radar) and computes each
// rig's disposition. Without it the read is local and instant.
func runLs(args []string) error {
	jsonOut := false
	full := false
	refresh := false
	for _, a := range args {
		switch a {
		case "--format=json":
			jsonOut = true
		case "--format=table", "":
			jsonOut = false
		case "--full":
			full = true
		case "--refresh":
			refresh = true
		default:
			return fmt.Errorf("usage: rig ls [--format=json|table] [--full] [--refresh]")
		}
	}
	if refresh && !full {
		return fmt.Errorf("rig ls: --refresh only applies to --full (the cheap tier does no PR fetch)")
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

	doc := boardDoc{GeneratedAt: now, Tier: "cheap"}
	if full {
		doc.Tier = "full"
		doc.PRsAsOf = enrichBoardPRs(statuses, refresh, now)
	} else {
		enrichBoardLocal(statuses)
	}

	inbox := activeNotifications()
	doc.LooseNotifications = looseNotifications(inbox)
	for i := range statuses {
		statuses[i].Notifications = notificationsForRig(inbox, statuses[i].ID)
		statuses[i].Inbox = boardInboxFor(statuses[i].Notifications)
	}
	doc.Rigs = rigsForJSON(statuses, full)

	if jsonOut {
		blob, err := encodeRigsJSON(doc)
		if err != nil {
			return err
		}
		fmt.Println(string(blob))
		return nil
	}

	// Loose inbox entries belong to no rig, so they can't ride a row. They go
	// above the table on purpose: a stalled background job is exactly the thing
	// you'd never scroll down to find.
	for _, line := range notifyBanner(doc.LooseNotifications) {
		fmt.Fprintln(os.Stderr, line)
	}

	if len(statuses) == 0 {
		fmt.Fprintln(os.Stderr, "rig: no rigs in flight")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, s := range statuses {
		title := s.Title
		if mark := rigNotifyMark(s); mark != "" {
			title = mark + " " + title
		}
		// Kind leads the row rather than riding the id, because the id here is
		// the handle you copy into `rig switch` and a glyph glued to its front
		// gets caught in the selection. On the left it also gives the table a
		// hard edge to scan down, which is what a kind marker is for. The cell
		// is the bare glyph: tabwriter owns the padding, and a loose rig's empty
		// cell collapses the column to nothing when no row on the board has a
		// kind to show.
		kind := rigKindGlyph(rigKindOf(s))
		if full {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", kind, s.ID, age(s.Created), agentMarker(s), prMarker(s), title)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", kind, s.ID, age(s.Created), agentMarker(s), title)
		}
	}
	return w.Flush()
}

// rigNotifyMark is the badge a rig-pinned notification puts on that rig's row,
// at the loudest level pinned to it.
func rigNotifyMark(s rigStatus) string {
	worst := ""
	for _, n := range s.Notifications {
		if worst == "" || notifyLevels[n.Level] > notifyLevels[worst] {
			worst = n.Level
		}
	}
	if worst == "" {
		return ""
	}
	return notifyLevelMark(worst)
}

// rigStatus is the board row: a rigInfo plus the live signals (tmux session
// presence, agent attention) and the derived board facts (disposition, WIP,
// links) that make `rig ls` the one place to read everything in flight. It is
// both the table's row and the JSON document's row, so a board fact lands in
// exactly one struct. Field tags pin the json shape as an API.
type rigStatus struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
	Kind  string `json:"kind,omitempty"` // review | project; absent means authoring
	Path  string `json:"path"`
	// Created is when the rig was made; Age is age(Created) as a string, a
	// convenience twin for a reader that would rather not do the arithmetic.
	Created time.Time `json:"created"`
	Age     string    `json:"age"`
	// LastTouched is the durable MRU stamp (falls back to Created for legacy
	// rigs); the radar's recency ordering and ls read it.
	LastTouched time.Time `json:"last_touched"`
	// State is ls's agent column widened into an enum: parked and half-built
	// outrank liveness (a parked rig with a warm transcript is still parked),
	// then a live session splits working from idle, and anything left has no
	// session at all. parked | building | stopped | working | idle.
	State string `json:"state"`
	// Location is where the rig lives. Rigs are local by construction; see
	// surface.go and PERS-17. Present so a remote rig has somewhere to land.
	Location string `json:"location"`
	// Repos is the local repo facts. It is tagged out of the wire shape and
	// re-added as `repos` on rigRowJSON, which nests each repo's PRs; see
	// rigsForJSON. Keeping storage and wire separate here means one PR list.
	Repos []rigRepo `json:"-"`
	Links rigLinks  `json:"links"`
	Inbox rigInbox  `json:"inbox"`

	// SessionLive is whether the rig's multiplexer session exists at all, and
	// Backend names the multiplexer hosting it (empty means tmux). AgentType is
	// the manifest's agent (claude | codex | antigravity | pi). Agent is the
	// attention bucket — working | idle, or "" with no session — and Parked is
	// dormant-awaiting-review, which outranks attention the way State says.
	SessionLive bool       `json:"session_live"`
	Backend     string     `json:"backend,omitempty"`
	AgentType   string     `json:"agent_type,omitempty"`
	Agent       string     `json:"agent"`
	Parked      bool       `json:"parked"`
	LastActive  *time.Time `json:"last_active,omitempty"`

	// Disposition is parkedDisposition's vocabulary ("waiting", "approved",
	// ...) computed over every rig, not just parked ones: it is the "what does
	// this rig want" answer sweep and waiting share. Full tier only — it needs
	// PR state — so a cheap read leaves it empty.
	Disposition string `json:"disposition,omitempty"`
	// PRs is every PR across the rig's repos, flat, each tagged with the repo
	// and branch it belongs to. It is the one PR list (the radar, sweep and
	// waiting all read it); the board's JSON nests it per repo on the way out.
	// Populated only under --full, so a cheap read leaves it nil.
	PRs []rigPR `json:"-"`
	// LastActivity is max(agent turn, message-log mtime), the newest sign of
	// life the board can point at.
	LastActivity *time.Time `json:"last_activity,omitempty"`
	// Building is the manifest's BuildingRepo: "owner/repo" when this rig's
	// create was interrupted before its workspace existed. A half-built rig
	// can be finished but not entered.
	Building string `json:"building,omitempty"`

	// Notifications are the inbox entries pinned to this rig. Loose entries
	// (the common case) belong to no rig and never appear here.
	Notifications []notification `json:"notifications,omitempty"`

	// Tracker is the manifest's tracker triple, carried flat so a consumer
	// that wants the raw fields (not the named Links slots) has them.
	Tracker    string `json:"tracker,omitempty"`
	TrackerID  string `json:"tracker_id,omitempty"`
	TrackerURL string `json:"tracker_url,omitempty"`

	// radar-only, never serialized: a row that's a bare tmux session (not a
	// rig) carries bare=true and the session to attach to. A child row dangled
	// under a parent carries child=true, session set to the agent pane's switch
	// target on its backend, and childKey holding the window label. agents
	// are the parent's agent windows, expanded into child rows at render.
	bare     bool
	child    bool
	session  rigSession
	childKey string
	agents   []agentChild
	// activity is the agent title of a row's lone agent, carried verbatim.
	// Only the cursor row draws it; every other row is pure identity, so this
	// never competes with the subject for width.
	activity string
	// stone marks a row that isn't a rig at all any more: it's a tombstone from
	// the history section, and Enter on it resurrects rather than switches.
	// Nil on every live row. Rows carrying one have no Path worth locking, no
	// session to attach, and no PRs to fetch, so the pipelines that do those
	// things skip them.
	stone *tombstone
	// peers is every iso peer container under the rig's repos, from the
	// radar's own peers pass (radarPeersCmd). The board's JSON carries the
	// same facts per repo instead, on rigRepo.
	peers []isoPeer
}

// rigLinks is the rig's identity in its trackers: the identifier as a plain
// string when the rig is tracked there, null when it isn't. The nulls stay
// explicit (rather than omitted) because "this slot exists and is empty" is
// exactly what an unticketed rig needs to say.
type rigLinks struct {
	Linear  *string `json:"linear"`
	Vikunja *string `json:"vikunja"`
	GitHub  *string `json:"github"`
	URL     string  `json:"url,omitempty"`
}

// rigRepo is one repository in a rig, with the local facts the board carries:
// the branches the workspace is on and whether it holds unaccounted work. The
// repo's PRs are not stored here — they live on the row's flat PRs list and are
// nested in at JSON time (see rigsForJSON), so there is one PR list to fill.
// Repos are ordered by subdir so the board reads the same way twice running.
type rigRepo struct {
	Name     string   `json:"name"` // owner/repo
	Branches []string `json:"branches,omitempty"`
	// PRs is the repo's PRs, filled only when the row is marshaled (null in the
	// cheap tier — "not looked" — and a possibly-empty list in the full tier).
	PRs *[]rigPR `json:"prs,omitempty"`
	// WIP is rigDirtyRepos' verdict: work nothing accounts for yet. In the
	// cheap tier it is computed without PR heads, so pushed-but-unqueried work
	// reads as WIP — conservative on purpose.
	WIP bool `json:"wip"`
	// WIPUncertain marks the "sub?" case: jj couldn't read the workspace, so
	// even the existence of work is a guess (the mir-822 lesson — name a broken
	// state rather than silently reporting clean).
	WIPUncertain bool `json:"wipUncertain,omitempty"`
	// Peers is the repo's iso peer containers (`iso peers up`), running or
	// not, read from iso's docker labels. Absent when there are none or
	// docker couldn't be asked; see isopeers.go.
	Peers []isoPeer `json:"peers,omitempty"`
}

// rigInbox condenses a rig's pinned notifications to a count and the loudest
// level, mirroring the badge ls puts on the row. Full bodies stay in
// `rig notify list`, which is where a sweep that cares should go next.
type rigInbox struct {
	NotifyCount int    `json:"notifyCount"`
	WorstLevel  string `json:"worstLevel,omitempty"` // info | warn | error
}

// rigPR is one of a rig's pull requests, tagged with the repo and branch it
// belongs to so a multi-repo rig's PRs stay distinguishable. The prInfo fields
// (number, state, url, checks) flatten into the same json object.
type rigPR struct {
	Repo   string `json:"repo"`   // owner/repo
	Branch string `json:"branch"` // head branch
	prInfo
}

// agentActiveWindow is how recently an agent turn must have landed for the
// agent to read as "working" rather than "idle".
const agentActiveWindow = 3 * time.Minute

// rigStatuses enriches each rig with its live signals. Kept out of listRigs
// so cd and reap don't pay for tmux/agent probes they don't use. Repos come
// back names-only (the manifest's slugs); the board layers branches, WIP and
// PRs on via enrichBoardLocal/enrichBoardPRs, which is where the jj and gh
// costs live.
func rigStatuses(rigs []rigInfo, home string, now time.Time) []rigStatus {
	out := make([]rigStatus, 0, len(rigs))
	paths := make([]string, len(rigs))
	for i := range rigs {
		paths[i] = rigs[i].Path
	}
	activity := agentSessionActivities(home, paths)
	for _, r := range rigs {
		repos := make([]rigRepo, 0, len(r.Repos))
		for _, nwo := range r.Repos {
			repos = append(repos, rigRepo{Name: nwo})
		}
		s := rigStatus{
			ID:          r.ID,
			Slug:        r.Slug,
			Title:       r.Title,
			Kind:        r.Kind,
			Tracker:     r.Tracker,
			TrackerID:   r.TrackerID,
			TrackerURL:  r.TrackerURL,
			Path:        r.Path,
			Created:     r.Created,
			Age:         age(r.Created),
			LastTouched: r.LastTouched,
			Parked:      !r.Parked.IsZero(),
			Repos:       repos,
			Building:    r.Building,
			Backend:     r.Backend,
			Location:    "local", // rigs are local by construction; see surface.go and PERS-17
			SessionLive: r.session().live(),
		}
		if ts := activity[r.Path]; ts > 0 {
			t := time.Unix(ts, 0)
			s.LastActive = &t
		}
		s.Agent = agentState(s.LastActive, now)
		s.State = boardState(s)
		out = append(out, s)
	}
	return out
}

// boardState is ls's agent column widened into an enum: parked and half-built
// outrank liveness (a parked rig with a warm transcript is still parked), then
// a live session splits working from idle, and anything left has no session at
// all. Sweep and waiting share this vocabulary so they never disagree.
func boardState(s rigStatus) string {
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

// agentState buckets agent attention from the newest agent turn. We can only
// honestly read recency from session-file mtimes (a turn appends, repaint
// doesn't), so this is working-vs-idle, not the working/waiting/idle split the
// issue sketched — telling "waiting on input" from "quiet" needs a richer
// signal than a timestamp. Returns "" when no agent session exists at all.
func agentState(lastActive *time.Time, now time.Time) string {
	if lastActive == nil {
		return ""
	}
	if now.Sub(*lastActive) < agentActiveWindow {
		return "working"
	}
	return "idle"
}

// agentMarker renders the agent column for the table. A parked rig reads
// "parked" (it's deliberately dormant, session killed); otherwise it's the live
// agent state, or a dash when there's no session so the column stays scannable.
func agentMarker(s rigStatus) string {
	// A half-built rig has no session to have a state, and reading as an
	// ordinary idle rig is the lie that sent you looking for a working `switch`.
	if s.Building != "" {
		return "half"
	}
	if s.Parked {
		return "parked"
	}
	if s.Agent == "" {
		return "-"
	}
	return s.Agent
}

// prMarker renders the PR column for the --full table. A single-repo rig reads
// "#7 OPEN/passing"; a rig spanning repos prefixes each with its short repo
// name ("infra #80 OPEN  runtime #42 OPEN/failing") so the PRs don't blur
// together. A dash keeps the column scannable for rigs with no PR.
func prMarker(s rigStatus) string {
	if len(s.PRs) == 0 {
		return "-"
	}
	multi := len(s.PRs) > 1
	segs := make([]string, len(s.PRs))
	for i, pr := range s.PRs {
		seg := fmt.Sprintf("#%d %s", pr.Number, pr.State)
		if pr.Checks != "" {
			seg += "/" + pr.Checks
		}
		if multi {
			seg = shortRepo(pr.Repo) + " " + seg
		}
		segs[i] = seg
	}
	return strings.Join(segs, "  ")
}

// shortRepo trims "owner/repo" to just "repo" for compact table display.
func shortRepo(nameWithOwner string) string {
	if _, name, ok := strings.Cut(nameWithOwner, "/"); ok {
		return name
	}
	return nameWithOwner
}

// enrichWithPRs fills in each rig's PRs for `rig ls --full`. Every rig's per-
// repo branch is resolved locally first (manifest-recorded, or the bookmark
// heuristic), then one gh call per rig-repo fetches that exact branch's PR,
// fanned out concurrently. Cost scales with repos-in-flight, and each call is a
// single PR's rollup rather than a repo-wide list. A rig can carry several PRs
// (one per repo it touches). gh failures and branchless repos degrade to a
// blank column rather than failing the whole listing.
func enrichWithPRs(statuses []rigStatus) {
	// Populate from scratch, never augment. The radar merges each rig's cached
	// PRs back into its status before asking for a refresh, so appending onto
	// whatever's already there would re-add the whole set every refetch — the
	// cache grew to dozens of copies of the same PR. Clearing up front keeps the
	// call idempotent for any caller.
	for i := range statuses {
		statuses[i].PRs = nil
	}

	type task struct {
		rig    int
		repo   string // owner/repo
		branch string
	}
	var tasks []task
	for i := range statuses {
		m, err := readManifest(statuses[i].Path)
		if err != nil {
			continue
		}
		subdirs := make([]string, 0, len(m.Repos))
		for sub := range m.Repos {
			subdirs = append(subdirs, sub)
		}
		sort.Strings(subdirs)
		for _, sub := range subdirs {
			branches, err := repoBranches(m, sub, filepath.Join(statuses[i].Path, sub))
			if err != nil {
				continue
			}
			for _, branch := range branches {
				if branch == "" {
					continue
				}
				tasks = append(tasks, task{i, m.Repos[sub], branch})
			}
		}
	}

	results := make([]*prInfo, len(tasks))
	sem := make(chan struct{}, 8) // cap concurrent gh calls
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, t task) {
			defer wg.Done()
			defer func() { <-sem }()
			if pr, err := prForBranch(t.repo, t.branch); err == nil {
				results[i] = pr
			}
		}(i, t)
	}
	wg.Wait()

	// Tasks were built in rig-then-sorted-subdir order, so appending in order
	// keeps each rig's PRs stable and grouped.
	for i, t := range tasks {
		if results[i] != nil {
			statuses[t.rig].PRs = append(statuses[t.rig].PRs, rigPR{
				Repo: t.repo, Branch: t.branch, prInfo: *results[i],
			})
		}
	}
}

// boardDoc is the ls json document: the board plus the provenance a program
// consumer needs to judge it. It replaced census's document when the two
// readers merged. `rig ls --format=json` emits the cheap tier (no gh); adding
// --full fills PR state and disposition and timestamps the oldest answer.
type boardDoc struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Tier        string    `json:"tier"` // full | cheap
	// PRsAsOf is the oldest PR answer in the document, full tier only, so the
	// reader can judge how stale the most-cached row is allowed to be.
	PRsAsOf *time.Time `json:"prsAsOf,omitempty"`
	// LooseNotifications are the inbox entries pinned to no rig — what ls and
	// the radar draw as a banner. They're top-level because a stalled cron
	// bears on the whole board, not on one row.
	LooseNotifications []notification `json:"looseNotifications,omitempty"`
	Rigs               []rigRowJSON   `json:"rigs"`
}

// rigRowJSON is a rigStatus as it appears on the wire: identical except its
// repos carry their PRs nested, which is the shape a board consumer wants. The
// stored row keeps one flat PR list (see rigStatus.PRs); nesting happens here
// at the boundary so there is no second copy to keep in sync.
type rigRowJSON struct {
	rigStatus
	Repos []rigRepoJSON `json:"repos"`
}

// rigRepoJSON is a rigRepo on the wire: its own PRs, sliced out of the row's
// flat list. PRs is null in the cheap tier ("not looked") and a possibly-empty
// list in the full tier; the pointer is what keeps those apart.
type rigRepoJSON struct {
	Name         string    `json:"name"`
	Branches     []string  `json:"branches,omitempty"`
	PRs          *[]rigPR  `json:"prs"`
	WIP          bool      `json:"wip"`
	WIPUncertain bool      `json:"wipUncertain,omitempty"`
	Peers        []isoPeer `json:"peers,omitempty"`
}

// encodeRigsJSON marshals the board as the ls json API: a document, never a
// bare array, so consumers get the provenance alongside the rows. Rigs is
// always [] (never null) so consumers can iterate unconditionally. The full
// tier is signaled by a non-nil PRsAsOf: when it's set, every repo's PR list is
// materialized (empty when the repo has none), and when it's nil PRs stay null.
func encodeRigsJSON(doc boardDoc) ([]byte, error) {
	if doc.Rigs == nil {
		doc.Rigs = []rigRowJSON{}
	}
	return json.MarshalIndent(doc, "", "  ")
}

// rigsForJSON lowers the board's rows to their wire shape, nesting each row's
// flat PRs into the repo they belong to. looked reports whether PRs were
// fetched at all (the full tier); when false every repo's PRs marshal as null.
func rigsForJSON(rows []rigStatus, looked bool) []rigRowJSON {
	out := make([]rigRowJSON, len(rows))
	for i, s := range rows {
		row := rigRowJSON{rigStatus: s, Repos: make([]rigRepoJSON, len(s.Repos))}
		byName := map[string]int{}
		for j, repo := range s.Repos {
			byName[repo.Name] = j
			row.Repos[j] = rigRepoJSON{
				Name:         repo.Name,
				Branches:     repo.Branches,
				WIP:          repo.WIP,
				WIPUncertain: repo.WIPUncertain,
				Peers:        repo.Peers,
			}
			if looked {
				empty := []rigPR{}
				row.Repos[j].PRs = &empty
			}
		}
		if looked {
			for _, pr := range s.PRs {
				if j, ok := byName[pr.Repo]; ok {
					*row.Repos[j].PRs = append(*row.Repos[j].PRs, pr)
				}
			}
		}
		out[i] = row
	}
	return out
}

// age renders a compact relative age for ls output.
func age(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

type rigInfo struct {
	ID          string
	Slug        string // basedir directory name
	Title       string
	Kind        string
	Tracker     string
	TrackerID   string
	TrackerURL  string
	Path        string // absolute basedir path
	Created     time.Time
	LastTouched time.Time // durable MRU stamp; falls back to Created for legacy rigs
	Parked      time.Time // non-zero once `rig park` marked it dormant
	Repos       []string  // "owner/repo" per repo in the rig, subdir-sorted
	// Building is the manifest's BuildingRepo: non-empty means this rig's
	// create was interrupted before its workspace existed, so it can be
	// finished but not entered.
	Building string
	// Backend is the manifest's, so a board can find the rig's session without
	// re-reading the manifest; empty means tmux.
	Backend string
}

// session is the rig's session on the backend its manifest recorded.
func (r rigInfo) session() rigSession {
	return rigSession{name: rigSessionName(r.Path), b: backendNamed(r.Backend)}
}

// manifestRepos flattens a manifest's repo table to its "owner/repo" slugs,
// ordered by subdir so a multi-repo rig reads the same way twice running. The
// subdir keys are dropped: they're the repo name already in every rig we've
// ever written, so carrying both would only double the haystack.
func manifestRepos(m manifest) []string {
	if len(m.Repos) == 0 {
		return nil
	}
	dirs := make([]string, 0, len(m.Repos))
	for dir := range m.Repos {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	repos := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		repos = append(repos, m.Repos[dir])
	}
	return repos
}

// listRigs scans ~/workspaces for directories carrying a rig manifest.
func listRigs() ([]rigInfo, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, "workspaces")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var rigs []rigInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		base := filepath.Join(root, e.Name())
		manifestPath, err := findManifestPath(base)
		if err != nil {
			continue
		}
		fi, err := os.Stat(manifestPath)
		if err != nil {
			continue
		}
		m, err := readManifest(base)
		if err != nil {
			continue
		}
		// Rigs created before the manifest grew a created field fall back to
		// the manifest's mtime (close enough: rewritten only by `rig add`).
		created := m.Created
		if created.IsZero() {
			created = fi.ModTime()
		}
		touched := m.Touched
		if touched.IsZero() {
			touched = created
		}
		rigs = append(rigs, rigInfo{
			ID: m.ID, Slug: e.Name(), Title: m.Title, Kind: m.Kind,
			Tracker: m.Tracker, TrackerID: m.TrackerID, TrackerURL: m.TrackerURL,
			Path: base, Created: created, LastTouched: touched, Parked: m.Parked,
			Repos: manifestRepos(m), Building: m.BuildingRepo, Backend: m.Backend,
		})
	}
	sort.Slice(rigs, func(i, j int) bool { return rigs[i].Created.Before(rigs[j].Created) })
	return rigs, nil
}
