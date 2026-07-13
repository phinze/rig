package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// runReap implements `rig reap`: walk the rigs in flight and break down the
// ones whose work is merged, whose workspaces hold no WIP, and whose tmux
// sessions have gone idle. This is the rig-shaped replacement for the
// nightly dev-session-cleanup's workspace phase: every rig has a manifest
// and one teardown code path, so cleanup is enumeration plus policy instead
// of path archaeology. Fail-closed throughout — a jj error keeps the rig,
// never guesses.
func runReap(args []string) error {
	dryRun := false
	maxIdle := 24 * time.Hour
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run", "-n":
			dryRun = true
		case "--max-idle":
			i++
			if i >= len(args) {
				return fmt.Errorf("--max-idle needs a value (seconds)")
			}
			secs, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("--max-idle: %w", err)
			}
			maxIdle = time.Duration(secs) * time.Second
		default:
			return fmt.Errorf("usage: rig reap [--dry-run|-n] [--max-idle SECONDS]")
		}
	}

	rigs, err := listRigs()
	if err != nil {
		return err
	}
	if len(rigs) == 0 {
		fmt.Fprintln(os.Stderr, "rig: no rigs in flight")
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	now := time.Now()
	fetched := map[string]bool{} // source repo path → already fetched this run
	paths := make([]string, len(rigs))
	for i := range rigs {
		paths[i] = rigs[i].Path
	}
	activity := agentSessionActivities(home, paths)
	reaped := 0
	for _, r := range rigs {
		if reason := reapBlocker(r, activity[r.Path], now, maxIdle, fetched); reason != "" {
			fmt.Fprintf(os.Stderr, "rig: keep %s — %s\n", r.ID, reason)
			continue
		}
		if dryRun {
			fmt.Fprintf(os.Stderr, "rig: would reap %s — %s\n", r.ID, r.Path)
			reaped++
			continue
		}
		m, err := readManifest(r.Path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rig: keep %s — reading manifest: %v\n", r.ID, err)
			continue
		}
		if err := teardownRig(r.Path, m); err != nil {
			fmt.Fprintf(os.Stderr, "rig: reap %s failed: %v\n", r.ID, err)
			continue
		}
		if err := tmuxKillSession(tmuxSessionName(r.Path)); err != nil {
			fmt.Fprintf(os.Stderr, "rig: warning: tmux kill-session: %v\n", err)
		}
		fmt.Fprintf(os.Stderr, "rig: reaped %s — %s gone\n", r.ID, r.Path)
		reaped++
	}
	verb := "reaped"
	if dryRun {
		verb = "would reap"
	}
	fmt.Fprintf(os.Stderr, "rig: reap complete — %s %d of %d rigs\n", verb, reaped, len(rigs))
	return nil
}

// reapBlocker decides whether a rig is safe to reap, returning the first
// reason it isn't ("" means reapable).
func reapBlocker(r rigInfo, activity int64, now time.Time, maxIdle time.Duration, fetched map[string]bool) string {
	// Attention gate first: recent attention means the rig is mid-thought
	// regardless of merge state. Two signals, both persistent and neither
	// resettable by accident: agent session activity (a turn appends
	// whether human-driven or autonomous; repaint doesn't) and the rig's
	// own age (a rig younger than the idle window can't be idle). File
	// changes are deliberately the VCS gates' job below — jj sees any
	// non-ignored modification as WIP; losing gitignored scratch is the
	// accepted cost of not mtime-crawling every workspace nightly. tmux
	// signals all failed here: output-based ones are pinned by claude's
	// at-rest TUI repaint, and attach-based ones reset on a mere peek, so
	// checking whether a rig was dead would keep it alive another day.
	last := r.Created.Unix()
	if activity > last {
		last = activity
	}
	if idle := now.Sub(time.Unix(last, 0)); idle < maxIdle {
		return fmt.Sprintf("recently active (idle %s)", idle.Round(time.Second))
	}

	return rigTeardownBlocker(r.Path, fetched)
}

// rigTeardownBlocker reports why a rig's work isn't safe to delete yet, or ""
// when every workspace is accounted for — merged, or holding no WIP. It's the
// shared judgment behind both `reap` (which gates it behind an idle window) and
// `down` (which runs it on an explicit teardown), so the two can't disagree
// about what "done" means. Fail-closed throughout: any error reading the rig,
// resolving a source repo, or asking jj/gh keeps the rig rather than guessing
// it's disposable. fetched dedups the per-source-repo git fetch across a sweep;
// pass a fresh map for a one-shot check.
func rigTeardownBlocker(basedir string, fetched map[string]bool) string {
	entries, err := os.ReadDir(basedir)
	if err != nil {
		return fmt.Sprintf("reading basedir: %v", err)
	}
	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Sprintf("reading manifest: %v", err)
	}
	// A review rig's terminal check is "have I posted a review?", which needs my
	// GitHub login. Resolve it once, up front, only when this rig is a review —
	// authoring rigs never touch it.
	login := ""
	if m.isReview() {
		l, err := ghCurrentLogin()
		if err != nil {
			return fmt.Sprintf("resolving GitHub login: %v", err)
		}
		login = l
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ws := filepath.Join(basedir, e.Name())
		if _, err := os.Stat(filepath.Join(ws, ".jj")); err != nil {
			continue
		}
		source, err := jjSourceRepo(ws)
		if err != nil {
			return fmt.Sprintf("%s: resolving source repo: %v", e.Name(), err)
		}
		// Best-effort fetch so trunk() reflects what merged since anyone
		// last fetched; without it reap is mostly inert. Once per source
		// repo per run, and failure just means a stale trunk — fail-closed.
		if !fetched[source] {
			fetched[source] = true
			_ = jjGitFetch(source)
		}
		branches, err := repoBranches(m, e.Name(), ws)
		if err != nil {
			return fmt.Sprintf("%s: resolving branches: %v", e.Name(), err)
		}
		if reason := workspaceTeardownBlocker(ws, m.Repos[e.Name()], e.Name(), branches, m.isReview(), login); reason != "" {
			return reason
		}
	}
	return ""
}

// workspaceTeardownBlocker judges whether one workspace's local work is safe to
// delete, returning the first reason it isn't ("" when it's clean). It's the
// lazy, offline-tolerant half of the teardown judgment: it only asks GitHub
// when there's actually off-trunk work to account for, so a freshly-pitched or
// fully-merged workspace reaps without a round-trip. `rig down` layers an eager
// all-PRs-merged gate on top (unmergedPRsBlocker); reap leans on this alone.
//
// The judgment differs by rig kind, because "done" does. An authoring rig
// accounts for all work reachable from @ against merged PR heads. A review rig
// guards none of the author's commits and instead requires that you've posted a
// review, then checks @ alone for scratch edits of your own. Both paths allow
// the direnv anchor rig wrote at setup: jj auto-tracks it in repos that ship no
// .envrc, but it isn't authored work.
func workspaceTeardownBlocker(ws, nameWithOwner, label string, branches []string, review bool, login string) string {
	if review {
		if reason := reviewTeardownBlocker(nameWithOwner, label, branches, login); reason != "" {
			return reason
		}
		atEmpty, err := jjRevsetEmpty(ws, "@ & ~empty() & ~::trunk()")
		if err != nil {
			return fmt.Sprintf("%s: jj check failed: %v", label, err)
		}
		if !atEmpty && !anchorOnlyWIP(ws) {
			return fmt.Sprintf("%s has working-copy changes", label)
		}
		return ""
	}
	return authoringTeardownBlocker(ws, nameWithOwner, label, branches)
}

// authoringTeardownBlocker accounts for an authoring rig's off-trunk work by
// immutable PR head commits, not bookmark names. GitHub commonly deletes a
// merged branch, but its PR head OID remains available and jj can still resolve
// the commit. This also lets a pushed, merged PR remain at @ without being
// mislabeled as "uncommitted": if the PR head covers it, the work is safe.
// Anything no merged PR head covers stays put, with a more specific diagnosis
// when the workspace has evolved onto an untracked or still-open branch.
func authoringTeardownBlocker(ws, nameWithOwner, label string, branches []string) string {
	atEmpty, err := jjRevsetEmpty(ws, "@ & ~empty() & ~::trunk()")
	if err != nil {
		return fmt.Sprintf("%s: jj check failed: %v", label, err)
	}
	anchorOnly := !atEmpty && anchorOnlyWIP(ws)
	work := "::@ & ~empty() & ~::trunk()"
	if anchorOnly {
		// The generated direnv anchor is setup metadata, not authored work.
		work = "::@ & ~@ & ~empty() & ~::trunk()"
	}

	// Cheap first: any authored off-trunk commit reachable from @? If not,
	// there's no local work to lose and nothing to verify against GitHub.
	empty, err := jjRevsetEmpty(ws, work)
	if err != nil {
		return fmt.Sprintf("%s: jj check failed: %v", label, err)
	}
	if empty {
		return ""
	}

	clauses := []string{work}
	prs := make(map[string]*prInfo, len(branches))
	for _, b := range branches {
		pr, err := prForBranch(nameWithOwner, b)
		if err != nil {
			return fmt.Sprintf("%s: checking PR for %s: %v", label, b, err)
		}
		prs[b] = pr
		if pr != nil && pr.State == "MERGED" {
			if pr.HeadOID == "" {
				return fmt.Sprintf("%s: checking PR #%d (%s): GitHub returned no head commit", label, pr.Number, b)
			}
			// If the head isn't in this repo, it cannot cover any commit in work.
			// Leaving it out fails closed: the remaining work blocks below.
			if revExists(ws, pr.HeadOID) {
				clauses = append(clauses, fmt.Sprintf("~::%q", pr.HeadOID))
			}
		}
	}
	remaining := strings.Join(clauses, " & ")
	beyond, err := jjRevsetEmpty(ws, remaining)
	if err != nil {
		return fmt.Sprintf("%s: jj check failed: %v", label, err)
	}
	if beyond {
		return ""
	}

	current, err := jjPRBranch(ws)
	if err != nil {
		return fmt.Sprintf("%s: resolving current branch: %v", label, err)
	}
	if current != "" {
		if !slices.Contains(branches, current) {
			return fmt.Sprintf("%s has work on untracked branch %s\n      run `rig track %s` to include it", label, current, current)
		}
		pr := prs[current]
		switch {
		case pr == nil:
			return fmt.Sprintf("%s branch %s has work but no PR", label, current)
		case pr.State != "MERGED":
			// Distinguish an exact pushed PR head from local work layered on top.
			if pr.HeadOID != "" && revExists(ws, pr.HeadOID) {
				extra, err := jjRevsetEmpty(ws, remaining+fmt.Sprintf(" & ~::%q", pr.HeadOID))
				if err != nil {
					return fmt.Sprintf("%s: jj check failed: %v", label, err)
				}
				if !extra {
					return fmt.Sprintf("%s has changes beyond PR #%d (%s)", label, pr.Number, current)
				}
			}
			return fmt.Sprintf("%s PR #%d (%s) is %s, not merged", label, pr.Number, current, strings.ToLower(pr.State))
		default:
			return fmt.Sprintf("%s has changes beyond merged PR #%d (%s)", label, pr.Number, current)
		}
	}
	if !atEmpty && !anchorOnly {
		return fmt.Sprintf("%s has working-copy changes", label)
	}
	return fmt.Sprintf("%s has unmerged work", label)
}

// reviewTeardownBlocker is gate 1b: a review rig is done once you've posted a
// review on every recorded branch that has a PR. The branch's commits are the
// author's, so their merge state is irrelevant — only your review is. An
// unreviewed branch (or a gh error) keeps the rig; login is the reviewer's
// GitHub identity, resolved once by the caller.
func reviewTeardownBlocker(nameWithOwner, label string, branches []string, login string) string {
	for _, b := range branches {
		pr, err := prForBranch(nameWithOwner, b)
		if err != nil {
			return fmt.Sprintf("%s: checking PR for %s: %v", label, b, err)
		}
		if pr == nil {
			continue // no PR for this branch — nothing to review
		}
		reviewed, err := reviewSubmittedByMe(nameWithOwner, b, login)
		if err != nil {
			return fmt.Sprintf("%s: checking your review on %s: %v", label, b, err)
		}
		if !reviewed {
			return fmt.Sprintf("%s PR #%d (%s) has no review from you yet", label, pr.Number, b)
		}
	}
	return ""
}

// anchorOnlyWIP reports whether the workspace's @ carries nothing but the
// direnv anchor rig itself wrote (addRepoWorkspace drops "source_up\n" when
// the repo ships no .envrc). Fail-closed: any doubt — diff error, extra
// files, content that isn't the bare anchor — reads as real WIP.
func anchorOnlyWIP(ws string) bool {
	cmd := exec.Command("jj", "-R", ws, "diff", "-r", "@", "--name-only")
	cmd.Dir = ws // jj prints paths relative to cwd; anchor to the workspace
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	if len(files) != 1 || files[0] != ".envrc" {
		return false
	}
	body, err := os.ReadFile(filepath.Join(ws, ".envrc"))
	return err == nil && string(body) == "source_up\n"
}
