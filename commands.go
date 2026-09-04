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
	session := tmuxSessionName(basedir)
	if tmuxHasSession(session) {
		if pane, window, err := tmuxNewCommandWindow(session, repo, repoDest, rectoCommand()); err == nil {
			_ = markRigPane(pane, rigPaneRecto, repo)
			_ = markRigRepoWindow(window, repo)
		}
	}

	fmt.Fprintf(os.Stderr, "rig: added %s → %s\n", ref.nameWithOwner(), repoDest)
	return nil
}

// runLs lists the rigs currently in flight under ~/workspaces. The default
// table is the human call-sheet; --format=json exposes the same rows as a
// stable API for tmux statuslines, FleetView-style boards, and scripts that
// would otherwise have to scrape columns.
func runLs(args []string) error {
	jsonOut := false
	full := false
	for _, a := range args {
		switch a {
		case "--format=json":
			jsonOut = true
		case "--format=table":
			jsonOut = false
		case "--full":
			full = true
		default:
			return fmt.Errorf("usage: rig ls [--format=json|table] [--full]")
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
	statuses := rigStatuses(rigs, home, time.Now())
	// PR/CI is opt-in: it costs a gh round-trip per repo, so plain ls (the
	// common case, and what statuslines poll) stays local and instant.
	if full {
		enrichWithPRs(statuses)
	}

	inbox := activeNotifications()
	for i := range statuses {
		statuses[i].Notifications = notificationsForRig(inbox, statuses[i].ID)
	}

	if jsonOut {
		blob, err := encodeRigsJSON(statuses)
		if err != nil {
			return err
		}
		fmt.Println(string(blob))
		return nil
	}

	// Loose inbox entries belong to no rig, so they can't ride a row. They go
	// above the table on purpose: a stalled background job is exactly the thing
	// you'd never scroll down to find.
	for _, line := range notifyBanner(looseNotifications(inbox)) {
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

// rigStatus is the enriched view rig ls renders: a rigInfo plus the live
// signals (tmux session presence, agent attention) that make ls the one place
// to scan everything in flight. Field tags pin the json shape as an API.
type rigStatus struct {
	ID          string     `json:"id"`
	Slug        string     `json:"slug"`
	Title       string     `json:"title"`
	Kind        string     `json:"kind,omitempty"`
	Tracker     string     `json:"tracker,omitempty"`
	TrackerID   string     `json:"tracker_id,omitempty"`
	TrackerURL  string     `json:"tracker_url,omitempty"`
	Path        string     `json:"path"`
	Created     time.Time  `json:"created"`
	LastTouched time.Time  `json:"last_touched"`
	SessionLive bool       `json:"session_live"`
	Agent       string     `json:"agent"`                 // working | idle | "" (no session)
	Parked      bool       `json:"parked"`                // dormant, awaiting review
	LastActive  *time.Time `json:"last_active,omitempty"` // newest agent turn, if any
	Repos       []string   `json:"repos,omitempty"`       // "owner/repo" per repo in the rig
	PRs         []rigPR    `json:"prs,omitempty"`         // populated only under --full

	// Notifications are the inbox entries pinned to this rig. Loose entries
	// (the common case) belong to no rig and never appear here.
	Notifications []notification `json:"notifications,omitempty"`

	// radar-only, never serialized: a row that's a bare tmux session (not a
	// rig) carries bare=true and the raw session name to attach to. A child row
	// dangled under a parent carries child=true, session set to the window's
	// session:index switch target, and childKey holding the window label. agents
	// are the parent's agent windows, expanded into child rows at render.
	bare     bool
	child    bool
	session  string
	childKey string
	agents   []agentChild
	// stone marks a row that isn't a rig at all any more: it's a tombstone from
	// the history section, and Enter on it resurrects rather than switches.
	// Nil on every live row. Rows carrying one have no Path worth locking, no
	// session to attach, and no PRs to fetch, so the pipelines that do those
	// things skip them.
	stone *tombstone
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
// so cd and reap don't pay for tmux/agent probes they don't use.
func rigStatuses(rigs []rigInfo, home string, now time.Time) []rigStatus {
	out := make([]rigStatus, 0, len(rigs))
	paths := make([]string, len(rigs))
	for i := range rigs {
		paths[i] = rigs[i].Path
	}
	activity := agentSessionActivities(home, paths)
	for _, r := range rigs {
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
			LastTouched: r.LastTouched,
			Parked:      !r.Parked.IsZero(),
			Repos:       r.Repos,
			SessionLive: tmuxHasSession(tmuxSessionName(r.Path)),
		}
		if ts := activity[r.Path]; ts > 0 {
			t := time.Unix(ts, 0)
			s.LastActive = &t
		}
		s.Agent = agentState(s.LastActive, now)
		out = append(out, s)
	}
	return out
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

// encodeRigsJSON marshals the status rows as the ls json API. Always an array
// (never null) so consumers can iterate unconditionally.
func encodeRigsJSON(statuses []rigStatus) ([]byte, error) {
	if statuses == nil {
		statuses = []rigStatus{}
	}
	return json.MarshalIndent(statuses, "", "  ")
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
			Repos: manifestRepos(m),
		})
	}
	sort.Slice(rigs, func(i, j int) bool { return rigs[i].Created.Before(rigs[j].Created) })
	return rigs, nil
}
