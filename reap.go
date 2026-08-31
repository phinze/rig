package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// runReap implements `rig reap`: the unattended janitor for rig's runtime
// state. It retries teardown jobs a previous `down` or `sweep` left stranded,
// then stops the tmux and iso scopes of rigs that are already gone.
//
// It deliberately does not decide which rigs should stop existing. That
// judgment used to live here behind a 24h idle window, and it could not work:
// rigTeardownBlocker reasons entirely in commits, branches, and PR states, so
// a rig that produced none of those — a long exploration, a planning session,
// anything whose whole value is the agent conversation it carries — was
// indistinguishable from one whose work had shipped and merged. Both read as
// "no PR, clean tree", and the nightly pass silently collected them at 3am
// with nobody looking. `rig sweep` asks the same question with the row in
// front of a human, which is the only way it can be asked safely; see
// sweepCollectable, which already refuses to pre-check exactly this case.
func runReap(args []string) error {
	dryRun := false
	for _, arg := range args {
		switch arg {
		case "--dry-run", "-n":
			dryRun = true
		case "--runtime-only":
			// Accepted as a no-op: runtime cleanup is now all reap does. The
			// deployed systemd units still pass this flag, and a unit and a
			// binary that update out of step shouldn't fail hourly over it.
		default:
			return fmt.Errorf("usage: rig reap [--dry-run|-n]")
		}
	}

	jobs, err := pendingTeardownJobs()
	if err != nil {
		return err
	}
	for _, path := range jobs {
		job, err := readTeardownJob(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rig: warning: unreadable teardown job %s: %v\n", path, err)
			continue
		}
		if dryRun {
			fmt.Fprintf(os.Stderr, "rig: would retry teardown %s — %s\n", job.ID, path)
			continue
		}
		fmt.Fprintf(os.Stderr, "rig: retry teardown %s — %s\n", job.ID, path)
		if err := executeTeardownJobFile(path, true); err != nil {
			fmt.Fprintf(os.Stderr, "rig: retry teardown %s failed: %v\n", job.ID, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "rig: completed pending teardown %s\n", job.ID)
	}
	rigs, err := listRigs()
	if err != nil {
		return err
	}
	active := make(map[string]bool, len(rigs))
	for _, rig := range rigs {
		active[rig.ID] = true
	}
	tearingDown := map[string]bool{}
	remainingJobs, err := pendingTeardownJobs()
	if err != nil {
		return err
	}
	for _, path := range remainingJobs {
		if job, err := readTeardownJob(path); err == nil {
			tearingDown[job.ID] = true
		}
	}
	cleaned, err := cleanupOrphanedRigRuntime(active, tearingDown, dryRun)
	if err != nil {
		return err
	}
	verb := "cleaned"
	if dryRun {
		verb = "would clean"
	}
	fmt.Fprintf(os.Stderr, "rig: runtime cleanup complete — %s %d orphan scopes\n", verb, cleaned)
	return nil
}

// rigTeardownBlocker reports why a rig's work isn't safe to delete yet, or ""
// when every workspace is accounted for — merged, or holding no WIP. It's the
// shared judgment behind both `down` (which runs it on an explicit teardown)
// and `sweep` (which runs it per row before offering one), so the two can't
// disagree about what "done" means. Note what it cannot see: a rig whose value
// is a conversation rather than a commit reads as perfectly clean here, which
// is why nothing unattended is allowed to act on it alone — see runReap.
// Fail-closed throughout: any error reading the rig,
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
	// Authoring rigs are only done when every recorded PR is merged. The local
	// commit accounting above is intentionally lazy, but it cannot see a
	// disjoint secondary PR whose commits are no longer reachable from @. Reap
	// and explicit down must share this eager final gate.
	if m.isAuthoring() {
		return unmergedPRsBlocker(basedir)
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
// review, then checks @ alone for scratch edits of your own.
func workspaceTeardownBlocker(ws, nameWithOwner, label string, branches []string, review bool, login string) string {
	if review {
		if reason := reviewTeardownBlocker(nameWithOwner, label, branches, login); reason != "" {
			return reason
		}
		atEmpty, err := jjRevsetEmpty(ws, "@ & ~empty() & ~::trunk()")
		if err != nil {
			return fmt.Sprintf("%s: jj check failed: %v", label, err)
		}
		if !atEmpty {
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
	work := "::@ & ~empty() & ~::trunk()"

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
	if !atEmpty {
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
