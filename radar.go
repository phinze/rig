package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

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
//
// It doubles as the universal picker that replaces tmux-session-wizard. Below
// the rigs sits an OTHER SESSIONS section listing every non-rig tmux session in
// MRU order. The board is modal like k9s: bare letters are verbs (n opens a NEW
// picker that stands up a session at a zoxide dir, R refreshes, q quits), and
// `/` drops into a fuzzy filter that ranks the whole board best-match-first. The
// NEW picker is where `rig up` and `rig review` will grow their own sources.
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

	final, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
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
	// A bare session is nothing but a name to land in; a child is the same, its
	// session field a session:index window target. No manifest, no session to
	// stand up, no PR to wake.
	if s.bare || s.child {
		return attachOrReport(s.session)
	}
	// A create-row stands up a fresh session at a zoxide dir; promoting its
	// frecency first keeps the picker's order honest next time.
	if s.create {
		zoxideAdd(s.Path)
	}
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
	sessions  []rigStatus      // bare (non-rig) tmux sessions, MRU order
	attached  map[string]int64 // session → last-attached, for in-flight order
	prs       map[string][]rigPR
	fetchedAt map[string]time.Time // slug → when its PRs were fetched
	pending   map[string]bool      // slug → PR fetch in flight

	mode    radarMode
	newDirs []string // zoxide frecency list, fetched when the NEW picker opens
	filter  string   // fuzzy query; empty = show everything
	cursor  int
	chosen  *rigStatus // set on Enter; acted on after the program exits
	width   int
	height  int
	scanErr error
}

// radarMode is the picker's input mode. The board defaults to command keys
// (letters are verbs); a slash drops into filter entry where letters are text;
// n opens the NEW picker, a create-a-session view that's always filter-entry.
type radarMode int

const (
	modeBoard       radarMode = iota // sections, command keys, / to filter
	modeBoardFilter                  // board narrowed by a live fuzzy query
	modeNew                          // NEW picker: zoxide dirs → fresh session
)

type radarScanMsg struct {
	statuses []rigStatus
	sessions []tmuxSession
	attached map[string]int64
	agents   map[string][]agentChild // session name → its claude windows
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
	// One list-sessions feeds both the in-flight attach-order map and the bare
	// session rows, so the universal picker costs the same tmux round-trip the
	// board already paid.
	sessions := tmuxSessions()
	attached := make(map[string]int64, len(sessions))
	for _, s := range sessions {
		attached[s.Name] = s.LastAttached
	}
	return radarScanMsg{
		statuses: rigStatuses(rigs, home, time.Now()),
		sessions: sessions,
		attached: attached,
		agents:   tmuxAgentChildren(),
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
	for _, s := range m.rigRows() {
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
		m.height = msg.Height
		return m, nil

	case tea.MouseMsg:
		// Basic mouse: the wheel walks the cursor, a left click selects the row
		// under the pointer, and clicking the already-selected row activates it —
		// so a double-click reads as select-then-go. Release and motion events
		// are ignored so only the press acts.
		if msg.Action != tea.MouseActionPress {
			return m, nil
		}
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if m.cursor > 0 {
				m.cursor--
			}
		case tea.MouseButtonWheelDown:
			if m.cursor < len(m.rows())-1 {
				m.cursor++
			}
		case tea.MouseButtonLeft:
			if cur, ok := m.rowAtY(msg.Y); ok {
				if cur == m.cursor {
					rows := m.rows()
					s := rows[cur]
					m.chosen = &s
					return m, tea.Quit
				}
				m.cursor = cur
			}
		}
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

// handleKey drives the picker in always-on filter mode, the way session-wizard's
// fzf did. The board proper is modal — letters are verbs (n new, R refresh, q
// quit), and a slash drops into filter entry — while the filter and NEW modes
// are pure text entry, where letters narrow the list and esc walks back out.
func (m radarModel) handleKey(key string) (radarModel, tea.Cmd) {
	// Keys shared by every mode: move, select, hard-quit.
	switch key {
	case "ctrl+c":
		return m, tea.Quit
	case "down", "ctrl+n":
		if m.cursor < len(m.rows())-1 {
			m.cursor++
		}
		return m, nil
	case "up", "ctrl+p":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "enter":
		rows := m.rows()
		if len(rows) == 0 {
			return m, nil
		}
		s := rows[m.cursor]
		m.chosen = &s
		return m, tea.Quit
	}
	if m.mode == modeBoard {
		return m.handleBoardKey(key)
	}
	return m.handleTypingKey(key)
}

// handleBoardKey runs the command-mode board: bare letters are verbs, so the
// keymap reads like a launcher rather than a search box.
func (m radarModel) handleBoardKey(key string) (radarModel, tea.Cmd) {
	switch key {
	case "q", "esc":
		return m, tea.Quit
	case "j":
		if m.cursor < len(m.rows())-1 {
			m.cursor++
		}
	case "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "/":
		m.enter(modeBoardFilter)
	case "n":
		// Fetch the frecency list once, when the picker opens, so the board's
		// 2s tick never pays for it.
		m.newDirs = zoxideDirs()
		m.enter(modeNew)
	case "R":
		// Refetch every rig's PRs; stale cells keep showing until the fresh
		// answer lands rather than flashing back to "…".
		var cmds []tea.Cmd
		for _, s := range m.rigRows() {
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

// handleTypingKey runs the filter and NEW modes: printable runes narrow the
// list, backspace trims, esc walks back to the board.
func (m radarModel) handleTypingKey(key string) (radarModel, tea.Cmd) {
	switch key {
	case "esc":
		m.enter(modeBoard)
	case "ctrl+u":
		m.setFilter("")
	case "backspace":
		if r := []rune(m.filter); len(r) > 0 {
			m.setFilter(string(r[:len(r)-1]))
		}
	default:
		// Any lone printable rune is filter input. Named keys (tab, f-keys)
		// arrive as multi-rune strings and fall through untouched.
		if r := []rune(key); len(r) == 1 && unicode.IsGraphic(r[0]) {
			m.setFilter(m.filter + key)
		}
	}
	return m, nil
}

// enter switches modes with a clean slate: the query resets and the cursor
// snaps to the top so each mode opens on its best/first row.
func (m *radarModel) enter(mode radarMode) {
	m.mode = mode
	m.filter = ""
	m.cursor = 0
}

// setFilter changes the query and snaps the cursor to the top row, which under
// an active filter is the best-scoring match — as you type, the selection rides
// the strongest hit the way fzf does.
func (m *radarModel) setFilter(q string) {
	m.filter = q
	m.cursor = 0
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
	selected := m.selectedKey()

	// The rig session names are the ones the bare-session pass must exclude, so
	// a rig never shows up twice (once as itself, once as a plain session).
	rigSessions := make(map[string]bool, len(scan.statuses))
	var inflight, parked []rigStatus
	for _, s := range scan.statuses {
		rigSessions[tmuxSessionName(s.Path)] = true
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

	var sessions []rigStatus
	for _, ts := range scan.sessions {
		if rigSessions[ts.Name] || ts.Name == m.current {
			continue
		}
		sessions = append(sessions, bareSession(ts, m.home))
	}

	// Dangle each parent's live claude windows under it: rigs key off their
	// computed session name, bare sessions off their own. A row with no agent
	// running just gets no children.
	attachAgents := func(rows []rigStatus, sessionOf func(rigStatus) string) {
		for i := range rows {
			rows[i].agents = scan.agents[sessionOf(rows[i])]
		}
	}
	rigSession := func(s rigStatus) string { return tmuxSessionName(s.Path) }
	attachAgents(inflight, rigSession)
	attachAgents(parked, rigSession)
	attachAgents(sessions, func(s rigStatus) string { return s.session })

	m.inflight, m.parked, m.sessions = inflight, parked, sessions
	m.resortKeeping(selected)
}

// bareSession turns a plain tmux session into a radar row: its working
// directory (home-relativized) reads as the title, and its last-attached time
// stands in for Created so the age column shows how long since you were there
// and MRU sorting falls out of the same field the rigs use.
func bareSession(ts tmuxSession, home string) rigStatus {
	var created time.Time
	if ts.LastAttached > 0 {
		created = time.Unix(ts.LastAttached, 0)
	}
	title := ts.Name
	if ts.Path != "" {
		title = tildePath(ts.Path, home)
	}
	return rigStatus{
		Title:   title,
		Path:    ts.Path,
		Created: created,
		bare:    true,
		session: ts.Name,
	}
}

// tildePath shortens an absolute path under $HOME to a leading ~ for display.
func tildePath(path, home string) string {
	if home != "" {
		if rel, ok := strings.CutPrefix(path, home); ok {
			return "~" + rel
		}
	}
	return path
}

// rowKey is a row's stable identity across a re-sort or re-scan: a rig is its
// slug, a bare session its name. Used to keep the cursor pinned to the same row
// even as the board reorders under it.
func rowKey(s rigStatus) string {
	switch {
	case s.child:
		return "child:" + s.session // session holds the window target
	case s.bare:
		return "sess:" + s.session
	default:
		return "slug:" + s.Slug
	}
}

// selectedKey is the identity of the row under the cursor, or "" when there's
// nothing to point at (first render).
func (m *radarModel) selectedKey() string {
	if rows := m.rows(); m.cursor < len(rows) {
		return rowKey(rows[m.cursor])
	}
	return ""
}

// resort re-sorts every section and keeps the cursor on the row it's on, so a
// rig climbing the board doesn't yank the selection off it.
func (m *radarModel) resort() {
	m.resortKeeping(m.selectedKey())
}

func (m *radarModel) resortKeeping(selected string) {
	radarSortInflight(m.inflight, m.attached)
	radarSortParked(m.parked, m.prs)
	radarSortSessions(m.sessions)

	rows := m.rows()
	m.cursor = 0
	for i, s := range rows {
		if rowKey(s) == selected {
			m.cursor = i
			break
		}
	}
	if m.cursor >= len(rows) {
		m.cursor = max(0, len(rows)-1)
	}
}

// rigRows is the two rig sections flattened, unfiltered: the PR fan-out enriches
// every rig regardless of what the filter is currently hiding.
func (m radarModel) rigRows() []rigStatus {
	out := make([]rigStatus, 0, len(m.inflight)+len(m.parked))
	out = append(out, m.inflight...)
	out = append(out, m.parked...)
	return out
}

// radarLine is one line the board draws: a section header (not selectable) or a
// row. A row can be a parent or a dangled child; last marks the final child of a
// parent so the tree closes with └ instead of ├.
type radarLine struct {
	header string
	row    rigStatus
	child  bool
	last   bool
}

// displayItems is the single ordered list both the cursor and the renderer read,
// so the two never drift. In board mode it's the three sections with each
// parent's live claude windows dangled beneath it; under a filter it collapses
// to one ranked list of parents (children are the resting HUD, not part of the
// hunt); the NEW picker is its own header plus create-rows.
func (m radarModel) displayItems() []radarLine {
	var items []radarLine
	parent := func(p rigStatus) {
		items = append(items, radarLine{row: p})
		// Label children by window only when they span more than one — a repo
		// name places the runtime agent vs the rfd agent, but two agents in one
		// window are told apart by their context, so the repeated name is noise.
		windows := map[string]bool{}
		for _, c := range p.agents {
			windows[c.Window] = true
		}
		showLabel := len(windows) > 1
		for i, c := range p.agents {
			key := ""
			if showLabel {
				key = c.Window
			}
			items = append(items, radarLine{
				row:   rigStatus{child: true, session: c.Target, Title: c.Context, childKey: key},
				child: true,
				last:  i == len(p.agents)-1,
			})
		}
	}
	switch m.mode {
	case modeNew:
		items = append(items, radarLine{header: "NEW SESSION"})
		for _, s := range m.rankRows(m.newRows()) {
			items = append(items, radarLine{row: s})
		}
	case modeBoardFilter:
		for _, s := range m.rankRows(m.rigRows(), m.sessions) {
			items = append(items, radarLine{row: s})
		}
	default: // modeBoard
		section := func(header string, parents []rigStatus) {
			if len(parents) == 0 {
				return
			}
			items = append(items, radarLine{header: header})
			for _, p := range parents {
				parent(p)
			}
		}
		section("IN FLIGHT", m.inflight)
		section("PARKED · AWAITING REVIEW", m.parked)
		section("OTHER SESSIONS", m.sessions)
	}
	return items
}

// screenLine is one rendered row of the board: a blank separator, a section
// header, or a display item, tagged with the selectable cursor index it maps to
// (-1 when it can't be landed on). View renders from this and the mouse handler
// hit-tests against it, so the two share one notion of what sits on each line.
type screenLine struct {
	item   radarLine
	blank  bool
	cursor int
}

// boardLines expands the display items into the exact sequence of screen lines,
// blank section-separators included, numbering the selectable ones. len(result)
// is precisely how many lines View emits.
func (m radarModel) boardLines() []screenLine {
	var lines []screenLine
	sel := 0
	for _, it := range m.displayItems() {
		if it.header != "" {
			if len(lines) > 0 {
				lines = append(lines, screenLine{blank: true, cursor: -1})
			}
			lines = append(lines, screenLine{item: it, cursor: -1})
			continue
		}
		lines = append(lines, screenLine{item: it, cursor: sel})
		sel++
	}
	return lines
}

// rows is the selectable list the cursor walks: every display item that isn't a
// header, parents and dangled children alike.
func (m radarModel) rows() []rigStatus {
	items := m.displayItems()
	out := make([]rigStatus, 0, len(items))
	for _, it := range items {
		if it.header == "" {
			out = append(out, it.row)
		}
	}
	return out
}

// rankRows scores the given sections against the query, drops the misses, and
// sorts survivors by score descending. With no query it returns the sections
// concatenated in order, untouched. Ties keep that order (each section is
// already MRU/urgency/frecency-sorted), so equally-good matches still fall out
// sensibly.
func (m radarModel) rankRows(sections ...[]rigStatus) []rigStatus {
	if m.filter == "" {
		var out []rigStatus
		for _, sec := range sections {
			out = append(out, sec...)
		}
		return out
	}
	type scored struct {
		s     rigStatus
		score float64
	}
	var xs []scored
	for _, sec := range sections {
		for _, s := range sec {
			if score, ok := fuzzyScore(m.filter, radarHaystack(s)); ok {
				xs = append(xs, scored{s, score})
			}
		}
	}
	sort.SliceStable(xs, func(i, j int) bool { return xs[i].score > xs[j].score })

	out := make([]rigStatus, len(xs))
	for i, x := range xs {
		out[i] = x.s
	}
	return out
}

// newRows builds the NEW picker's create-rows from the zoxide frecency list:
// one row per dir that doesn't already have a session (rig, bare session, or
// the current one), since a dir you're already in belongs on the board, not
// here. Enter on one stands up a session at that dir.
func (m radarModel) newRows() []rigStatus {
	open := m.openSessions()
	var out []rigStatus
	for _, dir := range m.newDirs {
		if open[tmuxSessionName(dir)] {
			continue
		}
		out = append(out, rigStatus{
			create: true,
			Path:   dir,
			Title:  tildePath(dir, m.home),
		})
	}
	return out
}

// openSessions is the set of tmux session names already represented on the
// board — every rig's computed name, every bare session, and the current one —
// so the NEW picker can skip dirs you can already reach.
func (m radarModel) openSessions() map[string]bool {
	open := make(map[string]bool)
	for _, s := range m.rigRows() {
		open[tmuxSessionName(s.Path)] = true
	}
	for _, s := range m.sessions {
		open[s.session] = true
	}
	if m.current != "" {
		open[m.current] = true
	}
	return open
}

// agentPlaceholder is Claude Code's default title before a task is named — the
// tell that an agent is open but idle-blank rather than mid-task.
const agentPlaceholder = "Claude Code"

// stripAgentGlyph peels Claude Code's leading state glyph off a pane title,
// leaving just the task text. The glyph is either ✳ (the resting star) or a
// braille spinner frame (U+2800–U+28FF) captured mid-animation; either way it's
// one rune plus a space. A title with no such glyph comes back unchanged, which
// is also how callers tell an agent pane from a plain shell.
func stripAgentGlyph(title string) string {
	r := []rune(title)
	if len(r) == 0 {
		return title
	}
	if r[0] == '✳' || (r[0] >= 0x2800 && r[0] <= 0x28FF) {
		return strings.TrimSpace(string(r[1:]))
	}
	return title
}

// radarHaystack is the text a row is fuzzy-matched against: a rig by its id and
// title, a bare session or create-row by its path-title (plus the session name
// for a bare row, since that's what you'd half-remember typing).
func radarHaystack(s rigStatus) string {
	if s.bare {
		return s.Title + " " + s.session
	}
	return s.ID + " " + s.Title
}

// fuzzyMatch reports whether a row survives the query at all (used where only
// presence matters). Matching is the score's yes/no: every space-separated term
// must be a case-insensitive subsequence of the haystack.
func fuzzyMatch(query, hay string) bool {
	_, ok := fuzzyScore(query, hay)
	return ok
}

// fuzzyScore rates how well a query fits a haystack, fzy-style. The query splits
// on spaces into terms; every term must match (AND), and the total is the sum of
// each term's best alignment score. A higher number is a tighter, more
// boundary-aligned match. The bool is false the moment any term fails to match.
func fuzzyScore(query, hay string) (float64, bool) {
	hay = strings.ToLower(hay)
	total := 0.0
	matched := false
	for term := range strings.FieldsSeq(strings.ToLower(query)) {
		matched = true
		score, ok := matchScore(term, hay)
		if !ok {
			return 0, false
		}
		total += score
	}
	if !matched {
		return 0, true // empty query matches everything, score-neutral
	}
	return total, true
}

// Fuzzy scoring weights, lifted from fzy: consecutive matched runes and matches
// landing on a boundary (after a path slash, a word separator, a dot) score
// well; gaps cost a little, leading gaps least. Tuned so "runtime" beats a
// scattered r-u-n-t-i-m-e strewn across an unrelated path.
const (
	scoreGapLeading       = -0.005
	scoreGapTrailing      = -0.005
	scoreGapInner         = -0.01
	scoreMatchConsecutive = 1.0
	scoreMatchSlash       = 0.9
	scoreMatchWord        = 0.8
	scoreMatchDot         = 0.6
)

// matchBonus is the boundary bonus a matched rune earns from the rune before it:
// the char after a slash, separator, or dot reads as the start of a segment and
// is what people usually aim at. Inputs are already lowercased.
func matchBonus(prev rune) float64 {
	switch prev {
	case '/':
		return scoreMatchSlash
	case '-', '_', ' ':
		return scoreMatchWord
	case '.':
		return scoreMatchDot
	default:
		return 0
	}
}

// matchScore aligns needle against hay with a two-matrix DP (fzy's): D[i][j] is
// the best score ending with needle[i] on hay[j], M[i][j] the best over
// hay[0..j]. Returns (score, true) or (0, false) when needle isn't a
// subsequence at all. Cost is O(len(needle)·len(hay)); both are short here.
func matchScore(needle, hay string) (float64, bool) {
	nr, hr := []rune(needle), []rune(hay)
	n, m := len(nr), len(hr)
	if n == 0 {
		return 0, true
	}
	if n > m {
		return 0, false
	}

	// Boundary bonus per haystack position; the string start counts as a slash
	// so a leading match is treated as a segment start.
	bonus := make([]float64, m)
	prev := '/'
	for j, c := range hr {
		bonus[j] = matchBonus(prev)
		prev = c
	}

	negInf := math.Inf(-1)
	D := make([][]float64, n)
	M := make([][]float64, n)
	for i := range D {
		D[i] = make([]float64, m)
		M[i] = make([]float64, m)
	}
	for i := range n {
		prevScore := negInf
		gap := scoreGapInner
		if i == n-1 {
			gap = scoreGapTrailing
		}
		for j := range m {
			if nr[i] == hr[j] {
				score := negInf
				switch {
				case i == 0:
					score = float64(j)*scoreGapLeading + bonus[j]
				case j > 0:
					score = math.Max(M[i-1][j-1]+bonus[j], D[i-1][j-1]+scoreMatchConsecutive)
				}
				D[i][j] = score
				prevScore = math.Max(score, prevScore+gap)
				M[i][j] = prevScore
			} else {
				D[i][j] = negInf
				prevScore += gap
				M[i][j] = prevScore
			}
		}
	}
	final := M[n-1][m-1]
	if math.IsInf(final, -1) {
		return 0, false
	}
	return final, true
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

// radarSortSessions orders bare tmux sessions most-recently-attached first
// (Created carries last-attached for these), the MRU the picker is meant to
// front. Never-attached sessions sink; name breaks ties for a stable order.
func radarSortSessions(sessions []rigStatus) {
	sort.SliceStable(sessions, func(i, j int) bool {
		if !sessions[i].Created.Equal(sessions[j].Created) {
			return sessions[i].Created.After(sessions[j].Created)
		}
		return sessions[i].session < sessions[j].session
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
	if s.bare {
		// A plain session carries no rig state to read; an open ring marks it
		// as "just a place to land" without competing with the rigs' dots.
		return "○", radarFaintStyle
	}
	if s.create {
		// A NEW-picker row is a session that doesn't exist yet; the plus says so.
		return "+", radarGoodStyle
	}
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
	if s.bare || s.create {
		return nil
	}
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
	// In the typing modes a prompt rides at the top so the query is always where
	// fzf trained the eye, and stays put while the list below narrows. The board
	// proper is command-mode, so it shows no prompt.
	typing := m.mode == modeBoardFilter || m.mode == modeNew
	prompt := ""
	if typing {
		prompt = radarFaintStyle.Render("/ ") + m.filter + radarFaintStyle.Render("▌") + "\n\n"
	}

	var footer string
	switch m.mode {
	case modeBoardFilter:
		footer = radarFaintStyle.Render("type to filter · enter go · esc back")
	case modeNew:
		footer = radarFaintStyle.Render("type to find · enter start session · esc back")
	default:
		footer = radarFaintStyle.Render("j/k move · enter go · / filter · n new · R refresh · q quit")
	}

	items := m.displayItems()
	selectable := 0
	for _, it := range items {
		if it.header == "" {
			selectable++
		}
	}
	if selectable == 0 {
		msg := "  nothing to pick"
		switch m.mode {
		case modeBoardFilter:
			msg = radarFaintStyle.Render("  no matches")
		case modeNew:
			if len(m.newDirs) == 0 {
				msg = radarFaintStyle.Render("  no zoxide history")
			} else {
				msg = radarFaintStyle.Render("  no matches")
			}
		}
		return "\n" + prompt + msg + "\n\n" + footer + "\n"
	}

	// Column widths come from parent rows' local cells (id, age, title) only, so
	// dangled children never widen the columns and the PR fan-out landing swaps a
	// glyph in place without moving anything.
	var wID, wAge, wTitle int
	for _, it := range items {
		if it.header != "" || it.child {
			continue
		}
		wID = max(wID, lipgloss.Width(it.row.ID))
		wAge = max(wAge, lipgloss.Width(age(it.row.Created)))
		wTitle = max(wTitle, lipgloss.Width(it.row.Title))
	}
	fixed := 2 + wID + 2 + wAge + 2 + 1 + 2 // gutter, id, age, glyph, gaps
	if m.width > 0 {
		wTitle = min(wTitle, max(20, m.width-fixed-radarTailReserve))
	}

	renderRow := func(selected bool, s rigStatus) string {
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

		if selected {
			// One style over the whole line: inner color resets would chew
			// through a wrapping reverse, so the selected row goes plain.
			plain := fmt.Sprintf("▸ %-*s  %-*s  %s  %-*s  %s",
				wID, s.ID, wAge, age(s.Created), glyph, wTitle, title, strings.Join(plainTail, "  "))
			return radarCursorStyle.Render(strings.TrimRight(plain, " "))
		}
		return fmt.Sprintf("  %-*s  %-*s  %s  %-*s  %s",
			wID, s.ID, wAge, age(s.Created), gstyle.Render(glyph),
			wTitle, title, strings.Join(styledTail, "  "))
	}

	// renderChild draws a dangled agent line: an indented tree branch, the window
	// label when a parent has more than one, and the agent's current task (a faint
	// dash when it hasn't named one). The line is faint — it's ambient context —
	// and goes reverse-video when it's the selection.
	renderChild := func(selected bool, s rigStatus, last bool) string {
		branch := "├"
		if last {
			branch = "└"
		}
		// Put the branch glyph directly under the title's first char (fixed is
		// where titles start), so the child text steps in one notch and reads as
		// nested. A session's empty id column would otherwise strand it far left.
		head := strings.Repeat(" ", fixed) + branch + " "
		if s.childKey != "" {
			head += s.childKey + "  "
		}
		label := s.Title
		if label == "" {
			label = "—"
		}
		avail := int(^uint(0) >> 1)
		if m.width > 0 {
			avail = max(4, m.width-lipgloss.Width(head)-2)
		}
		plain := head + radarTruncate(label, avail)
		if selected {
			return radarCursorStyle.Render(plain)
		}
		return radarFaintStyle.Render(plain)
	}

	// Render every screen line — headers, blanks, parent rows, dangled children —
	// into one slice, remembering which line the cursor sits on. Windowing then
	// trims the slice to the popup rather than letting the terminal scroll the top
	// away. boardLines is the same layout the mouse handler hit-tests, so a click
	// lands on exactly the row drawn under it.
	lines := m.boardLines()
	body := make([]string, 0, len(lines))
	cursorLine := 0
	for _, ln := range lines {
		switch {
		case ln.blank:
			body = append(body, "")
		case ln.item.header != "":
			body = append(body, radarHeaderStyle.Render(ln.item.header))
		default:
			selected := ln.cursor == m.cursor
			if selected {
				cursorLine = len(body)
			}
			if ln.item.child {
				body = append(body, renderChild(selected, ln.item.row, ln.item.last))
			} else {
				body = append(body, renderRow(selected, ln.item.row))
			}
		}
	}

	// Window the body to the popup, scrolled to keep the cursor in view. An arrow
	// in the footer says when rows sit above or below the fold.
	if _, budget := m.viewportChrome(); budget >= 0 {
		start, end := windowBody(len(body), cursorLine, budget)
		if start > 0 || end < len(body) {
			hint := ""
			if start > 0 {
				hint += "↑"
			}
			if end < len(body) {
				hint += "↓"
			}
			footer += radarFaintStyle.Render("  " + hint + " more")
		}
		body = body[start:end]
	}

	out := prompt + strings.Join(body, "\n") + "\n\n" + footer
	if m.scanErr != nil {
		out += "\n" + radarErrStyle.Render("scan: "+m.scanErr.Error())
	}
	return out
}

// viewportChrome reports the furniture the View spends around the body:
// promptRows is how many lines precede it (the prompt and its blank in the
// typing modes), and budget is how many body lines fit the popup, or -1 when the
// height isn't known yet (no windowing). The mouse handler subtracts the same so
// a click lines up with the row drawn under it.
func (m radarModel) viewportChrome() (promptRows, budget int) {
	typing := m.mode == modeBoardFilter || m.mode == modeNew
	if typing {
		promptRows = 2 // prompt line + blank
	}
	if m.height <= 0 {
		return promptRows, -1
	}
	chrome := 3 // blank line + footer, plus a row of slack off the bottom cell
	if typing {
		chrome += promptRows
	}
	if m.scanErr != nil {
		chrome++
	}
	return promptRows, max(1, m.height-chrome)
}

// rowAtY maps a mouse Y (a popup screen row) to the selectable cursor index
// drawn there, reproducing the View's prompt offset and windowing. The bool is
// false when the click landed on a header, a blank, or empty space.
func (m radarModel) rowAtY(y int) (int, bool) {
	lines := m.boardLines()
	if len(lines) == 0 {
		return 0, false
	}
	cursorLine := 0
	for i, ln := range lines {
		if ln.cursor == m.cursor {
			cursorLine = i
			break
		}
	}
	promptRows, budget := m.viewportChrome()
	start, end := 0, len(lines)
	if budget >= 0 {
		start, end = windowBody(len(lines), cursorLine, budget)
	}
	vy := y - promptRows
	if vy < 0 || vy >= end-start {
		return 0, false
	}
	idx := start + vy
	if lines[idx].cursor < 0 {
		return 0, false
	}
	return lines[idx].cursor, true
}

// windowBody returns the [start, end) slice of a body of n display lines that
// fits budget rows while keeping cursorLine visible: the list holds at the top
// until the cursor would fall off the bottom, then scrolls just enough to keep
// it on screen. A zero or oversized budget shows everything.
func windowBody(n, cursorLine, budget int) (int, int) {
	if budget <= 0 || n <= budget {
		return 0, n
	}
	start := 0
	if cursorLine >= budget {
		start = cursorLine - budget + 1
	}
	if start > n-budget {
		start = n - budget
	}
	if start < 0 {
		start = 0
	}
	return start, start + budget
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
