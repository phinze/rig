package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// switches after repairing its carousel when needed, and a parked one wakes.
// The popup inherits $TMUX, so switch-client from inside it moves the
// underlying client, and the -E popup tears down as we exit.
//
// It doubles as the universal picker that replaces tmux-session-wizard: the
// hosting session appears first as non-selectable context, then other live rigs
// and non-rig tmux sessions share an MRU section, with review-priority parked
// rigs below. Each parent dangles its live agent windows as a HUD of what's in
// flight. You just type to fuzzy-filter, with verbs on modifier keys so they
// never fight the query: ctrl+n opens the shared new-rig wizard, ctrl+p parks or
// wakes the selected rig, ctrl+r refreshes, and esc clears a live query then
// quits.
func runRadar(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: rig radar")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	chosen, err := radarPick(home)
	if err != nil || chosen == nil {
		return err
	}
	return radarFinish(*chosen)
}

// radarPick runs the board as a chooser. It prepares a selected destination
// while Bubble Tea is still alive, so failures stay visible in the same model;
// a successful choice is returned for the final tmux switch after the TUI has
// restored the terminal. nil means the user escaped without choosing.
func radarPick(home string) (*rigStatus, error) {
	if !stdinIsTTY() {
		return nil, fmt.Errorf("radar is a TUI — run it from a terminal (or tmux popup)")
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
		return nil, scan.err
	}
	m.apply(scan)

	final, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	if err != nil {
		return nil, err
	}
	return final.(radarModel).chosen, nil
}

// radarPrepare does the fallible work behind Enter without switching the tmux
// client. It runs as a Bubble Tea command, so an error returns to the existing
// model and the board can render it without losing the filter or cursor.
func radarPrepare(s rigStatus) (rigStatus, error) {
	// A history row's rig no longer exists, so there's nothing to unpark or
	// attach to. Rebuild it instead, which is the only verb that makes sense on
	// something already gone.
	if s.stone != nil {
		session, err := prepareResurrect(s.stone.ID, true, io.Discard)
		if err != nil {
			return rigStatus{}, err
		}
		s.session = session
		return s, nil
	}
	// A bare session is nothing but a name to land in; a child is the same, its
	// session field a session:index window target. No manifest, no session to
	// stand up, no PR to wake.
	if s.bare {
		return s, nil
	}
	if s.child {
		if err := touchRigMode(s.Path, true); err != nil {
			if errors.Is(err, errRigBusy) {
				return rigStatus{}, fmt.Errorf("%q is busy — another rig command is holding it (try again shortly)", s.Title)
			}
			return rigStatus{}, err
		}
		return s, nil
	}
	// Nonblocking keeps a contended destination from parking the action command
	// forever. The error comes back through radarActionMsg and remains visible.
	err := setRigParked(s.Path, false, true, nil)
	if err != nil {
		if errors.Is(err, errRigBusy) {
			label := s.Title
			if label == "" {
				label = s.ID
			}
			return rigStatus{}, fmt.Errorf("%q is busy — another rig command is holding it (try again shortly)", label)
		}
		return rigStatus{}, err
	}
	s.session = tmuxSessionName(s.Path)
	return s, nil
}

// radarFinish is deliberately tiny: preparation has already established the
// destination, so after Bubble Tea restores the terminal only the tmux switch
// remains.
func radarFinish(s rigStatus) error {
	if s.session == "" {
		return fmt.Errorf("radar destination has no tmux target")
	}
	return attachOrReport(s.session)
}

// radarModel is the Bubble Tea model. The framework layer stays thin: state is
// the two sections plus a PR cache keyed by slug (map presence = fetch landed,
// so "no PR" and "not asked yet" stay distinguishable), and all the real logic
// lives in the helpers switch/waiting/ls already share.
type radarModel struct {
	home       string
	current    string     // tmux session under the popup
	currentRow *rigStatus // same session rendered as non-selectable context

	inflight    []rigStatus
	parked      []rigStatus
	sessions    []rigStatus      // bare (non-rig) tmux sessions, MRU order
	history     []rigStatus      // tombstones inside the regret window, newest death first
	showHistory bool             // ctrl+t: show history even with no filter active
	attached    map[string]int64 // session → last-attached, for in-flight order
	prs         map[string][]rigPR
	fetchedAt   map[string]time.Time // slug → when its PRs were fetched
	pending     map[string]bool      // slug → PR fetch in flight
	parkPending map[string]bool      // basedir → park/wake transition in flight

	// Loose inbox entries, rendered as one summary line above the board. The
	// radar lives in a tmux popup where every row costs, so it says how many and
	// how loud rather than reprinting each — `rig notify list` is the detail view.
	inbox []notification

	newRig        *newRigModel // shared `rig new` wizard, embedded without leaving radar
	filter        string       // fuzzy query; empty = show everything
	cursor        int
	chosen        *rigStatus // set on Enter or successful creation; acted on after exit
	actionPending bool       // selected destination is being prepared in a tea.Cmd
	width         int
	height        int
	scanErr       error
	actionErr     error
}

type radarScanMsg struct {
	statuses []rigStatus
	sessions []tmuxSession
	attached map[string]int64
	agents   map[string][]agentChild // session name → its claude windows
	stones   []rigStatus             // torn-down rigs still inside the regret window
	err      error
}

type radarPRsMsg struct {
	slug string
	prs  []rigPR
}

type radarParkMsg struct {
	path   string
	label  string
	parked bool
	err    error
}

type radarActionMsg struct {
	destination rigStatus
	err         error
}

type radarTickMsg time.Time

// radarTickEvery is how often local state (sessions, agent recency) re-scans.
const radarTickEvery = 2 * time.Second

// radarMinTitle is the width the title cell must clear before the layout will
// spend columns on the lower-value id and tail cells. Once the popup is too
// narrow to give the title this much *and* carry a detail column, that column
// collapses — the PR tail first, then the ticket id — so the title, the one
// cell you actually read, keeps the room. Titles still shrink below this on a
// genuinely tiny popup; it's the drop threshold, not a hard floor.
const radarMinTitle = 30

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
		stones:   tombstoneRows(time.Now()),
	}
}

// tombstoneRows renders the regret window as board rows. A failure to read the
// store costs the history section and nothing else — the radar's job is the
// live board, and it should still draw if the tombstone directory is
// unreadable.
func tombstoneRows(now time.Time) []rigStatus {
	stones, err := listTombstones(now)
	if err != nil {
		return nil
	}
	rows := make([]rigStatus, 0, len(stones))
	for _, t := range stones {
		// Created carries the death time, not the birth time: every ordering and
		// age-rendering path on the board reads Created, and on a history row the
		// interesting age is how long ago you lost it.
		rows = append(rows, rigStatus{
			ID:      t.ID,
			Slug:    filepath.Base(t.Basedir),
			Title:   t.subject(),
			Path:    t.Basedir,
			Created: t.Died,
			stone:   t,
		})
	}
	return rows
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

func radarParkCmd(s rigStatus, parked bool) tea.Cmd {
	return func() tea.Msg {
		err := setRigParked(s.Path, parked, true, nil)
		label := s.Title
		if s.ID != "" {
			label = s.ID
		}
		return radarParkMsg{path: s.Path, label: label, parked: parked, err: err}
	}
}

func radarActionCmd(s rigStatus) tea.Cmd {
	return func() tea.Msg {
		destination, err := radarPrepare(s)
		return radarActionMsg{destination: destination, err: err}
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
	// The new-rig wizard is a model inside this model, not a second Bubble Tea
	// program. That keeps radar's alternate screen and tmux popup intact through
	// every prompt. Background scans may still land while it is open; only input,
	// sizing, and wizard-owned async results are delegated here.
	if m.newRig != nil {
		switch msg.(type) {
		case tea.KeyMsg, tea.WindowSizeMsg, newRigReposMsg, newRigCreatedMsg:
			wizard, cmd := m.newRig.update(msg)
			if size, ok := msg.(tea.WindowSizeMsg); ok {
				m.width, m.height = size.Width, size.Height
			}
			if wizard.done {
				m.newRig = nil
				if wizard.result.Session != "" {
					dest := rigStatus{bare: true, session: wizard.result.Session}
					m.chosen = &dest
					return m, tea.Quit
				}
				return m, nil
			}
			m.newRig = &wizard
			return m, cmd
		}
	}

	// Preparation mutates the selected destination and cannot be cancelled
	// safely halfway through. Keep rendering ticks and scans, but ignore input
	// until its result lands; the footer makes that short wait explicit.
	if m.actionPending {
		switch msg.(type) {
		case tea.KeyMsg, tea.MouseMsg:
			return m, nil
		}
	}

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
					return m.beginAction(rows[cur])
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
		if m.currentRow != nil && m.currentRow.Slug == msg.slug {
			m.currentRow.PRs = msg.prs
		}
		for _, section := range []*[]rigStatus{&m.inflight, &m.parked} {
			for i := range *section {
				if (*section)[i].Slug == msg.slug {
					(*section)[i].PRs = msg.prs
				}
			}
		}
		m.fetchedAt[msg.slug] = time.Now()
		delete(m.pending, msg.slug)
		saveRadarCache(m.prs, m.fetchedAt)
		m.resort()
		return m, nil

	case radarParkMsg:
		delete(m.parkPending, msg.path)
		if msg.err != nil {
			if errors.Is(msg.err, errRigBusy) {
				m.actionErr = fmt.Errorf("%q is busy; try again shortly", msg.label)
			} else {
				m.actionErr = msg.err
			}
			return m, nil
		}
		m.actionErr = nil
		m.applyParked(msg.path, msg.parked)
		return m, radarScanCmd(m.home)

	case radarActionMsg:
		m.actionPending = false
		if msg.err != nil {
			m.actionErr = msg.err
			return m, nil
		}
		m.actionErr = nil
		m.chosen = &msg.destination
		return m, tea.Quit
	}
	return m, nil
}

// handleKey drives the picker as an always-on fuzzy filter, the way session-
// wizard's fzf did: printable keys narrow the list, the cursor rides the best
// match, and the verbs live on modifier keys so they never collide with the
// query. ctrl+n hands input to the embedded new-rig wizard instead.
func (m radarModel) handleKey(key string) (radarModel, tea.Cmd) {
	// Board-global keys: move, select, hard-quit.
	switch key {
	case "ctrl+c":
		return m, tea.Quit
	case "down":
		if m.cursor < len(m.rows())-1 {
			m.cursor++
		}
		return m, nil
	case "up":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "enter":
		rows := m.rows()
		if len(rows) == 0 {
			return m, nil
		}
		return m.beginAction(rows[m.cursor])
	}
	return m.handleBoardKey(key)
}

func (m radarModel) beginAction(s rigStatus) (radarModel, tea.Cmd) {
	if m.actionPending {
		return m, nil
	}
	m.actionErr = nil
	m.actionPending = true
	return m, radarActionCmd(s)
}

// handleBoardKey runs the board: printable runes narrow the filter, the verbs
// hang off modifiers (ctrl+n new, ctrl+r refresh), and esc clears a live query
// before it quits so a search is never one keystroke from dropping the popup.
func (m radarModel) handleBoardKey(key string) (radarModel, tea.Cmd) {
	switch key {
	case "esc":
		if m.filter != "" {
			m.setFilter("")
			return m, nil
		}
		return m, tea.Quit
	case "ctrl+t":
		// Deliberate "what have I lost lately". Reset the cursor because the
		// row set just changed shape under it, same as setFilter does.
		m.showHistory = !m.showHistory
		m.cursor = 0
	case "ctrl+u":
		m.setFilter("")
	case "backspace":
		if r := []rune(m.filter); len(r) > 0 {
			m.setFilter(string(r[:len(r)-1]))
		}
	case "ctrl+n":
		agent, err := parseAgent("")
		if err != nil {
			agent = agentClaude
		}
		wizard, err := newRigWizardModel("", "", agent)
		if err != nil {
			return m, nil // an empty kickoff has no preflight error
		}
		if m.width > 0 || m.height > 0 {
			wizard, _ = wizard.update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
		}
		m.newRig = &wizard
		return m, wizard.Init()
	case "ctrl+p":
		rows := m.rows()
		if len(rows) == 0 || m.cursor >= len(rows) {
			return m, nil
		}
		s := rows[m.cursor]
		// A history row keeps its old basedir in Path, so it slips past the
		// emptiness check below; park it and you'd be stamping a manifest that
		// no longer exists. Enter is the only verb it has.
		if s.stone != nil {
			m.actionErr = fmt.Errorf("%s is torn down — enter resurrects it", s.stone.ID)
			return m, nil
		}
		if s.bare || s.Path == "" {
			m.actionErr = fmt.Errorf("plain tmux sessions cannot be parked")
			return m, nil
		}
		if m.parkPending == nil {
			m.parkPending = map[string]bool{}
		}
		if m.parkPending[s.Path] {
			return m, nil
		}
		m.actionErr = nil
		m.parkPending[s.Path] = true
		return m, radarParkCmd(s, !s.Parked)
	case "ctrl+r":
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
	default:
		m = m.typeInto(key)
	}
	return m, nil
}

// typeInto appends a lone printable rune to the filter. Named keys (tab, f-keys,
// arrows) arrive as multi-rune strings and are left untouched.
func (m radarModel) typeInto(key string) radarModel {
	if r := []rune(key); len(r) == 1 && unicode.IsGraphic(r[0]) {
		m.setFilter(m.filter + key)
	}
	return m
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
	m.inbox = looseNotifications(activeNotifications())
	// The selection to preserve is what the user is looking at now, so read
	// it before the sections are replaced.
	selected := m.selectedKey()

	// The rig session names are the ones the bare-session pass must exclude, so
	// a rig never shows up twice (once as itself, once as a plain session).
	rigSessions := make(map[string]bool, len(scan.statuses))
	var currentRow *rigStatus
	var inflight, parked []rigStatus
	for _, s := range scan.statuses {
		session := tmuxSessionName(s.Path)
		rigSessions[session] = true
		if prs, ok := m.prs[s.Slug]; ok {
			s.PRs = prs
		}
		switch {
		case m.current != "" && session == m.current:
			current := s
			currentRow = &current
		case s.Parked:
			parked = append(parked, s)
		default:
			inflight = append(inflight, s)
		}
	}

	var sessions []rigStatus
	for _, ts := range scan.sessions {
		if rigSessions[ts.Name] {
			continue
		}
		s := bareSession(ts, m.home)
		if ts.Name == m.current {
			currentRow = &s
			continue
		}
		sessions = append(sessions, s)
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
	if currentRow != nil {
		currentRow.agents = scan.agents[m.current]
	}

	m.currentRow = currentRow
	m.inflight, m.parked, m.sessions = inflight, parked, sessions
	m.history = scan.stones
	m.resortKeeping(selected)
}

// applyParked reflects a completed toggle immediately instead of making the
// user wait for the next two-second scan. The follow-up scan remains the source
// of truth for session and agent details; this local move only updates the
// stable state needed to redraw the two sections now.
func (m *radarModel) applyParked(path string, parked bool) {
	var found rigStatus
	foundIndex := -1
	from := &m.inflight
	if !parked {
		from = &m.parked
	}
	for i, s := range *from {
		if s.Path == path {
			found, foundIndex = s, i
			break
		}
	}
	if foundIndex < 0 {
		return
	}
	*from = append((*from)[:foundIndex], (*from)[foundIndex+1:]...)
	found.Parked = parked
	found.LastTouched = time.Now()
	found.SessionLive = !parked
	found.Agent = ""
	found.agents = nil
	delete(m.attached, tmuxSessionName(path))
	if parked {
		m.parked = append(m.parked, found)
	} else {
		m.inflight = append(m.inflight, found)
	}
	m.resortKeeping("slug:" + found.Slug)
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

// resort keeps the cursor on the row it's on after the board reorders, so a rig
// climbing the MRU list doesn't yank the selection off it. The ordering itself
// lives in rows()/boardRows(); this only re-pins the cursor.
func (m *radarModel) resort() {
	m.resortKeeping(m.selectedKey())
}

func (m *radarModel) resortKeeping(selected string) {
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

// rigRows is every live rig flattened, unfiltered: the PR fan-out enriches the
// current rig and every destination regardless of what the filter is hiding.
func (m radarModel) rigRows() []rigStatus {
	out := make([]rigStatus, 0, len(m.inflight)+len(m.parked)+1)
	if m.currentRow != nil && !m.currentRow.bare {
		out = append(out, *m.currentRow)
	}
	out = append(out, m.inflight...)
	out = append(out, m.parked...)
	return out
}

// radarLine is one line the board draws: a section header, a non-selectable
// parent label, or a row. A row can be a parent or a dangled child; last marks
// the final child of a parent so the tree closes with └ instead of ├.
// action overrides the row returned on Enter. A rig with one child uses that to
// keep the rig-shaped display while jumping straight to its only agent pane.
type radarLine struct {
	header string
	row    rigStatus
	child  bool
	last   bool
	label  bool
	action *rigStatus
}

func (l radarLine) selectableRow() (rigStatus, bool) {
	if l.header != "" || l.label {
		return rigStatus{}, false
	}
	if l.action != nil {
		return *l.action, true
	}
	return l.row, true
}

// displayItems is the single ordered list both the cursor and renderer read, so
// the two never drift. Live rigs and plain sessions share an MRU section;
// parked rigs live in their own review-priority section. Filtering ranks within
// each section rather than flattening the state boundary away.
func (m radarModel) displayItems() []radarLine {
	var items []radarLine
	if m.currentRow != nil {
		current := *m.currentRow
		// Match the useful title folding of a one-agent rig without turning the
		// current row into a destination. Multiple agents keep the durable rig
		// title because there is no single context to stand in for the session.
		if !current.bare && len(current.agents) == 1 {
			if context := strings.TrimSpace(current.agents[0].Context); context != "" {
				current.Title = context
			}
		}
		items = append(items,
			radarLine{header: "CURRENT"},
			radarLine{row: current, label: true},
		)
	}
	parent := func(p rigStatus) {
		// A rig with one live agent has one meaningful destination. Fold the
		// agent's fresher context into the rig row and make Enter target that pane
		// exactly. With several agents the rig becomes a group label: only the
		// children are choices, so the cursor doesn't stop on an ambiguous parent.
		// Bare tmux sessions keep their path row because it is their only visible
		// identity; this folding is specifically about a rig and its agents.
		if !p.bare && len(p.agents) == 1 {
			c := p.agents[0]
			display := p
			if context := strings.TrimSpace(c.Context); context != "" {
				display.Title = context
			}
			action := display
			action.child = true
			action.session = c.Target
			items = append(items, radarLine{row: display, action: &action})
			return
		}

		items = append(items, radarLine{row: p, label: !p.bare && len(p.agents) > 1})
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
			title := strings.TrimSpace(c.Context)
			if title == "" && !p.bare {
				title = p.Title
			}
			agent := ""
			if c.Working {
				agent = "working"
			}
			display := rigStatus{child: true, session: c.Target, Title: title, childKey: key, Agent: agent}
			action := p
			action.child = true
			action.session = c.Target
			action.Title = title
			action.childKey = key
			action.Agent = agent
			items = append(items, radarLine{
				row:    display,
				child:  true,
				last:   i == len(p.agents)-1,
				action: &action,
			})
		}
	}
	for _, section := range m.parentSections() {
		if len(section.rows) == 0 {
			continue
		}
		items = append(items, radarLine{header: section.header})
		for _, p := range section.rows {
			parent(p)
		}
	}
	return items
}

type radarSection struct {
	header string
	rows   []rigStatus
}

// boardRows flattens the section parents for callers that care about selection
// order but not headers: in-flight MRU rows first, parked review-priority rows
// second.
func (m radarModel) boardRows() []rigStatus {
	sections := m.parentSections()
	var rows []rigStatus
	for _, section := range sections {
		rows = append(rows, section.rows...)
	}
	return rows
}

func (m radarModel) parentSections() []radarSection {
	live := make([]rigStatus, 0, len(m.inflight)+len(m.sessions))
	live = append(live, m.inflight...)
	live = append(live, m.sessions...)
	sort.SliceStable(live, func(i, j int) bool {
		return m.moreRecentlyTouched(live[i], live[j])
	})

	parked := append([]rigStatus(nil), m.parked...)
	sort.SliceStable(parked, func(i, j int) bool {
		return m.moreRecentlyTouched(parked[i], parked[j])
	})

	// History is hidden at rest. The popup's rows belong to live work, and a
	// busy week of teardowns would push it off the fold for rows that are, by
	// definition, finished. A filter reveals it instead, because the moment
	// this exists for is the one where you search for a rig without knowing
	// it's gone: coming back empty would teach you it never existed. ctrl+t
	// opens the whole window deliberately.
	//
	// It arrives newest-death-first from the store and stays that way; it's the
	// one section whose order isn't a judgment about what needs you.
	var history []rigStatus
	if m.filter != "" || m.showHistory {
		history = append(history, m.history...)
	}

	if m.filter != "" {
		live = m.rankRows(live)
		parked = m.rankRows(parked)
		history = m.rankRows(history)
	}
	sections := []radarSection{
		{header: "IN FLIGHT", rows: live},
		{header: "PARKED / AWAITING REVIEW", rows: parked},
	}
	// Only when there's something to show. The radar lives in a popup where
	// rows are expensive, and a permanent empty header would cost a line on
	// every board for the rare occasion it has content.
	if len(history) > 0 {
		sections = append(sections, radarSection{header: "RECENTLY TORN DOWN", rows: history})
	}
	return sections
}

// recency is the timestamp radar both displays and sorts on. A live tmux
// attachment can move a row to the top immediately; the manifest's durable
// last-touched stamp preserves that order after the session goes away. Legacy
// rigs and history rows fall back to Created. Agent output is intentionally not
// part of this key: background work should update state without moving rows.
func (m radarModel) recency(s rigStatus) int64 {
	var r int64
	if s.bare {
		r = m.attached[s.session]
	} else {
		r = m.attached[tmuxSessionName(s.Path)]
	}
	if t := s.LastTouched.Unix(); t > r {
		r = t
	}
	if c := s.Created.Unix(); c > r {
		r = c
	}
	return r
}

func (m radarModel) touchedAt(s rigStatus) time.Time {
	if unix := m.recency(s); unix > 0 {
		return time.Unix(unix, 0)
	}
	return time.Time{}
}

// moreRecentlyTouched deliberately returns false for a tie, leaving the stable
// sort's existing order alone. Equal second-resolution touch stamps are common
// when a rig is created and entered in one operation; inventing a secondary
// priority there would create motion without a newer touch.
func (m radarModel) moreRecentlyTouched(a, b rigStatus) bool {
	return m.recency(a) > m.recency(b)
}

// screenLine is one rendered row of the board: a blank separator, a section
// header, or a display item, tagged with the selectable cursor index it maps to
// (-1 for headers and non-selectable parent labels). View renders from this and
// the mouse handler hit-tests against it, so the two share one notion of what
// sits on each line.
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
		if _, ok := it.selectableRow(); !ok {
			lines = append(lines, screenLine{item: it, cursor: -1})
			continue
		}
		lines = append(lines, screenLine{item: it, cursor: sel})
		sel++
	}
	return lines
}

// rows is the selectable list the cursor walks. A lone child is folded into its
// rig row; a parent with several children is only a label, leaving one cursor
// stop per concrete agent destination.
func (m radarModel) rows() []rigStatus {
	items := m.displayItems()
	out := make([]rigStatus, 0, len(items))
	for _, it := range items {
		if row, ok := it.selectableRow(); ok {
			out = append(out, row)
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

// agentPlaceholder is Claude Code's default title before a task is named — the
// tell that an agent is open but idle-blank rather than mid-task.
const agentPlaceholder = "Claude Code"

func isAgentPlaceholder(title string) bool {
	return title == agentPlaceholder || title == "Codex" || title == "Antigravity"
}

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

// hayField is one segment of a row's fuzzy haystack, tagged with the rendered
// column it maps back to ("id", "title") or "" when it's match-only text with no
// cell of its own (a bare session's raw name). The tag is what lets a match light
// up the exact runes it landed on.
type hayField struct {
	text  string
	field string
}

// radarHayFields is a row's fuzzy haystack split into rendered fields: a rig by
// its id and title, a bare session or create-row by its path-title plus the raw
// session name (what you'd half-remember typing, though it has no column to
// bold). Each live agent's task context rides along too, so a row is findable by
// the text the board actually shows on its dangled children — "Evaluate UPS
// options", not just the repo path. Those are match-only (no column of their
// own): the hit lights up on the child row that carries the words, not up here.
//
// A rig's repos ride along the same way, because which repo you're working in is
// half of how you remember a task ("the runtime one") and a rig's flat basedir
// says nothing about it — unlike a bare session, whose path-title carries the
// repo already. They're the full owner/repo: the slash is a boundary the scorer
// rewards, so "runtime" still lands cleanly on mirendev/runtime, and the owner
// costs nothing but buys "phinze" as a way to sweep up personal work.
func radarHayFields(s rigStatus) []hayField {
	var fields []hayField
	if s.bare {
		fields = []hayField{{s.Title, "title"}, {s.session, ""}}
	} else {
		fields = []hayField{{s.ID, "id"}, {s.Title, "title"}}
	}
	for _, repo := range s.Repos {
		fields = append(fields, hayField{repo, ""})
	}
	for _, c := range s.agents {
		if ctx := strings.TrimSpace(c.Context); ctx != "" {
			fields = append(fields, hayField{ctx, ""})
		}
	}
	return fields
}

// radarHaystack is the flat text a row is fuzzy-matched against: its fields
// joined by spaces.
func radarHaystack(s rigStatus) string {
	fields := radarHayFields(s)
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = f.text
	}
	return strings.Join(parts, " ")
}

// radarMatchFields buckets the query's matched haystack positions back into the
// rendered id and title columns, so renderRow can bold exactly the runes that
// matched. Positions on the joining spaces or in a match-only field (a bare
// session's name) have no cell and are dropped.
func radarMatchFields(query string, s rigStatus) (idHits, titleHits map[int]bool) {
	idHits, titleHits = map[int]bool{}, map[int]bool{}
	if query == "" {
		return
	}
	fields := radarHayFields(s)
	var runes []rune
	starts := make([]int, len(fields))
	for i, f := range fields {
		if i > 0 {
			runes = append(runes, ' ')
		}
		starts[i] = len(runes)
		runes = append(runes, []rune(f.text)...)
	}
	hits := fuzzyPositions(query, string(runes))
	for i, f := range fields {
		start, n := starts[i], len([]rune(f.text))
		for p := range hits {
			if p < start || p >= start+n {
				continue
			}
			switch f.field {
			case "id":
				idHits[p-start] = true
			case "title":
				titleHits[p-start] = true
			}
		}
	}
	return
}

// fuzzyPositions is the set of haystack rune indices the query matched, unioned
// across its space-separated terms — the highlight companion to fuzzyScore.
func fuzzyPositions(query, hay string) map[int]bool {
	hits := map[int]bool{}
	lower := strings.ToLower(hay)
	for term := range strings.FieldsSeq(strings.ToLower(query)) {
		for _, p := range matchPositions(term, lower) {
			hits[p] = true
		}
	}
	return hits
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

// matchMatrices runs fzy's two-matrix DP of needle against hay: D[i][j] is the
// best score ending with needle[i] aligned on hay[j], M[i][j] the best over
// hay[0..j]. Both callers — the score and the highlight backtrace — share it.
// Caller guarantees 0 < len(needle) <= len(hay).
func matchMatrices(nr, hr []rune) (D, M [][]float64) {
	n, m := len(nr), len(hr)

	// Boundary bonus per haystack position; the string start counts as a slash
	// so a leading match is treated as a segment start.
	bonus := make([]float64, m)
	prev := '/'
	for j, c := range hr {
		bonus[j] = matchBonus(prev)
		prev = c
	}

	negInf := math.Inf(-1)
	D = make([][]float64, n)
	M = make([][]float64, n)
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
	return D, M
}

// matchScore aligns needle against hay with fzy's DP and returns the total
// score, or (0, false) when needle isn't a subsequence of hay at all. Cost is
// O(len(needle)·len(hay)); both are short here.
func matchScore(needle, hay string) (float64, bool) {
	nr, hr := []rune(needle), []rune(hay)
	n, m := len(nr), len(hr)
	if n == 0 {
		return 0, true
	}
	if n > m {
		return 0, false
	}
	_, M := matchMatrices(nr, hr)
	final := M[n-1][m-1]
	if math.IsInf(final, -1) {
		return 0, false
	}
	return final, true
}

// matchPositions returns, for a matching needle, the hay rune index each needle
// rune aligns to under the optimal score — fzy's backtrace through the same DP
// matchScore rates. Nil when needle isn't a subsequence. It walks the rows back
// to front, taking the cell that realized M's best and locking onto consecutive
// runs (a match that earned the consecutive bonus must have had its predecessor
// adjacent), which reproduces the alignment the score rewarded.
func matchPositions(needle, hay string) []int {
	nr, hr := []rune(needle), []rune(hay)
	n, m := len(nr), len(hr)
	if n == 0 || n > m {
		return nil
	}
	D, M := matchMatrices(nr, hr)
	if math.IsInf(M[n-1][m-1], -1) {
		return nil
	}
	positions := make([]int, n)
	matchRequired := false
	j := m - 1
	for i := n - 1; i >= 0; i-- {
		for ; j >= 0; j-- {
			if math.IsInf(D[i][j], -1) {
				continue
			}
			if matchRequired || D[i][j] == M[i][j] {
				if i > 0 && j > 0 && M[i][j] == D[i-1][j-1]+scoreMatchConsecutive {
					matchRequired = true
				}
				positions[i] = j
				j--
				break
			}
		}
	}
	return positions
}

func prsFetched(prs map[string][]rigPR, slug string) bool {
	_, ok := prs[slug]
	return ok
}

// radarStateCell is the state column: live agent state for an in-flight rig,
// review disposition for a parked one ("…" while the fetch is still out).
func radarStateCell(s rigStatus, fetched bool) string {
	// A history row's state isn't "what is it doing" but "can I get it back",
	// which is the only question you'd have about a rig that no longer exists.
	if s.stone != nil {
		if s.stone.resurrectable() {
			return "↺ " + s.stone.Session.Agent
		}
		return "no session"
	}
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
	radarMatchStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
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

// rigKind is what a rig is *about*, as distinct from what it's doing. The
// board's glyph column is already spent on state (agent attention, review
// disposition), so kind rides the id cell instead. Without it a ticket rig, a
// review rig and a project rig read as three identical rows whose ids merely
// happen to be shaped differently, and you learn the shapes instead of seeing
// the kinds.
type rigKind int

const (
	rigKindLoose rigKind = iota // `rig new`: a kickoff, no tracker
	rigKindTicket
	rigKindReview
	rigKindProject
)

func radarRigKind(s rigStatus) rigKind {
	switch s.Kind {
	case "review":
		return rigKindReview
	case "project":
		return rigKindProject
	}
	if s.Tracker != "" {
		return rigKindTicket
	}
	// Rigs made before the manifest recorded a tracker have none, and several
	// are usually still in flight. Leaving those unmarked would make the marker
	// read as unreliable rather than absent, so fall back to the id, which for
	// a Linear pickup is the issue identifier and nothing else. "pr-<n>" is
	// rig's own reserved id for a PR-derived rig rather than a team prefix, so
	// it's the one shape this must not read as an issue.
	if !strings.HasPrefix(s.ID, "pr-") && leadingIssueID(s.ID) == s.ID {
		return rigKindTicket
	}
	return rigKindLoose
}

// radarKindGlyph marks the id cell with its kind. The shapes come from families
// the state glyphs don't use — a tag for a ticket, octicon's pull-request for a
// review (its git-merge sibling already means merged), a sitemap for a
// project's umbrella of issues — so kind and state never trade places at a
// glance. A loose rig draws nothing: absence is its marker, and a glyph on
// every row buys less than the two columns it would cost in a popup.
func radarKindGlyph(k rigKind) string {
	switch k {
	case rigKindTicket:
		return "\uf02b"
	case rigKindReview:
		return "\uf407"
	case rigKindProject:
		return "\uf0e8"
	}
	return ""
}

// radarMaxID caps the id cell. Ticket and review ids are short by construction;
// the ones that run long are a slugified restatement of the title sitting two
// columns to the left, so clipping costs nothing — and uncapped, one such row
// sets a width every other row pays for. The collapse loop drops the tail
// before the id, so a single 56-character kickoff slug was quietly taking every
// PR and CI glyph on the board with it.
const radarMaxID = 20

// radarIDCell is the id as the board draws it: a kind glyph, then the id
// clipped to radarMaxID. It comes back empty for a row with nothing to say —
// a bare tmux session, or a `rig new` rig whose id is only its own title
// slugified, where drawing it means printing the title twice in one row.
type radarIDCell struct {
	glyph   string
	id      string // as displayed, so possibly clipped
	clipped bool
}

func newRadarIDCell(s rigStatus) radarIDCell {
	if s.bare || s.ID == "" {
		return radarIDCell{}
	}
	clip := func() radarIDCell {
		return radarIDCell{id: radarTruncate(s.ID, radarMaxID), clipped: len(s.ID) > radarMaxID}
	}
	// A tombstone carries no manifest to read a kind from, and its tail already
	// answers the only live question about it.
	if s.stone != nil {
		return clip()
	}
	kind := radarRigKind(s)
	// kickoffID is the function that produced this id, so asking it is an exact
	// test for "the id is the title" rather than a guess about slug shapes. A
	// rig whose title has since moved on fails the test and keeps its id, which
	// is the right way round to be wrong.
	if kind == rigKindLoose && s.ID == kickoffID(s.Title) {
		return radarIDCell{}
	}
	cell := clip()
	cell.glyph = radarKindGlyph(kind)
	return cell
}

func (c radarIDCell) empty() bool { return c.glyph == "" && c.id == "" }

// plain is the cell as it occupies space, which is what the width math and the
// selected row's unstyled render both need.
func (c radarIDCell) plain() string {
	switch {
	case c.glyph == "":
		return c.id
	case c.id == "":
		return c.glyph
	}
	return c.glyph + " " + c.id
}

// radarGlyph is the fixed-width status cell: one glyph whose shape and color
// carry the row's state, so the PR fan-out landing never shifts the layout.
// In-flight rigs read agent attention; parked rigs read review disposition.
// Glyphs are conservative nerd-font codepoints (classic FA + octicons).
func radarGlyph(s rigStatus, fetched bool) (string, lipgloss.Style) {
	// A history row is gone; the only state worth a glyph is whether it can
	// come back. The recycle arrow reads as "restore" without borrowing any of
	// the live vocabulary, and an unrecoverable one stays a faint dot.
	if s.stone != nil {
		if s.stone.resurrectable() {
			return "↺", radarWarnStyle
		}
		return "·", radarFaintStyle
	}
	if s.bare {
		// A plain session carries no rig state to read; an open ring marks it
		// as "just a place to land" without competing with the rigs' dots.
		return "○", radarFaintStyle
	}
	if s.Parked {
		if !fetched {
			return "…", radarFaintStyle
		}
		switch parkedDisposition(s.PRs) {
		case "changes requested":
			return "", radarHotStyle // comment
		case "approved":
			return "", radarGoodStyle // thumbs-up: approved
		case "merged":
			return "", radarDoneStyle // git merge
		case "no PR":
			return "·", radarFaintStyle
		default: // waiting
			return "", radarFaintStyle // eye: awaiting review
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

// radarReviewGlyph is the per-PR review glyph for the tail: where the PR stands
// with human review, drawn from a different icon family than radarChecksGlyph's
// CI so an approved review (thumbs-up) never wears the same check as passing CI,
// nor awaiting-review (eye) the same clock as pending CI. A merged or closed PR
// has no live review to show, so it trails no review glyph.
func radarReviewGlyph(pr rigPR) (string, lipgloss.Style) {
	if pr.State != "OPEN" {
		return "", radarFaintStyle
	}
	switch pr.Review {
	case "CHANGES_REQUESTED":
		return "", radarHotStyle // comment: your move
	case "APPROVED":
		return "", radarGoodStyle // thumbs-up: go merge
	default: // REVIEW_REQUIRED or "" — still out for review
		return "", radarFaintStyle // eye: awaiting review
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
// rig the disposition spelled out (the glyph's legend rides along), then each PR
// as number + review glyph + CI glyph, repo-prefixed when the rig spans repos.
// The review glyph is what surfaces review state on in-flight rigs too, not just
// parked ones. Loading reads as a bare "…"; an in-flight rig with no PR trails
// nothing at all.
func radarTailSegs(s rigStatus, fetched bool) []tailSeg {
	if s.bare {
		return nil
	}
	// History rows are never enriched (rigRows excludes them), so the "not
	// fetched yet" ellipsis below would be a permanent lie. Their tail says how
	// to get the rig back instead, which is the only live question about it.
	if s.stone != nil {
		if s.stone.resurrectable() {
			return []tailSeg{{s.stone.Session.Agent, radarFaintStyle.Render(s.stone.Session.Agent)}}
		}
		return []tailSeg{{"no session", radarFaintStyle.Render("no session")}}
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
		if g, st := radarReviewGlyph(pr); g != "" {
			plain += " " + g
			styled += " " + st.Render(g)
		}
		if g, st := radarChecksGlyph(pr.Checks); g != "" {
			plain += " " + g
			styled += " " + st.Render(g)
		}
		segs = append(segs, tailSeg{plain, styled})
	}
	return segs
}

// radarColumns is the resolved column layout for one render: each cell's width
// (zero means the cell is dropped) and the screen column the title starts at,
// which is also where dangled children indent their branch. Widths come only
// from parent rows so children never widen a column, and a fetch landing swaps a
// glyph in place without shifting anything.
type radarColumns struct {
	wID, wAge, wTitle, wTail int
	titleAt                  int
}

// radarTailWidth is the display width of a row's rendered PR tail (plain), the
// segments joined the way the row draws them. Zero for a row with no tail.
func (m radarModel) radarTailWidth(s rigStatus) int {
	segs := radarTailSegs(s, prsFetched(m.prs, s.Slug))
	w := 0
	for i, seg := range segs {
		if i > 0 {
			w += 2
		}
		w += lipgloss.Width(seg.plain)
	}
	return w
}

// columns resolves the board's column widths against the popup width. Age, title,
// and the ticket id come from the widest parent cell; the tail is the widest tail
// actually present (so absent PRs reserve nothing). The left frame — gutter, age,
// glyph — always stays and the title starts right after it, so an id-less session
// row leaves no gap down the left. The ticket id and PR tail are right-hand
// detail: low value, and the first to collapse (tail before id) once the popup
// can't seat them and still leave the title a readable width.
func (m radarModel) columns(items []radarLine) radarColumns {
	var wID, wAge, wTitle, wTail int
	for _, it := range items {
		if it.header != "" || it.child {
			continue
		}
		wID = max(wID, lipgloss.Width(newRadarIDCell(it.row).plain()))
		wAge = max(wAge, lipgloss.Width(age(m.touchedAt(it.row))))
		wTitle = max(wTitle, lipgloss.Width(it.row.Title))
		wTail = max(wTail, m.radarTailWidth(it.row))
	}
	const gutter, gap, glyph = 2, 2, 1
	// Left frame, always present; the title starts here and fills rightward.
	titleAt := gutter + wAge + gap + glyph + gap

	// Unknown width (not yet sized): everything at its desired width, no drops.
	if m.width <= 0 {
		return radarColumns{wID, wAge, wTitle, wTail, titleAt}
	}

	// Collapse the right-hand detail — tail before id — until the title clears
	// its floor or nothing droppable remains. The floor is the smaller of the
	// readable minimum and what the titles actually want, so short titles never
	// trigger a drop for room they wouldn't use.
	keepID, keepTail := wID > 0, wTail > 0
	floor := min(wTitle, radarMinTitle)
	need := func() int {
		n := 0
		if keepTail {
			n += gap + wTail
		}
		if keepID {
			n += gap + wID
		}
		return n
	}
	for keepTail || keepID {
		if m.width-titleAt-need() >= floor {
			break
		}
		if keepTail {
			keepTail = false
			continue
		}
		keepID = false
	}
	if !keepID {
		wID = 0
	}
	if !keepTail {
		wTail = 0
	}
	wTitle = max(8, min(wTitle, m.width-titleAt-need()))
	return radarColumns{wID, wAge, wTitle, wTail, titleAt}
}

func (m radarModel) View() string {
	if m.newRig != nil {
		return m.newRig.View()
	}
	// A prompt rides at the top whenever there's a query to show, so the text
	// lands where fzf trained the eye and stays put while the list below narrows.
	// The resting board shows no prompt: it reads as a clean HUD until you type.
	typing := m.filter != ""
	prompt := ""
	if typing {
		prompt = radarFaintStyle.Render("/ ") + m.filter + radarFaintStyle.Render("▌") + "\n\n"
	}
	prompt = m.inboxLine() + prompt

	var footer string
	toggle := "park/wake"
	// A selected history row has no park verb at all, so the footer advertises
	// what Enter will actually do instead of offering an action that errors.
	onStone := false
	if rows := m.rows(); len(rows) > 0 && m.cursor < len(rows) {
		s := rows[m.cursor]
		switch {
		case s.stone != nil:
			onStone = true
		case !s.bare && s.Path != "":
			if s.Parked {
				toggle = "wake"
			} else {
				toggle = "park"
			}
		}
	}
	switch {
	case m.actionPending:
		footer = radarFaintStyle.Render("opening selected destination…")
	case m.filter != "" && onStone:
		footer = radarFaintStyle.Render("type to filter · enter resurrect · esc clear")
	case m.filter != "":
		footer = radarFaintStyle.Render("type to filter · enter go · ^p " + toggle + " · esc clear")
	default:
		// The history hint carries its own state, because a hidden section is
		// otherwise undiscoverable and a shown one needs a way back.
		hist := "^t history"
		if m.showHistory {
			hist = "^t hide history"
		}
		footer = radarFaintStyle.Render("type filter · enter go · ^n new · ^p " + toggle + " · ^r refresh · " + hist + " · esc quit")
	}

	items := m.displayItems()
	selectable := 0
	for _, it := range items {
		if _, ok := it.selectableRow(); ok {
			selectable++
		}
	}
	if selectable == 0 {
		if m.currentRow == nil {
			msg := "  nothing to pick"
			if m.filter != "" {
				msg = radarFaintStyle.Render("  no matches")
			}
			return "\n" + prompt + msg + "\n\n" + footer + m.radarErrorLines() + "\n"
		}
		if m.filter != "" {
			footer = radarFaintStyle.Render("no other matches · esc clear")
		} else {
			footer = radarFaintStyle.Render("nothing else to pick · ^n new · ^r refresh · esc quit")
		}
	}

	// Resolve the column layout for this frame: cell widths, and where the title
	// (and each child's branch) start. A zero width means that cell collapsed
	// because the popup was too narrow to seat it and still leave the title room.
	cols := m.columns(items)

	renderRow := func(selected bool, s rigStatus) string {
		fetched := prsFetched(m.prs, s.Slug)
		glyph, gstyle := radarGlyph(s, fetched)

		// Right-margin detail for this row: the ticket id then the PR tail, each
		// shown only when the layout kept its column. They trail immediately after
		// the title (not in a reserved column), so a row without a ticket or a PR
		// hands that width straight to its title instead of leaving a hole. The id
		// is faint — low-value detail — and bolds its matched runes when filtering.
		var rightPlain, rightStyled []string
		if idCell := newRadarIDCell(s); cols.wID > 0 && !idCell.empty() {
			// Match bolding is scoped to the id's own runes, so it can only be
			// drawn against the id in full. A clipped one renders plain rather
			// than bolding runes that have shifted out from under their hits.
			styled := radarFaintStyle.Render(idCell.id)
			if m.filter != "" && !idCell.clipped {
				idHits, _ := radarMatchFields(m.filter, s)
				styled = highlightRunesBase(idCell.id, idHits, &radarFaintStyle)
			}
			if idCell.glyph != "" {
				styled = radarFaintStyle.Render(idCell.glyph) + " " + styled
			}
			rightPlain, rightStyled = append(rightPlain, idCell.plain()), append(rightStyled, styled)
		}
		if cols.wTail > 0 {
			for _, seg := range radarTailSegs(s, fetched) {
				rightPlain, rightStyled = append(rightPlain, seg.plain), append(rightStyled, seg.styled)
			}
		}
		rw := 0
		for i, p := range rightPlain {
			if i > 0 {
				rw += 2
			}
			rw += lipgloss.Width(p)
		}

		// The title fills whatever this row leaves left of its own detail, so an
		// id-less or PR-less row's title runs wider than a laden one's.
		budget := cols.wTitle
		if m.width > 0 {
			budget = m.width - cols.titleAt
			if rw > 0 {
				budget -= rw + 2
			}
			budget = max(8, budget)
		}
		title := radarTruncateTitle(s.Title, budget)

		if selected {
			// One style over the whole line: inner color resets would chew
			// through a wrapping reverse, so the selected row goes plain — the
			// reverse-video already marks it, no match bolding needed.
			var b strings.Builder
			fmt.Fprintf(&b, "▸ %-*s  %s  %s", cols.wAge, age(m.touchedAt(s)), glyph, title)
			if rw > 0 {
				b.WriteString("  " + strings.Join(rightPlain, "  "))
			}
			return radarCursorStyle.Render(strings.TrimRight(b.String(), " "))
		}

		// Bold the title runes the query matched; the title may be front- or
		// middle-collapsed, so match against the string as displayed.
		titleCell := title
		if m.filter != "" {
			titleCell = highlightRunes(title, fuzzyPositions(m.filter, title))
		}
		var b strings.Builder
		b.WriteString("  ")
		b.WriteString(padRight(age(m.touchedAt(s)), cols.wAge) + "  ")
		b.WriteString(gstyle.Render(glyph) + "  ")
		b.WriteString(titleCell)
		if rw > 0 {
			b.WriteString("  " + strings.Join(rightStyled, "  "))
		}
		return strings.TrimRight(b.String(), " ")
	}

	// renderChild draws a dangled agent line: an indented tree branch, a working
	// dot for an agent that's produced output lately, the window label when a
	// parent has more than one, and the current task (a faint dash when it hasn't
	// named one). Idle children stay faint — ambient context — while a working one
	// gets a green dot and full-strength text so the eye lands on what's live. The
	// selected row goes reverse-video.
	renderChild := func(selected bool, s rigStatus, last bool) string {
		branch := "├"
		if last {
			branch = "└"
		}
		working := s.Agent == "working"
		// A working agent flags with ●; idle leaves the slot blank so the board
		// stays calm and only live work draws the eye. Both are two cells wide so
		// the labels line up either way.
		dot := "  "
		if working {
			dot = "● "
		}
		// Put the branch glyph directly under the title's first char (cols.titleAt
		// is where titles start), so the child text steps in one notch and reads as
		// nested. A session's empty id column would otherwise strand it far left.
		prefix := strings.Repeat(" ", cols.titleAt) + branch + " " + dot
		if s.childKey != "" {
			prefix += s.childKey + "  "
		}
		label := s.Title
		if label == "" {
			label = "—"
		}
		avail := int(^uint(0) >> 1)
		if m.width > 0 {
			avail = max(4, m.width-lipgloss.Width(prefix)-2)
		}
		label = radarTruncate(label, avail)
		if selected {
			return radarCursorStyle.Render(prefix + label)
		}
		// The branch and any label stay faint; the dot is green and, when working,
		// the task text renders at full strength to stand out from idle siblings.
		out := strings.Repeat(" ", cols.titleAt) + radarFaintStyle.Render(branch+" ")
		if working {
			out += radarGoodStyle.Render("● ")
		} else {
			out += "  "
		}
		if s.childKey != "" {
			out += radarFaintStyle.Render(s.childKey + "  ")
		}
		// Bold the runes the query matched — this is the row the search text you
		// typed actually lives on (folded into the parent's haystack so the parent
		// surfaces; lit up here where you read it). Positions are taken against the
		// already-truncated label so the indices line up.
		var hits map[int]bool
		if m.filter != "" {
			hits = fuzzyPositions(m.filter, label)
		}
		if working {
			out += highlightRunes(label, hits)
		} else {
			out += highlightRunesBase(label, hits, &radarFaintStyle)
		}
		return out
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
	out += m.radarErrorLines()
	return out
}

func (m radarModel) radarErrorLines() string {
	var lines []string
	if m.actionErr != nil {
		lines = append(lines, radarErrStyle.Render(m.actionErr.Error()))
	}
	if m.scanErr != nil {
		lines = append(lines, radarErrStyle.Render("scan: "+m.scanErr.Error()))
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n" + strings.Join(lines, "\n")
}

// inboxLine is the radar's whole notification surface: one line naming the
// loudest entry, with a count when there's more behind it. A single source
// nagging every hour is the case worth reading at a glance; anything richer is
// what `rig notify list` is for.
func (m radarModel) inboxLine() string {
	if len(m.inbox) == 0 {
		return ""
	}
	top := m.inbox[0] // activeNotifications sorts loudest-then-newest first
	line := fmt.Sprintf("%s %s: %s", notifyLevelMark(top.Level), top.Source, top.Title)
	if rest := len(m.inbox) - 1; rest > 0 {
		line += fmt.Sprintf("  +%d", rest)
	}
	style := radarWarnStyle
	switch top.Level {
	case "error":
		style = radarErrStyle
	case "info":
		style = radarFaintStyle
	}
	return style.Render(" "+line) + "\n\n"
}

// viewportChrome reports the furniture the View spends around the body:
// promptRows is how many lines precede it (the prompt and its blank in the
// typing modes), and budget is how many body lines fit the popup, or -1 when the
// height isn't known yet (no windowing). The mouse handler subtracts the same so
// a click lines up with the row drawn under it.
func (m radarModel) viewportChrome() (promptRows, budget int) {
	if m.filter != "" {
		promptRows = 2 // prompt line + blank
	}
	// The inbox banner rides above the prompt and pushes the whole board down
	// with it, so it's prompt furniture too. Counting the newlines it actually
	// rendered beats re-deriving the condition here, which is how this drifted
	// in the first place: a notification silently sent every click two rows low.
	promptRows += strings.Count(m.inboxLine(), "\n")
	if m.height <= 0 {
		return promptRows, -1
	}
	chrome := 3 + promptRows // blank line + footer + a row of slack, past the prompt
	if m.scanErr != nil {
		chrome++
	}
	if m.actionErr != nil {
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

// highlightRunes bolds the runes of s at the given indices, leaving the rest
// verbatim, so a fuzzy match lights up exactly where it landed. Consecutive hits
// render as one styled run. No hits returns s untouched.
func highlightRunes(s string, hits map[int]bool) string {
	return highlightRunesBase(s, hits, nil)
}

// highlightRunesBase is highlightRunes with a base style for the un-matched
// runs, so a faint child line can still bold its matched runes without the match
// style's reset bleeding the faint away for the rest of the string: each run,
// matched or not, is wrapped on its own. base nil renders un-matched runs
// verbatim (the plain highlightRunes behavior).
func highlightRunesBase(s string, hits map[int]bool, base *lipgloss.Style) string {
	render := func(seg string) string {
		if base == nil {
			return seg
		}
		return base.Render(seg)
	}
	if len(hits) == 0 {
		return render(s)
	}
	runes := []rune(s)
	var b strings.Builder
	for i := 0; i < len(runes); {
		hit := hits[i]
		j := i
		for j < len(runes) && hits[j] == hit {
			j++
		}
		seg := string(runes[i:j])
		if hit {
			b.WriteString(radarMatchStyle.Render(seg))
		} else {
			b.WriteString(render(seg))
		}
		i = j
	}
	return b.String()
}

// padRight pads s to w display cells with trailing spaces, counting display
// width so any ANSI styling in s doesn't throw the padding off. It stands in for
// fmt's %-*s wherever a cell may carry match highlighting.
func padRight(s string, w int) string {
	if gap := w - lipgloss.Width(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// padLeft is padRight's mirror, for cells that read best flush against the
// right edge — the sweep board's meta tail, where lining the ages up is the
// whole point of the column.
func padLeft(s string, w int) string {
	if gap := w - lipgloss.Width(s); gap > 0 {
		return strings.Repeat(" ", gap) + s
	}
	return s
}

// radarTruncateTitle clips a title to w cells. A path-shaped title (a bare
// session or a repo rig, whose title is its working dir) collapses its leading
// segments so the tail — the last segments, where the repo name lives — survives
// rather than being clipped off the right edge; any other title clips from the
// right the plain way so it still reads left-to-right.
func radarTruncateTitle(s string, w int) string {
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~/") {
		return collapsePath(s, w)
	}
	return radarTruncate(s, w)
}

// collapsePath shortens a path to w display cells by dropping leading segments,
// marking the elision with a leading "…/" and keeping as many trailing segments
// as fit (never fewer than the last one). "~/src/github.com/phinze/rig" at width
// 14 becomes "…/phinze/rig". A path that already fits comes back untouched.
func collapsePath(p string, w int) string {
	if lipgloss.Width(p) <= w {
		return p
	}
	segs := strings.Split(p, "/")
	// Drop leading segments one at a time; the first candidate that fits keeps
	// the most trailing context. Start at 1 since the whole path already overflows.
	for start := 1; start < len(segs); start++ {
		if cand := "…/" + strings.Join(segs[start:], "/"); lipgloss.Width(cand) <= w {
			return cand
		}
	}
	// Even the last segment with its marker overflows; clip it from the right.
	return radarTruncate("…/"+segs[len(segs)-1], w)
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
