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
// opens a tmux window for it in the rig's session.
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
	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
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

	// Best-effort: give the new repo its own background window in the rig
	// session, laid out like rig up's primary window — repo shell on the left,
	// recto previewing diffs on the right. Backgrounded so adding a repo from a
	// main session doesn't yank focus into it.
	session := tmuxSessionName(basedir)
	if tmuxHasSession(session) {
		if winID, err := tmuxNewWindow(session, repo, repoDest); err == nil {
			_ = tmuxSplitH(winID, repoDest, "recto")
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

	if jsonOut {
		blob, err := encodeRigsJSON(statuses)
		if err != nil {
			return err
		}
		fmt.Println(string(blob))
		return nil
	}

	if len(statuses) == 0 {
		fmt.Fprintln(os.Stderr, "rig: no rigs in flight")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, s := range statuses {
		if full {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.ID, age(s.Created), agentMarker(s), prMarker(s), s.Title)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.ID, age(s.Created), agentMarker(s), s.Title)
		}
	}
	return w.Flush()
}

// rigStatus is the enriched view rig ls renders: a rigInfo plus the live
// signals (tmux session presence, agent attention) that make ls the one place
// to scan everything in flight. Field tags pin the json shape as an API.
type rigStatus struct {
	ID          string     `json:"id"`
	Slug        string     `json:"slug"`
	Title       string     `json:"title"`
	Path        string     `json:"path"`
	Created     time.Time  `json:"created"`
	SessionLive bool       `json:"session_live"`
	Agent       string     `json:"agent"`                 // working | idle | "" (no session)
	LastActive  *time.Time `json:"last_active,omitempty"` // newest claude turn, if any
	PRs         []rigPR    `json:"prs,omitempty"`         // populated only under --full
}

// rigPR is one of a rig's pull requests, tagged with the repo and branch it
// belongs to so a multi-repo rig's PRs stay distinguishable. The prInfo fields
// (number, state, url, checks) flatten into the same json object.
type rigPR struct {
	Repo   string `json:"repo"`   // owner/repo
	Branch string `json:"branch"` // head branch
	prInfo
}

// agentActiveWindow is how recently a claude turn must have landed for the
// agent to read as "working" rather than "idle".
const agentActiveWindow = 3 * time.Minute

// rigStatuses enriches each rig with its live signals. Kept out of listRigs
// so cd and reap don't pay for tmux/claude probes they don't use.
func rigStatuses(rigs []rigInfo, home string, now time.Time) []rigStatus {
	out := make([]rigStatus, 0, len(rigs))
	for _, r := range rigs {
		s := rigStatus{
			ID:          r.ID,
			Slug:        r.Slug,
			Title:       r.Title,
			Path:        r.Path,
			Created:     r.Created,
			SessionLive: tmuxHasSession(tmuxSessionName(r.Path)),
		}
		if ts := claudeSessionActivity(home, r.Path); ts > 0 {
			t := time.Unix(ts, 0)
			s.LastActive = &t
		}
		s.Agent = agentState(s.LastActive, now)
		out = append(out, s)
	}
	return out
}

// agentState buckets agent attention from the newest claude turn. We can only
// honestly read recency from session-file mtimes (a turn appends, repaint
// doesn't), so this is working-vs-idle, not the working/waiting/idle split the
// issue sketched — telling "waiting on input" from "quiet" needs a richer
// signal than a timestamp. Returns "" when no claude session exists at all.
func agentState(lastActive *time.Time, now time.Time) string {
	if lastActive == nil {
		return ""
	}
	if now.Sub(*lastActive) < agentActiveWindow {
		return "working"
	}
	return "idle"
}

// agentMarker renders the agent column for the table, blank-padded to a dash
// when there's no agent so the column stays scannable.
func agentMarker(s rigStatus) string {
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
			branch, err := repoBranch(m, sub, filepath.Join(statuses[i].Path, sub))
			if err != nil || branch == "" {
				continue
			}
			tasks = append(tasks, task{i, m.Repos[sub], branch})
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

// runCd jumps to a rig by attaching (or switching to) its tmux session. With
// no arg it fzf-picks; with an arg it filters by id/slug/title substring and
// only falls back to the picker when the filter is ambiguous.
func runCd(args []string) error {
	rigs, err := listRigs()
	if err != nil {
		return err
	}
	if len(rigs) == 0 {
		return fmt.Errorf("no rigs in flight")
	}

	var chosen *rigInfo
	if len(args) >= 1 {
		q := strings.ToLower(strings.Join(args, " "))
		var matches []rigInfo
		for _, r := range rigs {
			hay := strings.ToLower(r.ID + " " + r.Slug + " " + r.Title)
			if strings.Contains(hay, q) {
				matches = append(matches, r)
			}
		}
		switch len(matches) {
		case 0:
			return fmt.Errorf("no rig matches %q", q)
		case 1:
			chosen = &matches[0]
		default:
			rigs = matches // narrow the picker to the matches
		}
	}

	if chosen == nil {
		rows := make([]string, len(rigs))
		for i, r := range rigs {
			rows[i] = fmt.Sprintf("%s\t%s\t%s", r.ID, r.Title, r.Slug)
		}
		sel, err := fzfSelect(rows, "cd to rig: ")
		if err != nil {
			return err
		}
		if sel == "" {
			return nil
		}
		id, _, _ := strings.Cut(sel, "\t")
		for i := range rigs {
			if rigs[i].ID == id {
				chosen = &rigs[i]
				break
			}
		}
	}
	if chosen == nil {
		return nil
	}

	session := tmuxSessionName(chosen.Path)
	if !tmuxHasSession(session) {
		// Rig dir is present but its session was killed; stand up a bare one.
		if err := tmuxNewSession(session, chosen.Path); err != nil {
			return fmt.Errorf("tmux new-session: %w", err)
		}
	}
	return attachOrReport(session)
}

type rigInfo struct {
	ID      string
	Slug    string // basedir directory name
	Title   string
	Path    string // absolute basedir path
	Created time.Time
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
		fi, err := os.Stat(filepath.Join(base, manifestName))
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
		rigs = append(rigs, rigInfo{ID: m.ID, Slug: e.Name(), Title: m.Title, Path: base, Created: created})
	}
	sort.Slice(rigs, func(i, j int) bool { return rigs[i].Created.Before(rigs[j].Created) })
	return rigs, nil
}
