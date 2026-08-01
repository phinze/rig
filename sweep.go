package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// runSweep implements `rig sweep`: the Monday-morning pass that walks every rig
// and moves each one to its next step. It's the actionable middle the lifecycle
// was missing. `rig waiting` computes exactly this judgment and then prints a
// table you retype by hand; `rig radar` renders it live but its only verb is "go
// there"; `rig reap` acts, but only on the fully-resolved-and-cold end, and only
// ever to tear down. None of them advance a rig that's sitting one command away
// from done — which on a Monday morning is most of them.
//
// Shape is plan-then-stream, the same trick radar plays: a Bubble Tea board of
// proposed actions where you toggle what to apply, then the TUI releases the
// terminal and the real gh and teardown output streams underneath it. A board is
// the right surface for choosing across a dozen rigs at once (a y/n-per-rig walk
// made you answer six prompts to find the one that mattered), and a plain stream
// is the right surface for watching the work happen.
//
// Teardown reuses `down`'s gate untouched, so sweep adds no new destructive
// path: `down` is safe to batch precisely because the lifecycle invariant holds
// (down can't destroy anything up can't rebuild). Merging has no such property —
// it's the one irreversible act here — so merge rows start unchecked and never
// come along with "select all".
func runSweep(args []string) error {
	dryRun := false
	// Merge commits, because that's what we use. Squash was the first guess and
	// it was simply wrong: mirendev/runtime disallows it outright, so every merge
	// in the first real run died on "Squash merges are not allowed".
	mergeFlag := "--merge"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run", "-n":
			dryRun = true
		case "--merge-method":
			i++
			if i >= len(args) {
				return fmt.Errorf("--merge-method needs a value (squash|merge|rebase)")
			}
			switch args[i] {
			case "merge", "squash", "rebase":
				mergeFlag = "--" + args[i]
			default:
				return fmt.Errorf("--merge-method: expected merge, squash, or rebase, got %q", args[i])
			}
		default:
			return fmt.Errorf("usage: rig sweep [--dry-run|-n] [--merge-method merge|squash|rebase]")
		}
	}

	// fetched dedups the per-source-repo git fetch that rigTeardownBlocker does,
	// exactly as reap uses it, so trunk() reflects what merged over the weekend
	// without re-fetching once per rig.
	fetched := map[string]bool{}

	// No terminal means no board, and every action here needs a deliberate check.
	// Degrade to the report rather than refuse, so the command stays safe to pipe
	// or drop in a cron.
	if !stdinIsTTY() {
		plans, err := sweepScan(fetched, nil)
		if err != nil {
			return err
		}
		if len(plans) == 0 {
			fmt.Fprintln(os.Stderr, "rig: no rigs in flight")
			return nil
		}
		fmt.Fprintln(os.Stderr, "rig: stdin isn't a terminal — showing the plan, nothing will be changed")
		reportSweep(plans)
		return nil
	}

	// The scan runs inside the TUI rather than before it. It costs a gh
	// round-trip per branch plus a jj fetch and teardown check per candidate,
	// which is several seconds of a completely silent terminal if you do it first
	// and open the board after. Same render-now-fill-in-later posture the radar
	// takes, for the same reason.
	plans, picked, err := sweepPick(fetched, dryRun)
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		fmt.Fprintln(os.Stderr, "rig: no rigs in flight")
		return nil
	}
	if len(picked) == 0 {
		reportSweep(plans)
		return nil
	}
	return executeSweep(picked, dryRun, mergeFlag, fetched)
}

// sweep actions, in the order the board groups them: merge advances work, down
// clears the board, wake needs you in the seat (so it's shown, never executed),
// and "" means this rig wants nothing today.
const (
	actionNone  = ""
	actionMerge = "merge"
	actionDown  = "down"
	actionWake  = "wake"
)

// sweepPlan is one rig's proposed next step: what we'd do, why, and (for a
// merge) which PRs it applies to. merges is a slice because a rig spans repos —
// a multi-repo rig carries one PR per repo, plus any recorded by `rig track`,
// and they land together or not at all.
type sweepPlan struct {
	rig    rigInfo
	status rigStatus
	action string
	detail string
	merges []rigPR  // every PR this rig would land
	held   []string // PRs that kept themselves out, e.g. "#3 CI failing"
	repos  int      // how many repos the rig spans
	dirty  []string // repos holding local work, for the WIP marker
	// agentTitle is what the agent called its own conversation, filled in only
	// when the rungs above it in sweepSubject came back empty. Reading it costs
	// a file read, so the plan carries the answer rather than the board asking
	// for it on every repaint.
	agentTitle string
	// collect is whether a teardown row should arrive pre-checked. See
	// sweepCollectable — it is not the same question as "is this safe", which
	// the gate already answered by producing an actionDown at all.
	collect bool
}

// sweepStaleAfter is how long a rig with nothing to show for itself must sit
// untouched before its teardown arrives pre-checked. It's reap's idle window,
// on purpose: reap already had to decide when a rig has stopped mattering, and
// two different answers to that would be worse than either.
const sweepStaleAfter = 24 * time.Hour

// sweepCollectable decides whether a teardown starts checked. A rig that
// demonstrably shipped is a formality — the work is on trunk, tearing down is
// bookkeeping — so it's checked as soon as it's clean. A rig with nothing to
// show for itself is the opposite: a clean tree there means nothing came of it
// *yet*, and every rig carries a live agent conversation whose only anchor is
// the basedir path. Discarding that an hour after a kickoff is hostile, so it
// waits for the staleness window.
//
// Either way the row is visible and one keystroke from checked; this only picks
// the default. An agent mid-turn is never pre-checked, whatever else is true.
func sweepCollectable(shipped bool, agent string, lastActive *time.Time, now time.Time) bool {
	if agent == "working" {
		return false
	}
	if shipped {
		return true
	}
	// No recorded activity at all reads as cold, matching reap's treatment of a
	// rig with no agent session to date.
	return lastActive == nil || now.Sub(*lastActive) > sweepStaleAfter
}

// sweepInput is everything the ladder decides from, pulled out as plain data so
// the policy is testable without a filesystem, a jj repo, or GitHub.
type sweepInput struct {
	Disp    string // parkedDisposition's vocabulary, computed over all rigs
	PRs     []rigPR
	Review  bool   // manifest kind == review
	Blocker string // rigTeardownBlocker's verdict, "" when clean
	// Shipped means this rig was seen to have a PR at some point, live or
	// recorded in the manifest. It's what separates a rig that finished and had
	// its branch deleted from one that never produced anything — both of which
	// otherwise present as "no PR, clean tree".
	Shipped bool
}

// sweepDecision is the ladder: disposition in, next step out. It deliberately
// reuses parkedDisposition's vocabulary so sweep, waiting, and the radar can
// never disagree about what a rig's state is — they only differ in what they do
// about it.
func sweepDecision(in sweepInput) (action, detail string) {
	// A review rig's terminal condition is "you've posted a review", not a merge,
	// so its disposition (derived from the author's PR) says nothing useful. The
	// teardown gate already encodes the right question; trust it.
	if in.Review {
		if in.Blocker != "" {
			return actionNone, in.Blocker
		}
		return actionDown, "review posted"
	}

	switch in.Disp {
	case "changes requested":
		return actionWake, "review came back with changes"
	}

	// Red CI is your move, not a reviewer's, so it outranks everything below and
	// leaves the quiet section entirely. parkedDisposition can't see this — it
	// folds review state only — and burying "CI failing" under "awaiting review"
	// was the single most misleading thing the board did.
	if red := failingPRs(in.PRs); len(red) > 0 {
		return actionWake, "CI failing on " + strings.Join(red, ", ")
	}

	switch in.Disp {
	case "merged":
		if in.Blocker != "" {
			return actionNone, in.Blocker
		}
		return actionDown, "merged and clean"

	case "approved":
		// Every open PR on this rig is approved (that's what "approved" means
		// here), so a multi-repo rig lands all of them or none. A rig whose PRs
		// disagree — one approved, one still out — reads as "waiting" and is
		// deliberately left alone: landing half a cross-repo change is worse than
		// landing none of it.
		ready, held := mergeablePRs(in.PRs)
		if len(ready) == 0 {
			return actionNone, mergeDetail(ready, held)
		}
		return actionMerge, mergeDetail(ready, held)

	case "no PR":
		// "No PR" covers more ground than it sounds like: work you haven't pushed
		// yet, a repo that lands straight on trunk and never has one, and — most
		// often — a rig whose PR merged and whose branch GitHub then deleted, so
		// there's nothing left to look up. Only the teardown gate can tell those
		// apart, so always ask it.
		//
		// An earlier cut short-circuited on a live tmux session here, meaning to
		// protect the rig you're sitting in. Nearly every rig has a live session,
		// so all it really did was hide finished work: three merged, spotless rigs
		// sat in the quiet list as "in flight" and could never be offered. Whether
		// you're mid-thought is now a checkbox default (see newSweepModel), not a
		// reason to withhold the row.
		if in.Blocker != "" {
			return actionNone, in.Blocker
		}
		if in.Shipped {
			return actionDown, "shipped, nothing outstanding"
		}
		// Not "nothing happened here" — we genuinely don't know. A rig that
		// merged before the manifest started recording PR numbers lands here too,
		// and claiming its work never existed would be a lie the staleness
		// default is already covering for.
		return actionDown, "no PR on record"

	default: // waiting
		return actionNone, "awaiting review"
	}
}

// failingPRs names the rig's open PRs whose CI is red. Pending checks aren't
// here: those resolve themselves, and nagging about them would put half the
// board in the needs-you pile every time someone pushed.
func failingPRs(prs []rigPR) []string {
	var out []string
	for _, pr := range prs {
		if pr.State == "OPEN" && pr.Checks == "failing" {
			out = append(out, shortRepo(pr.Repo)+"#"+strconv.Itoa(pr.Number))
		}
	}
	return out
}

// mergeablePRs splits an approved rig's PRs into the ones that could merge right
// now and short notes about the ones holding back. An empty Checks string means
// the repo has no checks configured rather than checks that haven't reported,
// which is a normal merge-ready state, so it counts as clear.
func mergeablePRs(prs []rigPR) (ready []rigPR, held []string) {
	for _, pr := range prs {
		if pr.State != "OPEN" || pr.Review != "APPROVED" {
			continue
		}
		if pr.Checks == "failing" || pr.Checks == "pending" {
			held = append(held, fmt.Sprintf("#%d CI %s", pr.Number, pr.Checks))
			continue
		}
		ready = append(ready, pr)
	}
	return ready, held
}

// mergeDetail is a merge row's why. It's the same for every row in the MERGE
// group by construction — that's what qualified them — which is why it can't
// be the only thing a row says, and why the subject column exists. What it
// does carry that nothing else does is the holding note: an approved rig whose
// sibling PR is still going red or amber.
func mergeDetail(ready []rigPR, held []string) string {
	if len(ready) == 0 {
		if len(held) > 0 {
			return "approved but " + strings.Join(held, ", ")
		}
		return "approved"
	}
	detail := "approved, CI clear"
	if len(held) > 0 {
		detail += " (holding " + strings.Join(held, ", ") + ")"
	}
	return detail
}

// observedPRNumbers maps the rig's live PRs back onto the repo subdirs they
// belong to, which is the shape the manifest records. Only repos the manifest
// doesn't already know about are returned, so a settled rig produces no write.
func observedPRNumbers(m manifest, prs []rigPR) map[string]int {
	var seen map[string]int
	for sub, nameWithOwner := range m.Repos {
		if m.PRs[sub] != 0 {
			continue
		}
		for _, pr := range prs {
			if pr.Number > 0 && strings.EqualFold(pr.Repo, nameWithOwner) {
				if seen == nil {
					seen = map[string]int{}
				}
				seen[sub] = pr.Number
				break
			}
		}
	}
	return seen
}

// rigDirtyRepos names the repos in a rig holding work that isn't accounted for
// yet — uncommitted changes, or commits that are neither on trunk nor covered
// by one of the rig's PR heads. It's what tells a rig you've genuinely started
// from one that's merely sitting there, which the disposition alone can't:
// "no PR" reads identically for both.
//
// Subtracting the PR heads is what makes the marker mean something. Without it
// every open PR's own commits read as WIP, so an approved, fully-pushed rig
// wore the same "WIP" as one holding unpushed work — the exact distinction the
// marker exists to draw. The same immutable-head-OID trick authoringTeardownBlocker
// uses, so the two agree about what counts as accounted for.
//
// Deliberately local-only: no git fetch, no gh, just jj revsets against whatever
// trunk() already resolves to, so it's cheap enough to run for every rig on the
// board rather than only teardown candidates. A stale trunk can at worst call
// freshly-merged work dirty, which errs toward telling you something is there.
func rigDirtyRepos(basedir string, m manifest, prs []rigPR) []string {
	subdirs := make([]string, 0, len(m.Repos))
	for sub := range m.Repos {
		subdirs = append(subdirs, sub)
	}
	sort.Strings(subdirs)

	var dirty []string
	for _, sub := range subdirs {
		ws := filepath.Join(basedir, sub)
		if _, err := os.Stat(filepath.Join(ws, ".jj")); err != nil {
			continue
		}
		// One question covers both halves. Uncommitted edits live in @ itself, so
		// a dirty working copy shows up as a non-empty off-trunk commit here and
		// needs no separate check — and asking separately actively misleads,
		// because a workspace parked exactly on its pushed PR head has a
		// perfectly non-empty @ and nothing unaccounted for at all.
		clauses := []string{"::@ & ~empty() & ~::trunk()"}
		for _, pr := range prs {
			if pr.HeadOID == "" || !strings.EqualFold(pr.Repo, m.Repos[sub]) {
				continue
			}
			if revExists(ws, pr.HeadOID) {
				clauses = append(clauses, fmt.Sprintf("~::%q", pr.HeadOID))
			}
		}
		beyond, err := jjRevsetEmpty(ws, strings.Join(clauses, " & "))
		// A workspace jj can't read is a state worth naming, not one to silently
		// call clean — mir-822 sat broken for days looking tidy.
		if err != nil {
			dirty = append(dirty, sub+"?")
			continue
		}
		if !beyond {
			dirty = append(dirty, sub)
		}
	}
	return dirty
}

// sweepScan builds the whole picture: every rig, its live PRs, and the step it
// wants next. report, when non-nil, is called with a short description of each
// phase as it starts, so the board can say what it's waiting on instead of
// showing a frozen terminal. It's called from the scan's own goroutine.
func sweepScan(fetched map[string]bool, report func(string)) ([]sweepPlan, error) {
	say := func(s string) {
		if report != nil {
			report(s)
		}
	}
	say("reading workspaces")
	rigs, err := listRigs()
	if err != nil {
		return nil, err
	}
	if len(rigs) == 0 {
		return nil, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	statuses := rigStatuses(rigs, home, time.Now())
	say(fmt.Sprintf("asking GitHub about %d rigs", len(statuses)))
	enrichWithPRs(statuses)
	return planSweep(rigs, statuses, home, fetched, say), nil
}

// planSweep builds one plan per rig and orders them the way the board groups
// them: merges first (they advance work), then teardowns (they clear it), then
// the rigs that want a human, then everything quiet. The teardown gate is
// consulted lazily — it costs jj and gh calls, and only the merged / no-PR /
// review branches of the ladder actually consume it.
func planSweep(rigs []rigInfo, statuses []rigStatus, home string, fetched map[string]bool, say func(string)) []sweepPlan {
	byPath := make(map[string]rigInfo, len(rigs))
	for _, r := range rigs {
		byPath[r.Path] = r
	}

	now := time.Now()
	plans := make([]sweepPlan, 0, len(statuses))
	for _, s := range statuses {
		r, ok := byPath[s.Path]
		if !ok {
			continue
		}
		m, err := readManifest(s.Path)
		if err != nil {
			plans = append(plans, sweepPlan{rig: r, status: s, detail: fmt.Sprintf("reading manifest: %v", err)})
			continue
		}
		// Note any PR we can see right now, so this rig stays recognisable as one
		// that shipped after GitHub deletes the branch. Best-effort: a failure
		// here costs nothing but a repeat next sweep, and must never fail a scan.
		if seen := observedPRNumbers(m, s.PRs); len(seen) > 0 {
			if _, err := recordObservedPRs(s.Path, seen); err != nil {
				fmt.Fprintf(os.Stderr, "rig: warning: recording PRs for %s: %v\n", s.ID, err)
			} else {
				for sub, n := range seen {
					if m.PRs == nil {
						m.PRs = map[string]int{}
					}
					if m.PRs[sub] == 0 {
						m.PRs[sub] = n
					}
				}
			}
		}

		in := sweepInput{
			Disp:    parkedDisposition(s.PRs),
			PRs:     s.PRs,
			Review:  m.isReview(),
			Shipped: len(m.PRs) > 0 || len(s.PRs) > 0,
		}
		// The teardown gate is the expensive half — a jj fetch plus a gh call per
		// branch — so it's consulted only where the answer can change the verdict,
		// and it's the thing worth narrating while you wait.
		if in.Review || in.Disp == "merged" || in.Disp == "no PR" {
			if say != nil {
				say("checking " + s.ID)
			}
			in.Blocker = rigTeardownBlocker(s.Path, fetched)
		}
		action, detail := sweepDecision(in)
		p := sweepPlan{
			rig: r, status: s, action: action, detail: detail,
			repos:   len(m.Repos),
			dirty:   rigDirtyRepos(s.Path, m, s.PRs),
			collect: sweepCollectable(in.Shipped, s.Agent, s.LastActive, now),
		}
		if action == actionMerge {
			p.merges, p.held = mergeablePRs(s.PRs)
		}
		// Last rung of the subject ladder, and the only one that costs a read —
		// so ask for it only once the free rungs have come back empty.
		if sweepSubject(p) == "" {
			p.agentTitle = claudeSessionTitle(home, s.Path)
		}
		plans = append(plans, p)
	}

	sort.SliceStable(plans, func(i, j int) bool {
		return sweepRank(plans[i].action) < sweepRank(plans[j].action)
	})
	return plans
}

func sweepRank(action string) int {
	switch action {
	case actionMerge:
		return 0
	case actionDown:
		return 1
	case actionWake:
		return 2
	default:
		return 3
	}
}

// executeSweep carries out what you checked, streaming as it goes, and returns
// how many PRs it merged (the caller's signal that re-planning could surface new
// teardowns). This runs after the TUI has released the terminal, so gh's own
// output and the teardown log land where you can read them.
//
// It fails fast. The first real error stops the pass with everything after it
// untouched, because a failure here is nearly always a fact about your setup
// rather than about one PR — the first run hit a repo that forbids squash
// merges, and grinding through the remaining five to fail identically each time
// just buried the one line that mattered. A rig whose lock is held is the sole
// exception: that's someone else mid-command, not a problem with the sweep, so
// it's a skip.
func executeSweep(picked []sweepPlan, dryRun bool, mergeFlag string, fetched map[string]bool) error {
	fmt.Fprintln(os.Stderr)
	for _, p := range picked {
		switch p.action {
		case actionMerge:
			for _, pr := range p.merges {
				if dryRun {
					fmt.Fprintf(os.Stderr, "  would merge %s#%d\n", pr.Repo, pr.Number)
					continue
				}
				fmt.Fprintf(os.Stderr, "  merging %s#%d…\n", pr.Repo, pr.Number)
				if err := ghMergePR(pr.Repo, pr.Number, mergeFlag); err != nil {
					return fmt.Errorf("merging %s#%d: %w", pr.Repo, pr.Number, err)
				}
			}
			if dryRun {
				fmt.Fprintf(os.Stderr, "  then down %s if it comes up clean\n", p.status.ID)
				continue
			}
			// Merging was the only thing standing between this rig and teardown,
			// so carry straight on rather than sending you back to the board for a
			// second pass over a rig you already decided about. The gate still has
			// the final say: a rig holding work beyond the PR stays, and says why.
			if err := sweepCascade(p, fetched); err != nil {
				return err
			}
		case actionDown:
			if dryRun {
				fmt.Fprintf(os.Stderr, "  would down %s — %s\n", p.status.ID, p.status.Path)
				continue
			}
			if err := sweepDown(p, fetched); err != nil {
				return err
			}
		}
	}
	return nil
}

// sweepCascade tears a rig down immediately after its PRs merged. The merge just
// moved trunk under every workspace, so the fetch cache is dropped first — the
// gate would otherwise ask its question against a trunk that predates the very
// merge we're reacting to, and refuse every time.
func sweepCascade(p sweepPlan, fetched map[string]bool) error {
	clear(fetched)
	if reason := rigTeardownBlocker(p.status.Path, fetched); reason != "" {
		fmt.Fprintf(os.Stderr, "  %s stays — %s\n", p.status.ID, reason)
		return nil
	}
	return sweepDown(p, fetched)
}

// sweepDown runs one teardown and reports it, turning a busy rig into a skip
// rather than an abort.
func sweepDown(p sweepPlan, fetched map[string]bool) error {
	if err := sweepTeardown(p.rig, fetched); err != nil {
		if errors.Is(err, errSweepSkip) {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", p.status.ID, err)
			return nil
		}
		return fmt.Errorf("tearing down %s: %w", p.status.ID, err)
	}
	fmt.Fprintf(os.Stderr, "  down %s — %s gone\n", p.status.ID, p.status.Path)
	return nil
}

// sweepTeardown tears one rig down under its lock, re-running the gate inside
// the lock the way reap does. The plan was built (and checked off) outside the
// lock, which is deliberate — holding it while you read the board would block
// every other rig command — so the state it was built from has to be re-verified
// before anything is deleted.
func sweepTeardown(r rigInfo, fetched map[string]bool) error {
	lock, err := acquireRigLock(r.Path, true)
	if err != nil {
		return fmt.Errorf("%w — another rig command holds its lock", errSweepSkip)
	}
	defer func() { _ = lock.Close() }()

	m, err := readManifest(r.Path)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	if reason := rigTeardownBlocker(r.Path, fetched); reason != "" {
		return fmt.Errorf("state changed while the board was open: %s", reason)
	}
	return teardownRig(r.Path, m)
}

// reportSweep prints the whole picture as plain text. It's what you get when
// there's nothing to act on, when you quit the board, and when there's no
// terminal to draw one in.
func reportSweep(plans []sweepPlan) {
	if len(plans) == 0 {
		return
	}
	// tabwriter, not fixed-width columns: rig ids run from "mir-982" to a 55-char
	// kickoff slug, and padding sized for the former mangles the latter.
	w := tabwriter.NewWriter(os.Stderr, 0, 2, 2, ' ', 0)
	for _, p := range plans {
		next := "-"
		switch p.action {
		case actionMerge:
			next = "ready to merge"
		case actionDown:
			next = "ready to tear down"
		case actionWake:
			next = "rig wake " + p.status.ID
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			p.status.ID, sweepRefs(p), sweepSubject(p), p.detail, sweepMeta(p), next)
	}
	_ = w.Flush()
}

// errSweepSkip marks a per-rig condition that shouldn't abort the whole pass.
var errSweepSkip = errors.New("skipped")

// ghMergePR merges a PR by number. Deliberately no --delete-branch: teardown
// accounts for merged work by immutable head OID and copes fine with a branch
// GitHub already removed, but deleting the *local* branch out from under a live
// jj workspace is a different and much worse problem.
//
// stderr is captured rather than passed through so a failure reads as one line.
// Letting gh write straight to the terminal printed its GraphQL complaint and
// then our own "exit status 1" underneath it, which said the same thing twice
// and buried the half that was actionable.
func ghMergePR(nameWithOwner string, number int, mergeFlag string) error {
	cmd := exec.Command("gh", "pr", "merge", strconv.Itoa(number),
		"-R", nameWithOwner, mergeFlag)
	var stderr strings.Builder
	cmd.Stdout, cmd.Stderr = os.Stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// ---- the plan board ----------------------------------------------------

// sweepItem is one checkable row on the board.
type sweepItem struct {
	plan     sweepPlan
	selected bool
}

// sweepModel is the plan screen: actionable rigs as checkable rows grouped by
// verb, everything else rendered read-only underneath so the board is the whole
// picture rather than just the part you can act on.
type sweepModel struct {
	plans   []sweepPlan // everything the scan found, in board order
	items   []sweepItem // actionable, merges first then downs
	inert   []sweepPlan // wake + nothing-to-do, display only
	cursor  int
	showAll bool // expand the quiet rigs
	dryRun  bool
	apply   bool
	width   int
	height  int

	// Loose inbox entries, banner'd above the groups. A sweep is the pass where
	// you decide what to do about everything, so a background job that has been
	// failing since Tuesday belongs in it even though it isn't a rig.
	inbox []notification

	// The scan runs while the board is already on screen, so the model starts
	// empty and loading. phase is whatever the scan last said it was doing.
	loading bool
	phase   string
	spin    spinner.Model
	scanErr error
}

// sweepScanMsg delivers the finished scan; sweepPhaseMsg narrates it on the way.
type (
	sweepScanMsg struct {
		plans []sweepPlan
		err   error
	}
	sweepPhaseMsg string
)

// newSweepModel splits the plans into what you can act on and what you can only
// look at. Merge rows start unchecked: it's the one irreversible action in the
// pass, so it takes a deliberate keystroke, and "select all" is defined to leave
// them alone. Teardown defaults come from sweepCollectable, which is stricter
// than "is this safe" — a rig can be perfectly safe to tear down and still be
// one you'd be annoyed to lose.
func newSweepModel(plans []sweepPlan, dryRun bool) sweepModel {
	m := sweepModel{dryRun: dryRun}
	m.spin = spinner.New(spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(radarGoodStyle))
	m.load(plans)
	return m
}

// load splits a scan's plans into the rows you can act on and the ones you can
// only read.
func (m *sweepModel) load(plans []sweepPlan) {
	m.plans = plans
	m.inbox = looseNotifications(activeNotifications())
	m.items, m.inert = nil, nil
	for _, p := range plans {
		switch p.action {
		case actionMerge:
			m.items = append(m.items, sweepItem{plan: p, selected: false})
		case actionDown:
			m.items = append(m.items, sweepItem{plan: p, selected: p.collect})
		default:
			m.inert = append(m.inert, p)
		}
	}
}

func (m sweepModel) Init() tea.Cmd {
	if m.loading {
		return m.spin.Tick
	}
	return nil
}

func (m sweepModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case sweepPhaseMsg:
		m.phase = string(msg)
		return m, nil

	case sweepScanMsg:
		m.loading = false
		if msg.err != nil {
			m.scanErr = msg.err
			return m, tea.Quit
		}
		m.load(msg.plans)
		// Nothing to choose between: don't make anyone dismiss an empty board.
		// The caller prints the same report it would have shown.
		if len(m.items) == 0 {
			return m, tea.Quit
		}
		return m, nil

	case spinner.TickMsg:
		if !m.loading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		// While the scan is out there's nothing to toggle, so only the exits are
		// live — a keystroke that half-acts on an empty board is worse than one
		// that does nothing.
		if m.loading {
			switch msg.String() {
			case "ctrl+c", "q", "esc":
				return m, tea.Quit
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.apply = false
			return m, tea.Quit
		case "enter":
			m.apply = true
			return m, tea.Quit
		case "down", "j", "ctrl+n":
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case "up", "k", "ctrl+p":
			if m.cursor > 0 {
				m.cursor--
			}
		case " ", "x":
			if m.cursor < len(m.items) {
				m.items[m.cursor].selected = !m.items[m.cursor].selected
			}
		case "a":
			// Deliberately teardown-only. Merges are irreversible, so a single
			// key must never be able to queue one you didn't look at.
			all := true
			for _, it := range m.items {
				if it.plan.action == actionDown && !it.selected {
					all = false
					break
				}
			}
			for i := range m.items {
				if m.items[i].plan.action == actionDown {
					m.items[i].selected = !all
				}
			}
		case "w":
			m.showAll = !m.showAll
		}
	}
	return m, nil
}

// selected returns the checked plans in board order.
func (m sweepModel) selected() []sweepPlan {
	var out []sweepPlan
	for _, it := range m.items {
		if it.selected {
			out = append(out, it.plan)
		}
	}
	return out
}

func (m sweepModel) View() string {
	var b strings.Builder

	total := len(m.items) + len(m.inert)
	title := " rig sweep"
	if m.dryRun {
		title += radarWarnStyle.Render("  (dry run)")
	}
	if m.loading {
		phase := m.phase
		if phase == "" {
			phase = "starting"
		}
		return title + "\n\n " + m.spin.View() + " " +
			radarFaintStyle.Render(phase) + "\n\n" +
			radarFaintStyle.Render(" q to cancel") + "\n"
	}
	count := fmt.Sprintf("%d rigs ", total)
	gap := max(1, m.width-lipgloss.Width(title)-lipgloss.Width(count))
	b.WriteString(title + strings.Repeat(" ", gap) + radarFaintStyle.Render(count) + "\n")

	if lines := notifyBanner(m.inbox); len(lines) > 0 {
		b.WriteString("\n" + radarHeaderStyle.Render(" INBOX") + "\n")
		for _, line := range lines {
			b.WriteString("   " + radarWarnStyle.Render(line) + "\n")
		}
	}

	c := m.columns()

	writeGroup := func(header, action string) {
		first := true
		for i, it := range m.items {
			if it.plan.action != action {
				continue
			}
			if first {
				b.WriteString("\n" + radarHeaderStyle.Render(" "+header) + "\n")
				first = false
			}
			b.WriteString(m.renderItem(i, it, c) + "\n")
		}
	}
	writeGroup("MERGE", actionMerge)
	writeGroup("TEAR DOWN", actionDown)

	var wake, quiet []sweepPlan
	for _, p := range m.inert {
		if p.action == actionWake {
			wake = append(wake, p)
		} else {
			quiet = append(quiet, p)
		}
	}
	// Inert rows sit under the checkboxes rather than beside them: blanks where
	// a "[x]" would be, so the id column still lines up.
	inertRow := func(p sweepPlan) string {
		lead, subject, why, meta := m.sweepLine("   ", p, c)
		return lead + radarFaintStyle.Render(subject) +
			sweepWhyStyle(p).Render(why) + radarFaintStyle.Render(meta)
	}
	if len(wake) > 0 {
		b.WriteString("\n" + radarHeaderStyle.Render(" NEEDS YOU") + "\n")
		for _, p := range wake {
			b.WriteString(inertRow(p) + "\n")
		}
	}
	if len(quiet) > 0 {
		if m.showAll {
			b.WriteString("\n" + radarHeaderStyle.Render(" QUIET") + "\n")
			for _, p := range quiet {
				b.WriteString(inertRow(p) + "\n")
			}
		} else {
			b.WriteString("\n" + radarFaintStyle.Render(
				fmt.Sprintf(" %d quiet — w to show", len(quiet))) + "\n")
		}
	}

	// The footer counts what enter would actually do, so the commitment is
	// legible before you make it rather than after.
	nMerge, nDown := 0, 0
	for _, it := range m.items {
		if !it.selected {
			continue
		}
		if it.plan.action == actionMerge {
			nMerge += len(it.plan.merges)
		} else {
			nDown++
		}
	}
	var acts []string
	if nMerge > 0 {
		acts = append(acts, fmt.Sprintf("merge %d", nMerge))
	}
	if nDown > 0 {
		acts = append(acts, fmt.Sprintf("down %d", nDown))
	}
	apply := "nothing selected"
	if len(acts) > 0 {
		apply = strings.Join(acts, ", ")
	}
	b.WriteString("\n" + radarFaintStyle.Render(
		" space toggle · a all teardowns · w quiet · enter "+apply+" · q quit") + "\n")

	return b.String()
}

// Caps on the fixed columns, so one outlier row can't starve the subject
// column every other row is read for. A Linear id is 8 characters and a kickoff
// slug can be 55. A why is usually "merged and clean" but a teardown blocker
// carries a jj error and runs long. sweepMinSubject is the floor the subject
// keeps on a narrow terminal, below which it stops being worth showing.
const (
	sweepMaxIDWidth  = 30
	sweepMaxRefWidth = 26
	sweepMaxWhyWidth = 30
	sweepMinSubject  = 12
)

// sweepCols is the board's column grid, resolved once per frame across every
// group so the whole thing reads as one table rather than four that each found
// their own alignment. The fixed columns size to their content (capped); the
// subject takes whatever's left.
type sweepCols struct {
	id      int
	ref     int // 0 when no row on the board has a PR, and then the column vanishes
	subject int // 0 means no terminal width yet: don't clip, don't pad
	why     int
	meta    int
}

func (m sweepModel) columns() sweepCols {
	var c sweepCols
	measure := func(p sweepPlan) {
		c.id = max(c.id, lipgloss.Width(p.status.ID))
		c.ref = max(c.ref, lipgloss.Width(sweepRefs(p)))
		c.why = max(c.why, lipgloss.Width(p.detail))
		c.meta = max(c.meta, lipgloss.Width(sweepMeta(p)))
	}
	for _, it := range m.items {
		measure(it.plan)
	}
	for _, p := range m.inert {
		measure(p)
	}
	c.id = min(c.id, sweepMaxIDWidth)
	c.why = min(c.why, sweepMaxWhyWidth)
	if m.width > 0 {
		// The subject takes the slack: everything the fixed columns don't want,
		// less the two single-space gutters that follow it and one column held
		// back from the right edge.
		c.subject = max(sweepMinSubject, m.width-c.leadWidth()-c.why-c.meta-3)
	}
	return c
}

// leadWidth is everything left of the subject: the checkbox, the id, and the
// refs when any row has them.
func (c sweepCols) leadWidth() int {
	w := 1 + 3 + 1 + c.id + 2
	if c.ref > 0 {
		w += c.ref + 2
	}
	return w
}

// sweepRefs renders a row's PR refs ("runtime#971"). Merge rows name only the
// PRs they'd land; every other row names all of them, so a rig's scope is
// visible even when there's nothing to do about it — "awaiting review" reads
// very differently once you can see it's two PRs across two repos.
func sweepRefs(p sweepPlan) string {
	prs := p.status.PRs
	if p.action == actionMerge {
		prs = p.merges
	}
	refs := make([]string, 0, len(prs))
	for _, pr := range prs {
		refs = append(refs, shortRepo(pr.Repo)+"#"+strconv.Itoa(pr.Number))
	}
	return sweepClip(strings.Join(refs, " "), sweepMaxRefWidth)
}

// sweepMeta is the right-aligned tail: local work, breadth, and how long it's
// been since anyone touched the rig. All three come from data the scan already
// has, and each answers a question the disposition alone can't — whether
// there's anything in the tree, whether this is a one-repo change or a
// five-repo one, and whether "awaiting review" means since lunch or since
// Tuesday.
func sweepMeta(p sweepPlan) string {
	var parts []string
	if len(p.dirty) > 0 {
		parts = append(parts, "WIP "+strings.Join(p.dirty, ","))
	}
	if p.repos > 1 {
		parts = append(parts, strconv.Itoa(p.repos)+" repos")
	}
	if p.status.LastActive != nil {
		parts = append(parts, age(*p.status.LastActive))
	}
	return strings.Join(parts, " · ")
}

// sweepLine lays one row onto the shared grid and hands its four regions back
// rather than a finished string, so the caller can colour the subject by
// selection and the why by urgency — or reverse-video the lot for the cursor
// row. Concatenated in order they are the whole line.
//
// box is the three-cell checkbox, or blanks for the read-only groups, which is
// what keeps the id column aligned across rows you can act on and rows you
// can't.
func (m sweepModel) sweepLine(box string, p sweepPlan, c sweepCols) (lead, subject, why, meta string) {
	lead = " " + box + " " + padRight(sweepClip(p.status.ID, c.id), c.id) + "  "
	if c.ref > 0 {
		lead += padRight(sweepRefs(p), c.ref) + "  "
	}
	subject = sweepClip(sweepSubject(p), c.subject)
	why = sweepClip(p.detail, c.why)
	meta = sweepMeta(p)
	if c.subject == 0 {
		// No WindowSizeMsg yet, so there's no edge to align against. Keep the
		// columns separated and let the terminal wrap.
		return lead, subject, "  " + why, "  " + meta
	}
	return lead, padRight(subject, c.subject) + " ",
		padRight(why, c.why) + " ", padLeft(meta, c.meta)
}

// sweepWhyStyle colours the why column. Red is reserved for a why that names
// something broken — today that's exactly the wake rows' failing CI, and it's
// the whole reason those rows left the quiet section. Before the subject
// column existed the red covered the entire line; now it sits on the four
// words that earned it and the title beside them stays readable.
func sweepWhyStyle(p sweepPlan) lipgloss.Style {
	if p.action == actionWake {
		return radarHotStyle
	}
	return radarFaintStyle
}

// sweepSubject is a row's answer to "what is this rig about" — the column that
// keeps the board from being a list of ticket numbers you have to remember.
// Every group gets one, which is the point: MERGE rows already carried PR
// titles and were the only group that read well, while TEAR DOWN showed "no PR
// on record" three times over and left you guessing which of three ticket
// numbers you were about to delete.
//
// The ladder runs PR title, then task title, then agent session title. A PR
// title is the most authoritative statement of what a rig produced, and it's
// the one you most want in front of you before pressing enter on a merge. The
// task title is free (it's already on the status, and radar has been rendering
// it all along) and always present, so the third rung almost never fires — it's
// there for a manifest written before the field existed, or one edited by hand.
func sweepSubject(p sweepPlan) string {
	if t := sweepPRTitles(p); t != "" {
		return t
	}
	if p.status.Title != "" {
		return p.status.Title
	}
	return p.agentTitle
}

// sweepPRTitles is the PR rung of the ladder. A merge row names every PR it
// would land, because one checkbox lands all of them and you should see what
// you're committing to. Everywhere else a lone PR speaks for the rig and
// several don't — two titles joined overrun a column you're only reading for
// context — so a multi-PR rig falls through to its own title instead.
func sweepPRTitles(p sweepPlan) string {
	if p.action == actionMerge {
		titles := make([]string, 0, len(p.merges))
		for _, pr := range p.merges {
			if pr.Title != "" {
				titles = append(titles, pr.Title)
			}
		}
		return strings.Join(titles, " · ")
	}
	if len(p.status.PRs) == 1 {
		return p.status.PRs[0].Title
	}
	return ""
}

// sweepClip trims s to w display cells, marking the cut with an ellipsis. Only
// ever called on unstyled text — clipping a string that already carries ANSI
// would cut it mid-escape. w <= 0 means "don't".
func sweepClip(s string, w int) string {
	if w <= 0 || lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > w-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + "…"
}

// renderItem draws one checkable row. The cursor reverses the whole line, so
// the row's own colors are dropped there — reverse video over colored text is
// unreadable in half the palettes people run.
//
// Selection state rides the subject rather than the why, because the subject
// is the widest cell on the row and so the one that reads as "this is queued"
// from across the board.
func (m sweepModel) renderItem(i int, it sweepItem, c sweepCols) string {
	box := "[ ]"
	if it.selected {
		box = "[x]"
	}
	lead, subject, why, meta := m.sweepLine(box, it.plan, c)

	if i == m.cursor {
		return radarCursorStyle.Render(lead + subject + why + meta + " ")
	}
	style := radarGoodStyle
	if it.plan.action == actionDown {
		style = radarDoneStyle
	}
	if !it.selected {
		style = radarFaintStyle
	}
	return lead + style.Render(subject) +
		sweepWhyStyle(it.plan).Render(why) + radarFaintStyle.Render(meta)
}

// sweepPick puts the board up immediately and runs the scan behind it, feeding
// phase updates in as they happen. It returns everything the scan found (so the
// caller can report when there's nothing to do) and the subset you checked off.
// An empty pick means you quit, chose nothing, or there was nothing to choose.
func sweepPick(fetched map[string]bool, dryRun bool) (plans, picked []sweepPlan, err error) {
	m := newSweepModel(nil, dryRun)
	m.loading = true
	p := tea.NewProgram(m, tea.WithAltScreen())

	go func() {
		found, err := sweepScan(fetched, func(phase string) {
			p.Send(sweepPhaseMsg(phase))
		})
		p.Send(sweepScanMsg{plans: found, err: err})
	}()

	final, err := p.Run()
	if err != nil {
		return nil, nil, err
	}
	fm := final.(sweepModel)
	if fm.scanErr != nil {
		return nil, nil, fm.scanErr
	}
	if !fm.apply {
		return fm.plans, nil, nil
	}
	return fm.plans, fm.selected(), nil
}
