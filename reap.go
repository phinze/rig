package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	reaped := 0
	for _, r := range rigs {
		if reason := reapBlocker(r, home, now, maxIdle, fetched); reason != "" {
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
func reapBlocker(r rigInfo, home string, now time.Time, maxIdle time.Duration, fetched map[string]bool) string {
	// Attention gate first: recent attention means the rig is mid-thought
	// regardless of merge state. Two signals, both persistent and neither
	// resettable by accident: claude session-file mtimes (a turn appends
	// whether human-driven or autonomous; repaint doesn't) and the rig's
	// own age (a rig younger than the idle window can't be idle). File
	// changes are deliberately the VCS gates' job below — jj sees any
	// non-ignored modification as WIP; losing gitignored scratch is the
	// accepted cost of not mtime-crawling every workspace nightly. tmux
	// signals all failed here: output-based ones are pinned by claude's
	// at-rest TUI repaint, and attach-based ones reset on a mere peek, so
	// checking whether a rig was dead would keep it alive another day.
	last := r.Created.Unix()
	if t := claudeSessionActivity(home, r.Path); t > last {
		last = t
	}
	if idle := now.Sub(time.Unix(last, 0)); idle < maxIdle {
		return fmt.Sprintf("recently active (idle %s)", idle.Round(time.Second))
	}

	entries, err := os.ReadDir(r.Path)
	if err != nil {
		return fmt.Sprintf("reading basedir: %v", err)
	}
	m, err := readManifest(r.Path)
	if err != nil {
		return fmt.Sprintf("reading manifest: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ws := filepath.Join(r.Path, e.Name())
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
		// Merged + no-WIP below @: any non-empty commit reachable from @
		// that isn't on trunk is a candidate for blocking the reap. Catches
		// the jj-new-on-top-of-WIP shape and genuinely unmerged work alike.
		empty, err := jjRevsetEmpty(ws, "::@ & ~@ & ~empty() & ~::trunk()")
		if err != nil {
			return fmt.Sprintf("%s: jj check failed: %v", e.Name(), err)
		}
		if !empty {
			// Off-trunk commits don't prove the work is unmerged: a squash
			// merge lands on trunk as a brand-new commit, so the branch's
			// originals never become trunk ancestors and look unmerged
			// forever. Ask GitHub before believing jj here. Fail closed —
			// no branch, no PR, an unmerged PR, or a gh error all keep the
			// rig; only a confirmed MERGED state lets us look past trunk.
			// The branch comes from the manifest (recorded at up/review) when
			// we have it, else the bookmark heuristic.
			branch, _ := repoBranch(m, e.Name(), ws)
			if reason := unmergedReason(ws, m.Repos[e.Name()], e.Name(), branch); reason != "" {
				return reason
			}
		}
		// @ itself gets one allowance: the direnv anchor rig wrote at
		// setup. jj auto-tracks it, leaving @ permanently non-empty in
		// repos that ship no .envrc of their own — without the carve-out
		// no such rig would ever reap. Anything else dirty blocks.
		atEmpty, err := jjRevsetEmpty(ws, "@ & ~empty() & ~::trunk()")
		if err != nil {
			return fmt.Sprintf("%s: jj check failed: %v", e.Name(), err)
		}
		if !atEmpty && !anchorOnlyWIP(ws) {
			return fmt.Sprintf("%s has uncommitted changes", e.Name())
		}
	}
	return ""
}

// unmergedReason decides whether a workspace with off-trunk commits should
// still block the reap, returning the blocker reason ("" means the work is
// accounted for and the reap may proceed). It only runs when jj has already
// found non-empty off-trunk commits below @, so it's lazy: one gh call per
// rig that's a reap candidate, never a sweep. label is the workspace subdir,
// used in the reason strings; branch is the rig's resolved branch for this
// repo (empty when we couldn't determine one).
func unmergedReason(ws, nameWithOwner, label, branch string) string {
	if branch == "" {
		// No branch to map to a PR — treat as unmerged local work.
		return fmt.Sprintf("%s has unmerged work", label)
	}
	pr, err := prForBranch(nameWithOwner, branch)
	if err != nil || pr == nil || pr.State != "MERGED" {
		// Couldn't confirm a merge (offline, no PR, still open): fail closed.
		return fmt.Sprintf("%s has unmerged work", label)
	}
	// PR merged: the branch's own commits are done even though they aren't
	// trunk ancestors. Only work layered ON TOP of the merged branch still
	// counts as unfinished, so re-check excluding the branch's ancestry.
	beyond, err := jjRevsetEmpty(ws,
		fmt.Sprintf("::@ & ~@ & ~empty() & ~::trunk() & ~::%q", branch))
	if err != nil {
		return fmt.Sprintf("%s: jj check failed: %v", label, err)
	}
	if !beyond {
		return fmt.Sprintf("%s has work beyond the merged PR", label)
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
