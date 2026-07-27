package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
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
}

// sweepInput is everything the ladder decides from, pulled out as plain data so
// the policy is testable without a filesystem, a jj repo, or GitHub.
type sweepInput struct {
	Disp        string // parkedDisposition's vocabulary, computed over all rigs
	PRs         []rigPR
	Review      bool   // manifest kind == review
	Blocker     string // rigTeardownBlocker's verdict, "" when clean
	SessionLive bool
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
		// A rig with no PR is either abandoned scaffolding or work you haven't
		// pushed yet. The teardown gate tells those apart on disk, but it can't
		// see the one case that matters most: a rig you are sitting in right now,
		// which has a live session and no reason to be swept. Leaving it alone is
		// the whole difference between a useful pass and an annoying one.
		if in.SessionLive {
			return actionNone, "in flight"
		}
		if in.Blocker != "" {
			return actionNone, in.Blocker
		}
		return actionDown, "no PR and nothing to lose"

	default: // waiting
		return actionNone, "awaiting review"
	}
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

// mergeDetail is the one-line explanation for everywhere that isn't the board:
// the text report, and the non-tty path. The board itself shows PR titles
// instead, since "approved, CI clear" is true of every row in its MERGE group
// by construction and says nothing you can tell two rigs apart by.
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
	return planSweep(rigs, statuses, fetched, say), nil
}

// planSweep builds one plan per rig and orders them the way the board groups
// them: merges first (they advance work), then teardowns (they clear it), then
// the rigs that want a human, then everything quiet. The teardown gate is
// consulted lazily — it costs jj and gh calls, and only the merged / no-PR /
// review branches of the ladder actually consume it.
func planSweep(rigs []rigInfo, statuses []rigStatus, fetched map[string]bool, say func(string)) []sweepPlan {
	byPath := make(map[string]rigInfo, len(rigs))
	for _, r := range rigs {
		byPath[r.Path] = r
	}

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
		in := sweepInput{
			Disp:        parkedDisposition(s.PRs),
			PRs:         s.PRs,
			Review:      m.isReview(),
			SessionLive: s.SessionLive,
		}
		// The teardown gate is the expensive half — a jj fetch plus a gh call per
		// branch — so it's both consulted lazily and the thing worth narrating.
		if in.Review || in.Disp == "merged" || (in.Disp == "no PR" && !in.SessionLive) {
			if say != nil {
				say("checking " + s.ID)
			}
			in.Blocker = rigTeardownBlocker(s.Path, fetched)
		}
		action, detail := sweepDecision(in)
		p := sweepPlan{rig: r, status: s, action: action, detail: detail}
		if action == actionMerge {
			p.merges, p.held = mergeablePRs(s.PRs)
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
		fmt.Fprintf(w, "  %s\t%s\t%s\n", p.status.ID, p.detail, next)
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
// them alone. Teardowns start checked — a wrong one costs a `rig up` rebuild.
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
	m.items, m.inert = nil, nil
	for _, p := range plans {
		switch p.action {
		case actionMerge:
			m.items = append(m.items, sweepItem{plan: p, selected: false})
		case actionDown:
			m.items = append(m.items, sweepItem{plan: p, selected: true})
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

	// One id column shared by every group, so the board reads as one table
	// rather than four that each found their own alignment. Kickoff slugs run to
	// 55 characters and would otherwise shove the detail off the right edge for
	// the sake of one row, so the column is capped and long ids are clipped.
	wID := 0
	for _, it := range m.items {
		wID = max(wID, lipgloss.Width(it.plan.status.ID))
	}
	for _, p := range m.inert {
		wID = max(wID, lipgloss.Width(p.status.ID))
	}
	wID = min(wID, sweepMaxIDWidth)

	// Same for the PR refs, so "rfd#154" and "runtime#971" start in one place.
	wRef := 0
	for _, it := range m.items {
		wRef = max(wRef, lipgloss.Width(sweepRefs(it.plan)))
	}

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
			b.WriteString(m.renderItem(i, it, wID, wRef) + "\n")
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
	// Inert rows sit under the checkboxes rather than beside them: four leading
	// spaces where a "[x] " would be, so the id column still lines up.
	inertRow := func(p sweepPlan, style lipgloss.Style, trailer string) string {
		id := padRight(sweepClip(p.status.ID, wID), wID)
		lead := "     " + id + "  "
		detail := sweepClip(p.detail, m.detailWidth(lipgloss.Width(lead)+lipgloss.Width(trailer)))
		row := lead + style.Render(detail)
		if trailer != "" {
			row += "  " + radarFaintStyle.Render(trailer)
		}
		return row
	}
	if len(wake) > 0 {
		b.WriteString("\n" + radarHeaderStyle.Render(" NEEDS YOU") + "\n")
		for _, p := range wake {
			b.WriteString(inertRow(p, radarHotStyle, "rig wake "+p.status.ID) + "\n")
		}
	}
	if len(quiet) > 0 {
		if m.showAll {
			b.WriteString("\n" + radarHeaderStyle.Render(" QUIET") + "\n")
			for _, p := range quiet {
				b.WriteString(inertRow(p, radarFaintStyle, "") + "\n")
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

// sweepMaxIDWidth caps the id column. A Linear id is 8 characters and a kickoff
// slug can be 55; letting the longest one size the column costs every other row
// its detail.
const sweepMaxIDWidth = 30

// sweepRefs renders a merge row's PR refs ("runtime#971"), or "" for any other
// action.
func sweepRefs(p sweepPlan) string {
	if p.action != actionMerge {
		return ""
	}
	refs := make([]string, len(p.merges))
	for i, pr := range p.merges {
		refs[i] = shortRepo(pr.Repo) + "#" + strconv.Itoa(pr.Number)
	}
	return strings.Join(refs, " ")
}

// sweepTail is a row's free-text column. Merge rows carry PR titles: a branch
// name and a rig id are often the same slug twice, so "approved, CI clear" —
// true of every row in the group by construction — left you unable to tell which
// PR you were about to land. Everything else shows why it's here. A rig spanning
// repos names each title, since one checkbox lands all of them.
func sweepTail(p sweepPlan) string {
	if p.action != actionMerge {
		return p.detail
	}
	titles := make([]string, 0, len(p.merges))
	for _, pr := range p.merges {
		if pr.Title != "" {
			titles = append(titles, pr.Title)
		}
	}
	tail := strings.Join(titles, " · ")
	if tail == "" {
		tail = p.detail // no titles came back; better than a blank row
	}
	if len(p.held) > 0 {
		tail += " (holding " + strings.Join(p.held, ", ") + ")"
	}
	return tail
}

// detailWidth is what's left for the free-text detail column after the fixed
// ones. Zero width (no WindowSizeMsg yet) means don't clip at all rather than
// clip to nothing.
func (m sweepModel) detailWidth(used int) int {
	if m.width <= 0 {
		return 0
	}
	return max(12, m.width-used-2)
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
func (m sweepModel) renderItem(i int, it sweepItem, wID, wRef int) string {
	box := "[ ]"
	if it.selected {
		box = "[x]"
	}
	id := padRight(sweepClip(it.plan.status.ID, wID), wID)
	lead := " " + box + " " + id + "  "
	if wRef > 0 {
		lead += padRight(sweepRefs(it.plan), wRef) + "  "
	}
	tail := sweepClip(sweepTail(it.plan), m.detailWidth(lipgloss.Width(lead)))

	if i == m.cursor {
		return radarCursorStyle.Render(lead + tail + " ")
	}
	style := radarGoodStyle
	if it.plan.action == actionDown {
		style = radarDoneStyle
	}
	if !it.selected {
		style = radarFaintStyle
	}
	return lead + style.Render(tail)
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
