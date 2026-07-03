package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		home:      home,
		current:   currentTmuxSession(),
		prs:       map[string][]rigPR{},
		fetchedAt: map[string]time.Time{},
		pending:   map[string]bool{},
	}
	// Seed the PR cells from the on-disk cache: entries inside the TTL skip
	// their gh round-trip entirely (the radar gets popped several times a
	// minute), stale ones still paint instantly and refetch behind the render.
	for slug, e := range loadRadarCache() {
		m.prs[slug] = e.PRs
		m.fetchedAt[slug] = e.At
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

	inflight  []rigStatus
	parked    []rigStatus
	attached  map[string]int64 // session → last-attached, for in-flight order
	prs       map[string][]rigPR
	fetchedAt map[string]time.Time // slug → when its PRs were fetched
	pending   map[string]bool      // slug → PR fetch in flight

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

// radarTailReserve is how many columns long titles must leave for the PR
// tail, so the detail isn't starved off the right edge of the popup. Sized
// for a two-repo tail ("rfd #143   runtime #880 ") or a parked
// disposition plus PR ("changes requested  #877 ").
const radarTailReserve = 30

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

// radarPRTTL is how long a cached PR answer is trusted before the radar
// refetches it in the background. `r` is the backstop for anything staler
// than you can stand.
const radarPRTTL = 5 * time.Minute

// fetchMissing fires a PR fetch for every rig whose answer is missing or past
// the TTL and isn't already in flight. pending is a shared map, so marking
// here is visible to future Update calls even from the value-receiver Init.
func (m radarModel) fetchMissing() []tea.Cmd {
	now := time.Now()
	var cmds []tea.Cmd
	for _, s := range m.rows() {
		if m.pending[s.Slug] {
			continue
		}
		if at, ok := m.fetchedAt[s.Slug]; ok && now.Sub(at) < radarPRTTL {
			continue
		}
		m.pending[s.Slug] = true
		cmds = append(cmds, radarFetchCmd(s))
	}
	return cmds
}

// radarCacheEntry is one rig's cached PR answer in ~/.cache/rig/radar-prs.json.
type radarCacheEntry struct {
	At  time.Time `json:"at"`
	PRs []rigPR   `json:"prs,omitempty"`
}

func radarCachePath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "rig", "radar-prs.json"), nil
}

// loadRadarCache reads the PR cache, degrading to empty on any trouble — the
// cache is purely an accelerator, never a source of errors.
func loadRadarCache() map[string]radarCacheEntry {
	path, err := radarCachePath()
	if err != nil {
		return nil
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries map[string]radarCacheEntry
	if err := json.Unmarshal(blob, &entries); err != nil {
		return nil
	}
	return entries
}

// saveRadarCache writes the current PR answers back, atomically (write +
// rename) so a radar killed mid-write can't leave a torn file. Entries older
// than an hour are dropped: they'd be refetched anyway and this keeps the
// file from accreting rigs long since torn down.
func saveRadarCache(prs map[string][]rigPR, fetchedAt map[string]time.Time) {
	path, err := radarCachePath()
	if err != nil {
		return
	}
	entries := make(map[string]radarCacheEntry)
	for slug, at := range fetchedAt {
		if time.Since(at) > time.Hour {
			continue
		}
		entries[slug] = radarCacheEntry{At: at, PRs: prs[slug]}
	}
	blob, err := json.Marshal(entries)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
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
		m.fetchedAt[msg.slug] = time.Now()
		delete(m.pending, msg.slug)
		saveRadarCache(m.prs, m.fetchedAt)
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

var (
	radarHeaderStyle = lipgloss.NewStyle().Bold(true).Faint(true)
	radarCursorStyle = lipgloss.NewStyle().Reverse(true)
	radarFaintStyle  = lipgloss.NewStyle().Faint(true)
	radarGoodStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	radarHotStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	radarWarnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	radarDoneStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	radarErrStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Faint(true)
)

// radarStateStyle colors a state word by urgency: red for what wants you
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

// radarGlyph is the fixed-width status cell: one glyph whose shape and color
// carry the row's state, so the PR fan-out landing never shifts the layout.
// In-flight rigs read agent attention; parked rigs read review disposition.
// Glyphs are conservative nerd-font codepoints (classic FA + octicons).
func radarGlyph(s rigStatus, fetched bool) (string, lipgloss.Style) {
	if s.Parked {
		if !fetched {
			return "…", radarFaintStyle
		}
		switch parkedDisposition(s.PRs) {
		case "changes requested":
			return "", radarHotStyle // comment
		case "approved":
			return "", radarGoodStyle // check
		case "merged":
			return "", radarDoneStyle // git merge
		case "no PR":
			return "·", radarFaintStyle
		default: // waiting
			return "", radarFaintStyle // clock
		}
	}
	switch s.Agent {
	case "working":
		return "●", radarGoodStyle
	case "idle":
		return "●", radarFaintStyle
	default:
		return "·", radarFaintStyle
	}
}

// radarChecksGlyph is the per-PR CI glyph for the tail.
func radarChecksGlyph(checks string) (string, lipgloss.Style) {
	switch checks {
	case "passing":
		return "", radarGoodStyle
	case "failing":
		return "", radarHotStyle
	case "pending":
		return "", radarWarnStyle
	default:
		return "", radarFaintStyle
	}
}

// radarPRNumStyle colors a PR number by its state; open stays faint because
// the tail is detail, not the headline.
func radarPRNumStyle(state string) lipgloss.Style {
	switch state {
	case "MERGED":
		return radarDoneStyle
	case "CLOSED":
		return radarHotStyle
	default:
		return radarFaintStyle
	}
}

// tailSeg is one pre-styled piece of a row's right-hand tail, carrying its
// plain form so width math and the selected row's unstyled render stay honest.
type tailSeg struct {
	plain  string
	styled string
}

// radarTailSegs builds the append-only detail that trails a row: for a parked
// rig the disposition spelled out (the glyph's legend rides along), then each
// PR as number + CI glyph, repo-prefixed when the rig spans repos. Loading
// reads as a bare "…"; an in-flight rig with no PR trails nothing at all.
func radarTailSegs(s rigStatus, fetched bool) []tailSeg {
	if !fetched {
		return []tailSeg{{"…", radarFaintStyle.Render("…")}}
	}
	var segs []tailSeg
	if s.Parked {
		disp := parkedDisposition(s.PRs)
		segs = append(segs, tailSeg{disp, radarStateStyle(disp).Render(disp)})
	}
	multi := len(s.PRs) > 1
	for _, pr := range s.PRs {
		plain := fmt.Sprintf("#%d", pr.Number)
		if multi {
			plain = shortRepo(pr.Repo) + " " + plain
		}
		styled := radarPRNumStyle(pr.State).Render(plain)
		if g, st := radarChecksGlyph(pr.Checks); g != "" {
			plain += " " + g
			styled += " " + st.Render(g)
		}
		segs = append(segs, tailSeg{plain, styled})
	}
	return segs
}

func (m radarModel) View() string {
	rows := m.rows()
	if len(rows) == 0 {
		return "\n  no rigs in flight\n\n" + radarFaintStyle.Render("  q quit") + "\n"
	}

	// Column widths come from local-only cells (id, age, titles), so the
	// layout is stable from the first frame: the PR fan-out landing swaps a
	// glyph in place and appends a tail, it never moves a column.
	var wID, wAge, wTitle int
	for _, s := range rows {
		wID = max(wID, lipgloss.Width(s.ID))
		wAge = max(wAge, lipgloss.Width(age(s.Created)))
		wTitle = max(wTitle, lipgloss.Width(s.Title))
	}
	fixed := 2 + wID + 2 + wAge + 2 + 1 + 2 // gutter, id, age, glyph, gaps
	if m.width > 0 {
		wTitle = min(wTitle, max(20, m.width-fixed-radarTailReserve))
	}

	var b strings.Builder
	line := func(i int, s rigStatus) {
		fetched := prsFetched(m.prs, s.Slug)
		glyph, gstyle := radarGlyph(s, fetched)
		title := radarTruncate(s.Title, wTitle)

		// The tail takes whatever the line has left; segments append whole
		// or not at all, so a squeezed row drops detail instead of tearing.
		avail := int(^uint(0) >> 1)
		if m.width > 0 {
			avail = m.width - fixed - wTitle - 2
		}
		var plainTail, styledTail []string
		used := 0
		for _, seg := range radarTailSegs(s, fetched) {
			w := lipgloss.Width(seg.plain)
			gap := 0
			if len(plainTail) > 0 {
				gap = 2
			}
			if used+gap+w > avail {
				break
			}
			used += gap + w
			plainTail = append(plainTail, seg.plain)
			styledTail = append(styledTail, seg.styled)
		}

		if i == m.cursor {
			// One style over the whole line: inner color resets would chew
			// through a wrapping reverse, so the selected row goes plain.
			plain := fmt.Sprintf("▸ %-*s  %-*s  %s  %-*s  %s",
				wID, s.ID, wAge, age(s.Created), glyph, wTitle, title, strings.Join(plainTail, "  "))
			b.WriteString(radarCursorStyle.Render(strings.TrimRight(plain, " ")))
		} else {
			fmt.Fprintf(&b, "  %-*s  %-*s  %s  %-*s  %s",
				wID, s.ID, wAge, age(s.Created), gstyle.Render(glyph),
				wTitle, title, strings.Join(styledTail, "  "))
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
