package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// sweepDecision is the ladder that turns a rig's state into its next step. It's
// the whole policy of `rig sweep`, kept free of jj, gh, and the filesystem so it
// can be pinned down here.
func TestSweepDecision(t *testing.T) {
	cases := []struct {
		name       string
		in         sweepInput
		wantAction string
		wantDetail string // substring
	}{
		{
			name:       "merged and clean tears down",
			in:         sweepInput{Disp: "merged", Blocker: ""},
			wantAction: actionDown,
		},
		{
			name:       "merged but blocked reports the blocker",
			in:         sweepInput{Disp: "merged", Blocker: "rig has working-copy changes"},
			wantAction: actionNone,
			wantDetail: "working-copy changes",
		},
		{
			name: "approved and green offers the merge",
			in: sweepInput{Disp: "approved", PRs: []rigPR{
				{prInfo: prInfo{Number: 7, State: "OPEN", Review: "APPROVED", Checks: "passing"}},
			}},
			wantAction: actionMerge,
			wantDetail: "CI clear",
		},
		{
			// A repo with no CI configured reports no checks at all, which is a
			// normal merge-ready state rather than checks that haven't run.
			name: "approved with no checks configured is still mergeable",
			in: sweepInput{Disp: "approved", PRs: []rigPR{
				{prInfo: prInfo{Number: 8, State: "OPEN", Review: "APPROVED", Checks: ""}},
			}},
			wantAction: actionMerge,
		},
		{
			name: "approved but CI pending holds",
			in: sweepInput{Disp: "approved", PRs: []rigPR{
				{prInfo: prInfo{Number: 9, State: "OPEN", Review: "APPROVED", Checks: "pending"}},
			}},
			wantAction: actionNone,
			wantDetail: "CI pending",
		},
		{
			// Approved and red is still your move — the reviewer is done, the
			// build isn't — so it goes to needs-you rather than sitting quiet.
			name: "approved but CI failing summons you",
			in: sweepInput{Disp: "approved", PRs: []rigPR{
				{Repo: "o/runtime", prInfo: prInfo{Number: 10, State: "OPEN", Review: "APPROVED", Checks: "failing"}},
			}},
			wantAction: actionWake,
			wantDetail: "CI failing on runtime#10",
		},
		{
			name:       "changes requested wants you in the seat",
			in:         sweepInput{Disp: "changes requested"},
			wantAction: actionWake,
		},
		{
			name:       "still awaiting review does nothing",
			in:         sweepInput{Disp: "waiting"},
			wantAction: actionNone,
			wantDetail: "awaiting review",
		},
		{
			// The common shape of a finished rig: its PR merged, GitHub deleted
			// the branch, so there's nothing left to look up. An earlier cut hid
			// these behind a live-session check and they became unreachable.
			name:       "clean rig that shipped is offered up",
			in:         sweepInput{Disp: "no PR", Blocker: "", Shipped: true},
			wantAction: actionDown,
			wantDetail: "shipped, nothing outstanding",
		},
		{
			// Identical on disk to the case above, and a completely different
			// thing: nothing ever came of this one, so say so rather than call it
			// "nothing outstanding" as though it had finished.
			name:       "clean rig that never shipped says so",
			in:         sweepInput{Disp: "no PR", Blocker: ""},
			wantAction: actionDown,
			wantDetail: "no PR on record",
		},
		{
			name:       "no-PR rig holding WIP is left alone",
			in:         sweepInput{Disp: "no PR", Blocker: "rig has unmerged work"},
			wantAction: actionNone,
			wantDetail: "unmerged work",
		},
		{
			// Red CI is your move, not a reviewer's. parkedDisposition only folds
			// review state, so without this the row read "awaiting review" and sat
			// in the quiet pile.
			name: "failing CI outranks awaiting review",
			in: sweepInput{Disp: "waiting", PRs: []rigPR{
				{Repo: "o/mirendev", prInfo: prInfo{Number: 111, State: "OPEN", Review: "REVIEW_REQUIRED", Checks: "failing"}},
			}},
			wantAction: actionWake,
			wantDetail: "CI failing on mirendev#111",
		},
		{
			// Changes-requested is the more specific answer, so it still wins.
			name: "changes requested outranks failing CI",
			in: sweepInput{Disp: "changes requested", PRs: []rigPR{
				{Repo: "o/r", prInfo: prInfo{Number: 1, State: "OPEN", Review: "CHANGES_REQUESTED", Checks: "failing"}},
			}},
			wantAction: actionWake,
			wantDetail: "review came back with changes",
		},
		{
			// Pending checks resolve themselves; nagging would put half the board
			// in the needs-you pile every time anyone pushed.
			name: "pending CI is not a summons",
			in: sweepInput{Disp: "waiting", PRs: []rigPR{
				{Repo: "o/r", prInfo: prInfo{Number: 2, State: "OPEN", Review: "REVIEW_REQUIRED", Checks: "pending"}},
			}},
			wantAction: actionNone,
			wantDetail: "awaiting review",
		},
		{
			// A review rig's PRs belong to the author, so their merge state says
			// nothing about whether we're done. Only the teardown gate knows.
			name:       "review rig with the review posted tears down",
			in:         sweepInput{Disp: "waiting", Review: true, Blocker: ""},
			wantAction: actionDown,
			wantDetail: "review posted",
		},
		{
			name: "review rig with no review yet stays put",
			in: sweepInput{Disp: "merged", Review: true,
				Blocker: "rig PR #3 (feat) has no review from you yet"},
			wantAction: actionNone,
			wantDetail: "no review from you yet",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			action, detail := sweepDecision(c.in)
			if action != c.wantAction {
				t.Errorf("action = %q, want %q (detail %q)", action, c.wantAction, detail)
			}
			if c.wantDetail != "" && !strings.Contains(detail, c.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", detail, c.wantDetail)
			}
		})
	}
}

// mergeablePRs is the gate on the one irreversible action in the pass, so it has
// to be precise about which PRs it hands over and honest about the ones it holds.
func TestMergeablePRs(t *testing.T) {
	prs := []rigPR{
		{Repo: "o/a", prInfo: prInfo{Number: 1, State: "MERGED", Review: "APPROVED", Checks: "passing"}},
		{Repo: "o/b", prInfo: prInfo{Number: 2, State: "OPEN", Review: "APPROVED", Checks: "passing"}},
		{Repo: "o/c", prInfo: prInfo{Number: 3, State: "OPEN", Review: "APPROVED", Checks: "failing"}},
		{Repo: "o/d", prInfo: prInfo{Number: 4, State: "OPEN", Review: "REVIEW_REQUIRED", Checks: "passing"}},
	}
	ready, held := mergeablePRs(prs)
	if len(ready) != 1 || ready[0].Number != 2 {
		t.Fatalf("ready = %+v, want only #2", ready)
	}
	// An already-merged PR and an unapproved one are simply not candidates; a
	// red one is, and gets named so you know what the pass is holding back.
	detail := mergeDetail(ready, held)
	if !strings.Contains(detail, "#3 CI failing") {
		t.Errorf("detail = %q, want it to name the held PR", detail)
	}
	for _, name := range []string{"#1", "#4"} {
		if strings.Contains(detail, name) {
			t.Errorf("detail = %q, should not mention non-candidate %s", detail, name)
		}
	}
}

// A rig spans repos, so a merge row can carry several PRs. All of them land off
// one checkbox, which means the row has to name all of them.
func TestSweepMultiRepoMergeRow(t *testing.T) {
	p := sweepPlan{
		status: rigStatus{ID: "mir-75"},
		action: actionMerge,
		detail: "approved, CI clear",
		merges: []rigPR{
			{Repo: "o/runtime", prInfo: prInfo{Number: 971, Title: "Enable distributed runners"}},
			{Repo: "o/cloud", prInfo: prInfo{Number: 42, Title: "Wire the runner UI"}},
		},
	}
	if got, want := sweepRefs(p), "runtime#971 cloud#42"; got != want {
		t.Errorf("refs = %q, want %q", got, want)
	}
	subject := sweepSubject(p)
	for _, title := range []string{"Enable distributed runners", "Wire the runner UI"} {
		if !strings.Contains(subject, title) {
			t.Errorf("subject = %q, want it to name %q", subject, title)
		}
	}

	// The footer counts PRs, not rigs: this one rig is two merges.
	m := newSweepModel([]sweepPlan{p}, false)
	m.width = 120
	if got := m.View(); !strings.Contains(got, "nothing selected") {
		t.Errorf("merge rows start unchecked, so nothing should be queued:\n%s", got)
	}
	if got := press(m, " ").View(); !strings.Contains(got, "enter merge 2") {
		t.Errorf("footer should count both of the rig's PRs:\n%s", got)
	}
}

// A rig whose PRs disagree about review never reaches "approved", so the sweep
// leaves it alone rather than landing half a cross-repo change.
func TestSweepMixedReviewStatesAreLeftAlone(t *testing.T) {
	prs := []rigPR{
		{Repo: "o/runtime", prInfo: prInfo{Number: 1, State: "OPEN", Review: "APPROVED", Checks: "passing"}},
		{Repo: "o/cloud", prInfo: prInfo{Number: 2, State: "OPEN", Review: "REVIEW_REQUIRED", Checks: "passing"}},
	}
	if got := parkedDisposition(prs); got != "waiting" {
		t.Fatalf("disposition = %q, want waiting", got)
	}
	action, _ := sweepDecision(sweepInput{Disp: parkedDisposition(prs), PRs: prs})
	if action != actionNone {
		t.Errorf("action = %q, want nothing — half a cross-repo change must not land", action)
	}
}

// The board goes up before the scan finishes, so the loading frame has to say
// what it's waiting on rather than sit blank for the six seconds gh takes.
func TestSweepLoadingViewNarratesThePhase(t *testing.T) {
	m := newSweepModel(nil, false)
	m.loading = true
	m.width = 80

	if got := m.View(); !strings.Contains(got, "starting") {
		t.Errorf("view before the first phase should still say something:\n%s", got)
	}
	m = updateModel(m, sweepPhaseMsg("asking GitHub about 16 rigs"))
	got := m.View()
	if !strings.Contains(got, "asking GitHub about 16 rigs") {
		t.Errorf("view should narrate the current phase:\n%s", got)
	}
	if !strings.Contains(got, "cancel") {
		t.Errorf("loading view should offer a way out:\n%s", got)
	}
}

// A keystroke landing mid-scan must not half-act on a board with no rows in it.
func TestSweepKeysAreInertWhileLoading(t *testing.T) {
	m := newSweepModel(nil, false)
	m.loading = true
	if press(m, "enter").apply {
		t.Error("enter during the scan should not apply an empty board")
	}
	if press(m, " ").cursor != 0 {
		t.Error("space during the scan should do nothing")
	}
}

// The scan result populates the board, and a scan that turns up nothing
// actionable closes it rather than making anyone dismiss an empty screen.
func TestSweepScanMsgPopulatesTheBoard(t *testing.T) {
	m := newSweepModel(nil, false)
	m.loading = true
	m = updateModel(m, sweepScanMsg{plans: testPlans()})
	if m.loading {
		t.Error("scan result should clear the loading state")
	}
	if len(m.items) != 3 || len(m.plans) != 5 {
		t.Errorf("items = %d, plans = %d; want 3 and 5", len(m.items), len(m.plans))
	}

	// Nothing actionable: the caller reports instead, so the model must not have
	// applied anything on its way out.
	quiet := newSweepModel(nil, false)
	quiet.loading = true
	quiet = updateModel(quiet, sweepScanMsg{plans: []sweepPlan{
		{status: rigStatus{ID: "q"}, action: actionNone, detail: "awaiting review"},
	}})
	if quiet.apply {
		t.Error("an empty board must not report itself as applied")
	}
	if len(quiet.plans) != 1 {
		t.Error("the caller still needs the plans to print its report")
	}
}

// updateModel feeds one non-key message through Update.
func updateModel(m sweepModel, msg tea.Msg) sweepModel {
	next, _ := m.Update(msg)
	return next.(sweepModel)
}

// rigDirtyRepos has to subtract the rig's pushed PR heads, or the WIP marker
// fires on every open PR and stops distinguishing anything. A workspace parked
// exactly on its pushed head is the case that catches naive implementations:
// its working copy is genuinely non-empty, and nothing about it is unaccounted
// for.
func TestRigDirtyReposSubtractsPushedPRHeads(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}
	setHermeticGit(t)

	basedir := t.TempDir()
	ws := filepath.Join(basedir, "repo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main")
	run("git", "commit", "-q", "--allow-empty", "-m", "init")
	run("jj", "git", "init", "--colocate")
	run("jj", "config", "set", "--repo", `revset-aliases."trunk()"`, "main")

	// Feature commit C off trunk, with @ parked on an empty child of it — the
	// shape of a pushed, up-to-date PR.
	if err := os.WriteFile(filepath.Join(ws, "feat.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("jj", "commit", "-m", "feat")
	headRaw, err := exec.Command("jj", "-R", ws, "log", "--no-graph", "-r", "@-", "-T", "commit_id").Output()
	if err != nil {
		t.Fatalf("resolving feature commit: %v", err)
	}
	head := strings.TrimSpace(string(headRaw))

	m := manifest{ID: "t", Repos: map[string]string{"repo": "o/r"}}
	pushed := []rigPR{{Repo: "o/r", prInfo: prInfo{Number: 1, State: "OPEN", HeadOID: head}}}

	if got := rigDirtyRepos(basedir, m, pushed); len(got) != 0 {
		t.Errorf("dirty = %v, want none — the only off-trunk work is the pushed PR head", got)
	}
	// The same tree with no PR to account for it really is unaccounted work.
	if got := rigDirtyRepos(basedir, m, nil); len(got) != 1 || got[0] != "repo" {
		t.Errorf("dirty = %v, want [repo] when no PR covers the commit", got)
	}
	// And an edit layered on top of the pushed head is work the PR doesn't carry.
	if err := os.WriteFile(filepath.Join(ws, "extra.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := rigDirtyRepos(basedir, m, pushed); len(got) != 1 || got[0] != "repo" {
		t.Errorf("dirty = %v, want [repo] for edits beyond the PR head", got)
	}
}

// A dry run has to be inert even when it would otherwise abort: nothing is
// executed, so nothing can fail, and it reports the whole plan.
func TestExecuteSweepDryRunTouchesNothing(t *testing.T) {
	picked := []sweepPlan{
		{status: rigStatus{ID: "a", Path: "/tmp/a"}, action: actionMerge,
			merges: []rigPR{{Repo: "o/r", prInfo: prInfo{Number: 1}}}},
		{status: rigStatus{ID: "b", Path: "/tmp/b"}, action: actionDown},
	}
	// A real run here would shell out to gh and delete /tmp/b; a dry one must do
	// neither, so reaching the end without error is the whole assertion.
	if err := executeSweep(picked, true, "--merge", map[string]bool{}); err != nil {
		t.Fatalf("dry run returned %v", err)
	}
}

// A rig whose lock is held is someone else mid-command, not a broken sweep, so
// it's the one condition that skips instead of aborting the pass.
func TestSweepTeardownLockContentionIsASkip(t *testing.T) {
	// Locks live in runtime state, so point that somewhere writable — the nix
	// build sandbox has no home to fall back on.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()

	held, err := acquireRigLock(dir, false)
	if err != nil {
		t.Fatalf("taking the lock: %v", err)
	}
	defer func() { _ = held.Close() }()

	err = sweepTeardown(rigInfo{ID: "busy", Path: dir}, map[string]bool{})
	if !errors.Is(err, errSweepSkip) {
		t.Errorf("err = %v, want it to wrap errSweepSkip so the pass continues", err)
	}
}

// The subject ladder, rung by rung. Each one exists because the one above it
// can genuinely be missing: a PR title that didn't come back, then a manifest
// too old (or too hand-edited) to carry a title.
func TestSweepSubjectLadder(t *testing.T) {
	withPR := sweepPlan{
		status:     rigStatus{Title: "task title", PRs: []rigPR{{prInfo: prInfo{Number: 1, Title: "PR title"}}}},
		agentTitle: "agent title",
		action:     actionMerge,
		merges:     []rigPR{{Repo: "o/a", prInfo: prInfo{Number: 1, Title: "PR title"}}},
	}
	if got := sweepSubject(withPR); got != "PR title" {
		t.Errorf("subject = %q, want the PR title to win", got)
	}

	// A PR gh knew about but couldn't name falls through rather than rendering
	// an empty column.
	noTitle := withPR
	noTitle.merges = []rigPR{{Repo: "o/a", prInfo: prInfo{Number: 1}}}
	noTitle.status.PRs = noTitle.merges
	if got := sweepSubject(noTitle); got != "task title" {
		t.Errorf("subject = %q, want the task title once the PR has none", got)
	}

	untitled := noTitle
	untitled.status.Title = ""
	if got := sweepSubject(untitled); got != "agent title" {
		t.Errorf("subject = %q, want the agent session title as the last rung", got)
	}
}

// A lone PR speaks for its rig, so a wake or teardown row shows its title. Two
// PRs don't — joined they overrun a column you're only reading for context —
// so a multi-repo rig falls back to its own title outside the MERGE group,
// where the joined titles are the point.
func TestSweepSubjectMultiPRFallsBackOutsideMerge(t *testing.T) {
	p := sweepPlan{
		status: rigStatus{Title: "estimate review costs", PRs: []rigPR{
			{Repo: "o/reviewagent", prInfo: prInfo{Number: 19, Title: "Add a cost model"}},
			{Repo: "o/rfd", prInfo: prInfo{Number: 156, Title: "RFD-95: promote Biscuit"}},
		}},
		action: actionWake,
		detail: "CI failing on rfd#156",
	}
	if got := sweepSubject(p); got != "estimate review costs" {
		t.Errorf("subject = %q, want the rig's own title for a multi-PR wake row", got)
	}

	solo := p
	solo.status.PRs = solo.status.PRs[:1]
	if got := sweepSubject(solo); got != "Add a cost model" {
		t.Errorf("subject = %q, want the lone PR's title", got)
	}
}

// Every group gets a subject and keeps its why, and both start in the same
// screen column across all four groups. The board reading as one table is the
// point — a subject column that shifted between MERGE and NEEDS YOU would be
// worse than no subject column at all.
func TestSweepBoardSubjectAndWhyShareOneGrid(t *testing.T) {
	m := newSweepModel(testPlans(), false)
	m.width, m.showAll = 160, true
	view := m.View()

	subjectAt, whyAt := -1, -1
	for _, tc := range []struct{ id, subject, why string }{
		{"mir-982", "mir-982 title", "approved, CI clear"},
		{"old-rig", "old-rig title", "merged and clean"},
		{"mir-1364", "mir-1364 title", "review came back with changes"},
		{"mir-822", "mir-822 title", "awaiting review"},
	} {
		line := ""
		for l := range strings.SplitSeq(view, "\n") {
			if strings.Contains(l, tc.id) {
				line = l
				break
			}
		}
		if line == "" {
			t.Fatalf("no row for %s in:\n%s", tc.id, view)
		}
		s, w := strings.Index(line, tc.subject), strings.Index(line, tc.why)
		if s < 0 || w < 0 {
			t.Fatalf("row %s = %q, want it to carry both %q and %q", tc.id, line, tc.subject, tc.why)
		}
		if subjectAt < 0 {
			subjectAt, whyAt = s, w
			continue
		}
		if s != subjectAt {
			t.Errorf("row %s starts its subject at %d, want %d (one grid across groups)", tc.id, s, subjectAt)
		}
		if w != whyAt {
			t.Errorf("row %s starts its why at %d, want %d (one grid across groups)", tc.id, w, whyAt)
		}
	}
}

// The board's whole safety story is in its defaults: a teardown costs a rebuild
// if you get it wrong, a merge can't be taken back.
func TestSweepBoardDefaults(t *testing.T) {
	m := newSweepModel(testPlans(), false)
	if len(m.items) != 3 {
		t.Fatalf("actionable items = %d, want 3", len(m.items))
	}
	for _, it := range m.items {
		switch it.plan.action {
		case actionMerge:
			if it.selected {
				t.Errorf("%s: merge rows must start unchecked", it.plan.status.ID)
			}
		case actionDown:
			if !it.selected {
				t.Errorf("%s: teardown rows should start checked", it.plan.status.ID)
			}
		}
	}
	// Wake and quiet rigs are shown but never actionable.
	if len(m.inert) != 2 {
		t.Errorf("inert = %d, want 2", len(m.inert))
	}
}

// Whether a teardown starts checked is a different question from whether it's
// safe — the gate already settled safety by producing an actionDown at all.
// This is about whether losing the rig would annoy you.
func TestSweepCollectable(t *testing.T) {
	now := time.Now()
	recent := now.Add(-20 * time.Minute)
	stale := now.Add(-3 * 24 * time.Hour)

	cases := []struct {
		name       string
		shipped    bool
		agent      string
		lastActive *time.Time
		want       bool
	}{
		{"shipped and quiet is a formality", true, "idle", &recent, true},
		{"shipped but mid-turn waits for you", true, "working", &recent, false},
		{
			// The case that started all this: a kickoff conversation an hour old
			// with nothing committed yet. Safe to discard, and infuriating to.
			"nothing landed and still warm is protected", false, "idle", &recent, false,
		},
		{"nothing landed and long cold is collectable", false, "idle", &stale, true},
		{"no recorded activity at all reads as cold", false, "", nil, true},
		{"mid-turn beats staleness", false, "working", &stale, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sweepCollectable(c.shipped, c.agent, c.lastActive, now); got != c.want {
				t.Errorf("collectable = %v, want %v", got, c.want)
			}
		})
	}
}

// A protected rig is still shown and still one keystroke from checked. Hiding
// it was the old bug; the new behaviour must only soften the default.
func TestSweepProtectedTeardownIsStillOffered(t *testing.T) {
	plans := []sweepPlan{
		{status: rigStatus{ID: "warm-kickoff"}, action: actionDown, detail: "no PR on record"},
		{status: rigStatus{ID: "shipped"}, action: actionDown, detail: "shipped, nothing outstanding", collect: true},
	}
	m := newSweepModel(plans, false)
	if len(m.items) != 2 {
		t.Fatalf("both teardowns should be offered, got %d rows", len(m.items))
	}
	if m.items[0].selected {
		t.Error("a warm rig with nothing landed should not start checked")
	}
	if !m.items[1].selected {
		t.Error("a shipped, quiet rig should start checked")
	}
	// `a` is the one keystroke that collects the protected one too.
	if got := press(m, "a").items[0].selected; !got {
		t.Error("`a` should be able to check a protected teardown")
	}
}

// The metadata tail answers what the disposition can't: is there anything in
// the tree, how wide is this rig, and how long has it sat.
func TestSweepMeta(t *testing.T) {
	old := time.Now().Add(-50 * time.Hour)
	p := sweepPlan{
		status: rigStatus{ID: "r", LastActive: &old},
		repos:  3,
		dirty:  []string{"nix-config", "runtime"},
	}
	got := sweepMeta(p)
	for _, want := range []string{"WIP nix-config,runtime", "3 repos", "2d"} {
		if !strings.Contains(got, want) {
			t.Errorf("meta = %q, want it to contain %q", got, want)
		}
	}

	// A single-repo rig shouldn't waste the column saying "1 repos", and a rig
	// with a clean tree shouldn't claim WIP.
	quiet := sweepMeta(sweepPlan{status: rigStatus{ID: "r"}, repos: 1})
	if quiet != "" {
		t.Errorf("meta = %q, want empty for a clean single-repo rig with no activity", quiet)
	}
}

// Quiet rows carry PR refs too — "awaiting review" reads differently once you
// can see it's two PRs across two repos.
func TestSweepRefsCoverInertRows(t *testing.T) {
	p := sweepPlan{
		action: actionNone,
		status: rigStatus{ID: "r", PRs: []rigPR{
			{Repo: "o/reviewagent", prInfo: prInfo{Number: 19, State: "OPEN"}},
			{Repo: "o/rfd", prInfo: prInfo{Number: 156, State: "OPEN"}},
		}},
	}
	if got, want := sweepRefs(p), "reviewagent#19 rfd#156"; got != want {
		t.Errorf("refs = %q, want %q", got, want)
	}

	// A merge row narrows to what it would actually land.
	p.action = actionMerge
	p.merges = []rigPR{{Repo: "o/rfd", prInfo: prInfo{Number: 156}}}
	if got, want := sweepRefs(p), "rfd#156"; got != want {
		t.Errorf("merge refs = %q, want %q", got, want)
	}
}

// "select all" must never be able to queue a merge you didn't look at.
func TestSweepSelectAllSkipsMerges(t *testing.T) {
	m := newSweepModel(testPlans(), false)
	m = press(m, "a") // downs already all checked, so this clears them
	for _, it := range m.items {
		if it.plan.action == actionDown && it.selected {
			t.Errorf("%s: first `a` should have cleared the teardowns", it.plan.status.ID)
		}
	}
	m = press(m, "a") // and back on
	for _, it := range m.items {
		switch it.plan.action {
		case actionDown:
			if !it.selected {
				t.Errorf("%s: second `a` should have re-checked the teardowns", it.plan.status.ID)
			}
		case actionMerge:
			if it.selected {
				t.Errorf("%s: `a` must never check a merge", it.plan.status.ID)
			}
		}
	}
}

// Space is the deliberate keystroke that opts a merge in, and selected() has to
// hand back exactly what the boxes say.
func TestSweepToggleAndSelected(t *testing.T) {
	m := newSweepModel(testPlans(), false)
	m = press(m, " ") // cursor starts on the first row, a merge
	if got := m.selected(); len(got) != 2 {
		t.Fatalf("selected = %d, want 2 (the toggled merge plus the default teardown)", len(got))
	}
	m = press(m, " ") // back off
	got := m.selected()
	if len(got) != 1 || got[0].action != actionDown {
		t.Fatalf("selected = %+v, want just the teardown", got)
	}
}

// Quitting has to mean nothing happens, however many boxes are checked.
func TestSweepQuitAppliesNothing(t *testing.T) {
	for _, key := range []string{"q", "esc", "ctrl+c"} {
		t.Run(key, func(t *testing.T) {
			m := press(newSweepModel(testPlans(), false), key)
			if m.apply {
				t.Errorf("%s should not apply", key)
			}
		})
	}
	if m := press(newSweepModel(testPlans(), false), "enter"); !m.apply {
		t.Error("enter should apply")
	}
}

// The cursor only ever traverses rows you can act on, and can't run off either
// end of them.
func TestSweepCursorStaysInBounds(t *testing.T) {
	m := newSweepModel(testPlans(), false)
	for range 10 {
		m = press(m, "down")
	}
	if m.cursor != len(m.items)-1 {
		t.Errorf("cursor = %d, want it pinned at the last actionable row %d", m.cursor, len(m.items)-1)
	}
	for range 10 {
		m = press(m, "up")
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0", m.cursor)
	}
}

// The board is the whole picture, so every rig has to appear somewhere on it —
// including the ones there's nothing to do about.
func TestSweepViewShowsEveryRig(t *testing.T) {
	m := newSweepModel(testPlans(), false)
	m.width = 100
	m = press(m, "w") // expand the quiet group
	view := m.View()
	for _, id := range []string{"mir-982", "mir-955", "old-rig", "mir-1364", "mir-822"} {
		if !strings.Contains(view, id) {
			t.Errorf("view is missing %s:\n%s", id, view)
		}
	}
	for _, label := range []string{"MERGE", "TEAR DOWN", "NEEDS YOU", "QUIET"} {
		if !strings.Contains(view, label) {
			t.Errorf("view is missing the %s group:\n%s", label, view)
		}
	}
}

// The footer is the last thing you read before committing, so it has to count
// PRs (what actually merges) rather than rigs.
func TestSweepFooterCountsWhatEnterWillDo(t *testing.T) {
	m := newSweepModel(testPlans(), false)
	m.width = 100
	if got := m.View(); !strings.Contains(got, "enter down 1") {
		t.Errorf("footer should offer just the default teardown:\n%s", got)
	}
	m = press(m, " ") // opt the two-PR merge in
	if got := m.View(); !strings.Contains(got, "merge 2, down 1") {
		t.Errorf("footer should count PRs, not rigs:\n%s", got)
	}
}

// press feeds one keystroke to the model and hands back the updated one.
func press(m sweepModel, key string) sweepModel {
	var msg tea.KeyMsg
	switch key {
	case " ":
		msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+c":
		msg = tea.KeyMsg{Type: tea.KeyCtrlC}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	next, _ := m.Update(msg)
	return next.(sweepModel)
}

// testPlans is a board with one of everything: a merge carrying two PRs, a
// merge carrying one, a teardown, a rig that wants a human, and a quiet one.
func testPlans() []sweepPlan {
	plan := func(id, action, detail string, merges ...rigPR) sweepPlan {
		return sweepPlan{
			status: rigStatus{ID: id, Title: id + " title"},
			action: action, detail: detail, merges: merges,
			// Stands for a rig that shipped and went quiet, the ordinary case
			// where a teardown arrives pre-checked.
			collect: action == actionDown,
		}
	}
	pr := func(repo string, n int) rigPR {
		return rigPR{Repo: repo, prInfo: prInfo{Number: n, State: "OPEN", Review: "APPROVED", Checks: "passing"}}
	}
	return []sweepPlan{
		plan("mir-982", actionMerge, "approved, CI clear", pr("o/rfd", 154), pr("o/runtime", 971)),
		plan("mir-955", actionMerge, "approved, CI clear", pr("o/runtime", 972)),
		plan("old-rig", actionDown, "merged and clean"),
		plan("mir-1364", actionWake, "review came back with changes"),
		plan("mir-822", actionNone, "awaiting review"),
	}
}

// The walk reads best when it advances work first, clears the board second, and
// leaves the rigs that want a human for the report at the end.
func TestSweepRankOrdersTheWalk(t *testing.T) {
	want := []string{actionMerge, actionDown, actionWake, actionNone}
	for i := 1; i < len(want); i++ {
		if sweepRank(want[i-1]) >= sweepRank(want[i]) {
			t.Errorf("sweepRank(%q) should sort before sweepRank(%q)", want[i-1], want[i])
		}
	}
}
