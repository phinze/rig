package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// runRadar is the live board over everything rig knows: switch and waiting
// folded into one TUI, meant to live in a tmux popup on ilmari's old prefix-i
// slot. It renders immediately from local state (sessions, agent recency, park
// stamps), then fires the same per-branch gh fan-out ls --full uses and fills
// the PR/review cells in as they land. Local state re-ticks every couple
// seconds on top, so a rig going hot updates while you're looking at it.
//
// Enter does the right thing per row so you never pick a verb: a live rig
// switches, a sessionless one gets a bare session stood up first, a parked one
// wakes. The popup inherits $TMUX, so switch-client from inside it moves the
// underlying client, and the -E popup tears down as we exit.
func runRadar(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: rig radar")
	}
	if !stdinIsTTY() {
		return fmt.Errorf("radar is a TUI — run it from a terminal (or tmux popup)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	m := radarModel{
		home:    home,
		current: currentTmuxSession(),
		prs:     map[string][]rigPR{},
		pending: map[string]bool{},
	}
	// Scan before the first frame so the board renders instantly from local
	// state; the PR fan-out and the tick start from Init.
	scan := radarScanNow(home)
	if scan.err != nil {
		return scan.err
	}
	m.apply(scan)

	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		return err
	}
	chosen := final.(radarModel).chosen
	if chosen == nil {
		return nil
	}
	return radarAct(*chosen)
}

// radarAct is what Enter meant: wake the rig if it was parked, stand a session
// up if it lacks one, and land in it. This is the tail of switch and wake,
// executed after the TUI has released the terminal.
func radarAct(s rigStatus) error {
	if s.Parked {
		m, err := readManifest(s.Path)
		if err != nil {
			return fmt.Errorf("reading manifest: %w", err)
		}
		m.Parked = time.Time{}
		if err := writeManifest(s.Path, m); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "rig: woke %s\n", m.ID)
	}
	session := tmuxSessionName(s.Path)
	if !tmuxHasSession(session) {
		if err := tmuxNewSession(session, s.Path); err != nil {
			return fmt.Errorf("tmux new-session: %w", err)
		}
	}
	return attachOrReport(session)
}

// radarModel is the Bubble Tea model. The framework layer stays thin: state is
// the two sections plus a PR cache keyed by slug (map presence = fetch landed,
// so "no PR" and "not asked yet" stay distinguishable), and all the real logic
// lives in the helpers switch/waiting/ls already share.
type radarModel struct {
	home    string
	current string // tmux session under the popup; dropped from in-flight

	inflight []rigStatus
	parked   []rigStatus
	attached map[string]int64 // session → last-attached, for in-flight order
	prs      map[string][]rigPR
	pending  map[string]bool // slug → PR fetch in flight

	cursor  int
	chosen  *rigStatus // set on Enter; acted on after the program exits
	width   int
	scanErr error
}

type radarScanMsg struct {
	statuses []rigStatus
	attached map[string]int64
	err      error
}

type radarPRsMsg struct {
	slug string
	prs  []rigPR
}

type radarTickMsg time.Time

// radarTickEvery is how often local state (sessions, agent recency) re-scans.
const radarTickEvery = 2 * time.Second

func radarScanNow(home string) radarScanMsg {
	rigs, err := listRigs()
	if err != nil {
		return radarScanMsg{err: err}
	}
	return radarScanMsg{
		statuses: rigStatuses(rigs, home, time.Now()),
		attached: tmuxLastAttached(),
	}
}

func radarScanCmd(home string) tea.Cmd {
	return func() tea.Msg { return radarScanNow(home) }
}

// radarFetchCmd resolves one rig's PRs via the shared enrichment fan-out, so
// each cell fills in as its own gh calls land rather than waiting on the
// slowest rig.
func radarFetchCmd(s rigStatus) tea.Cmd {
	return func() tea.Msg {
		one := []rigStatus{s}
		enrichWithPRs(one)
		return radarPRsMsg{s.Slug, one[0].PRs}
	}
}

func radarTickCmd() tea.Cmd {
	return tea.Tick(radarTickEvery, func(t time.Time) tea.Msg { return radarTickMsg(t) })
}

func (m radarModel) Init() tea.Cmd {
	return tea.Batch(append(m.fetchMissing(), radarTickCmd())...)
}

// fetchMissing fires a PR fetch for every rig whose slug hasn't been fetched
// and isn't in flight. pending is a shared map, so marking here is visible to
// future Update calls even from the value-receiver Init.
func (m radarModel) fetchMissing() []tea.Cmd {
	var cmds []tea.Cmd
	for _, s := range m.rows() {
		if _, done := m.prs[s.Slug]; done || m.pending[s.Slug] {
			continue
		}
		m.pending[s.Slug] = true
		cmds = append(cmds, radarFetchCmd(s))
	}
	return cmds
}

func (m radarModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case tea.KeyMsg:
		// A fast burst of keypresses (key repeat, or synthetic send-keys)
		// coalesces into one multi-rune KeyMsg; unpack it so held-down j
		// still walks the cursor one row per press.
		keys := []string{msg.String()}
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 {
			keys = keys[:0]
			for _, r := range msg.Runes {
				keys = append(keys, string(r))
			}
		}
		var cmds []tea.Cmd
		for _, k := range keys {
			var cmd tea.Cmd
			m, cmd = m.handleKey(k)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)

	case radarTickMsg:
		return m, tea.Batch(radarScanCmd(m.home), radarTickCmd())

	case radarScanMsg:
		m.apply(msg)
		// A rig created (or parked) while the radar is open gets its PR
		// fetch on the scan that first sees it.
		return m, tea.Batch(m.fetchMissing()...)

	case radarPRsMsg:
		m.prs[msg.slug] = msg.prs
		delete(m.pending, msg.slug)
		m.resort()
		return m, nil
	}
	return m, nil
}

func (m radarModel) handleKey(key string) (radarModel, tea.Cmd) {
	switch key {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	case "j", "down":
		if m.cursor < len(m.rows())-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "enter":
		rows := m.rows()
		if len(rows) == 0 {
			return m, nil
		}
		s := rows[m.cursor]
		m.chosen = &s
		return m, tea.Quit
	case "r":
		// Refetch every rig's PRs; stale cells keep showing until the
		// fresh answer lands rather than flashing back to "…".
		var cmds []tea.Cmd
		for _, s := range m.rows() {
			if m.pending[s.Slug] {
				continue
			}
			m.pending[s.Slug] = true
			cmds = append(cmds, radarFetchCmd(s))
		}
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

// apply folds a fresh local scan into the model: split sections, merge the PR
// cache back in (scans return bare statuses), re-sort, and keep the cursor on
// the same rig it was on.
func (m *radarModel) apply(scan radarScanMsg) {
	if scan.err != nil {
		m.scanErr = scan.err
		return
	}
	m.scanErr = nil
	m.attached = scan.attached
	// The selection to preserve is what the user is looking at now, so read
	// it before the sections are replaced.
	selected := m.selectedSlug()

	var inflight, parked []rigStatus
	for _, s := range scan.statuses {
		if prs, ok := m.prs[s.Slug]; ok {
			s.PRs = prs
		}
		switch {
		case s.Parked:
			parked = append(parked, s)
		case m.current != "" && tmuxSessionName(s.Path) == m.current:
			// You never switch to where you already are.
		default:
			inflight = append(inflight, s)
		}
	}
	m.inflight, m.parked = inflight, parked
	m.resortKeeping(selected)
}

// selectedSlug is the slug under the cursor, or "" when there's nothing to
// point at (first render).
func (m *radarModel) selectedSlug() string {
	if rows := m.rows(); m.cursor < len(rows) {
		return rows[m.cursor].Slug
	}
	return ""
}

// resort re-sorts both sections and keeps the cursor on the rig it's on, so a
// rig climbing the board doesn't yank the selection off it.
func (m *radarModel) resort() {
	m.resortKeeping(m.selectedSlug())
}

func (m *radarModel) resortKeeping(selected string) {
	radarSortInflight(m.inflight, m.attached)
	radarSortParked(m.parked, m.prs)

	rows := m.rows()
	m.cursor = 0
	for i, s := range rows {
		if s.Slug == selected {
			m.cursor = i
			break
		}
	}
	if m.cursor >= len(rows) {
		m.cursor = max(0, len(rows)-1)
	}
}

// rows flattens the two sections in display order: the cursor walks in-flight
// then parked as one list.
func (m radarModel) rows() []rigStatus {
	out := make([]rigStatus, 0, len(m.inflight)+len(m.parked))
	out = append(out, m.inflight...)
	out = append(out, m.parked...)
	return out
}

// radarSortInflight orders live rigs the way switch does: most-recently-
// attached first, sessionless rigs sinking, ties broken newest-created.
func radarSortInflight(statuses []rigStatus, attached map[string]int64) {
	sort.SliceStable(statuses, func(i, j int) bool {
		ai := attached[tmuxSessionName(statuses[i].Path)]
		aj := attached[tmuxSessionName(statuses[j].Path)]
		if ai != aj {
			return ai > aj
		}
		return statuses[i].Created.After(statuses[j].Created)
	})
}

// radarSortParked orders parked rigs the way waiting does: most-actionable
// disposition first, oldest-created within a bucket. Rigs whose PR fetch
// hasn't landed sink to the bottom until it does.
func radarSortParked(statuses []rigStatus, prs map[string][]rigPR) {
	sort.SliceStable(statuses, func(i, j int) bool {
		ri := dispRank(radarStateCell(statuses[i], prsFetched(prs, statuses[i].Slug)))
		rj := dispRank(radarStateCell(statuses[j], prsFetched(prs, statuses[j].Slug)))
		if ri != rj {
			return ri < rj
		}
		return statuses[i].Created.Before(statuses[j].Created)
	})
}

func prsFetched(prs map[string][]rigPR, slug string) bool {
	_, ok := prs[slug]
	return ok
}

// radarStateCell is the state column: live agent state for an in-flight rig,
// review disposition for a parked one ("…" while the fetch is still out).
func radarStateCell(s rigStatus, fetched bool) string {
	if s.Parked {
		if !fetched {
			return "…"
		}
		return parkedDisposition(s.PRs)
	}
	return agentMarker(s)
}

// radarPRCell is the PR column, "…" until that rig's fan-out lands.
func radarPRCell(s rigStatus, fetched bool) string {
	if !fetched {
		return "…"
	}
	return prMarker(s)
}

var (
	radarHeaderStyle = lipgloss.NewStyle().Bold(true).Faint(true)
	radarCursorStyle = lipgloss.NewStyle().Reverse(true)
	radarFaintStyle  = lipgloss.NewStyle().Faint(true)
	radarGoodStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	radarHotStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	radarDoneStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	radarErrStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Faint(true)
)

// radarStateStyle colors the state cell by urgency: red for what wants you
// (changes requested), green for live/mergeable, magenta for done, faint for
// everything quiet.
func radarStateStyle(state string) lipgloss.Style {
	switch state {
	case "working", "approved":
		return radarGoodStyle
	case "changes requested":
		return radarHotStyle
	case "merged":
		return radarDoneStyle
	default:
		return radarFaintStyle
	}
}

func (m radarModel) View() string {
	rows := m.rows()
	if len(rows) == 0 {
		return "\n  no rigs in flight\n\n" + radarFaintStyle.Render("  q quit") + "\n"
	}

	// Cells are assembled for every row first so both sections share column
	// widths and the board reads as one table.
	type cells struct{ id, age, state, pr string }
	cs := make([]cells, len(rows))
	var wID, wAge, wState, wPR int
	for i, s := range rows {
		c := cells{s.ID, age(s.Created), radarStateCell(s, prsFetched(m.prs, s.Slug)), radarPRCell(s, prsFetched(m.prs, s.Slug))}
		cs[i] = c
		wID = max(wID, lipgloss.Width(c.id))
		wAge = max(wAge, lipgloss.Width(c.age))
		wState = max(wState, lipgloss.Width(c.state))
		wPR = max(wPR, lipgloss.Width(c.pr))
	}

	var b strings.Builder
	line := func(i int, s rigStatus) {
		c := cs[i]
		title := s.Title
		if m.width > 0 {
			// Fixed columns plus the "▸ " gutter and three 2-space gaps.
			title = radarTruncate(title, m.width-(2+wID+2+wAge+2+wState+2+wPR+2))
		}
		if i == m.cursor {
			// One style over the whole line: inner color resets would chew
			// through a wrapping reverse, so the selected row goes plain.
			plain := fmt.Sprintf("▸ %-*s  %-*s  %-*s  %-*s  %s", wID, c.id, wAge, c.age, wState, c.state, wPR, c.pr, title)
			b.WriteString(radarCursorStyle.Render(plain))
		} else {
			fmt.Fprintf(&b, "  %-*s  %-*s  %s  %-*s  %s",
				wID, c.id, wAge, c.age,
				radarStateStyle(c.state).Render(fmt.Sprintf("%-*s", wState, c.state)),
				wPR, c.pr, title)
		}
		b.WriteString("\n")
	}

	if len(m.inflight) > 0 {
		b.WriteString(radarHeaderStyle.Render("IN FLIGHT") + "\n")
		for i := range m.inflight {
			line(i, m.inflight[i])
		}
	}
	if len(m.parked) > 0 {
		if len(m.inflight) > 0 {
			b.WriteString("\n")
		}
		b.WriteString(radarHeaderStyle.Render("PARKED · AWAITING REVIEW") + "\n")
		for i := range m.parked {
			line(len(m.inflight)+i, m.parked[i])
		}
	}

	b.WriteString("\n" + radarFaintStyle.Render("↑/↓ move · enter go · r refresh prs · q quit"))
	if m.scanErr != nil {
		b.WriteString("\n" + radarErrStyle.Render("scan: "+m.scanErr.Error()))
	}
	return b.String()
}

// radarTruncate clips s to width cells (rune-counted — close enough for issue
// titles) with an ellipsis.
func radarTruncate(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}
