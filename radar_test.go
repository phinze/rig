package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// dispRank drives both waiting's table order and the radar's parked section;
// the whole point is "most actionable first", so pin the full ordering.
func TestDispRank(t *testing.T) {
	order := []string{"changes requested", "approved", "merged", "waiting", "no PR", "…"}
	for i := 1; i < len(order); i++ {
		if !(dispRank(order[i-1]) < dispRank(order[i])) {
			t.Errorf("dispRank(%q)=%d should sort before dispRank(%q)=%d",
				order[i-1], dispRank(order[i-1]), order[i], dispRank(order[i]))
		}
	}
}

// The board is one flat MRU list mixing rigs and sessions, ranked by recency:
// tmux last-attached, or the newest claude turn / creation time for a rig with
// no live session. Newest first; never-touched rows sink.
func TestBoardRowsMRU(t *testing.T) {
	at := func(u int64) *time.Time { tm := time.Unix(u, 0); return &tm }
	m := radarModel{
		attached: map[string]int64{
			tmuxSessionName("/w/live"): 500, // live rig, attached recently
			"sess-old":                 100, // bare session, old attach
		},
		inflight: []rigStatus{
			{Slug: "live", Path: "/w/live", Created: time.Unix(1, 0)},                        // recency 500 (attach)
			{Slug: "agent", Path: "/w/agent", Created: time.Unix(1, 0), LastActive: at(400)}, // recency 400 (claude turn, no attach)
		},
		parked: []rigStatus{
			{Slug: "parked", Path: "/w/parked", Created: time.Unix(300, 0)}, // recency 300 (created)
		},
		sessions: []rigStatus{
			{bare: true, session: "sess-old", Created: time.Unix(100, 0)}, // recency 100 (attach)
		},
	}

	var got []string
	for _, s := range m.boardRows() {
		if s.bare {
			got = append(got, s.session)
		} else {
			got = append(got, s.Slug)
		}
	}
	want := []string{"live", "agent", "parked", "sess-old"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("MRU order = %v, want %v", got, want)
		}
	}
}

// The state cell reads live agent state for in-flight rigs and review
// disposition for parked ones, with "…" holding the seat until the fetch lands.
func TestRadarStateCell(t *testing.T) {
	cases := []struct {
		name    string
		s       rigStatus
		fetched bool
		want    string
	}{
		{"inflight working", rigStatus{Agent: "working"}, false, "working"},
		{"inflight no session", rigStatus{}, false, "-"},
		{"parked unfetched", rigStatus{Parked: true}, false, "…"},
		{"parked no pr", rigStatus{Parked: true}, true, "no PR"},
		{"parked merged", rigStatus{Parked: true, PRs: []rigPR{{prInfo: prInfo{State: "MERGED"}}}}, true, "merged"},
	}
	for _, c := range cases {
		if got := radarStateCell(c.s, c.fetched); got != c.want {
			t.Errorf("%s: radarStateCell = %q, want %q", c.name, got, c.want)
		}
	}
}

// isolateRadarCache redirects the radar cache into a throwaway HOME so the
// test never touches the real user cache. We point HOME rather than
// XDG_CACHE_HOME because radarCachePath goes through os.UserCacheDir, and on
// darwin that returns $HOME/Library/Caches and ignores XDG entirely. Under the
// Nix build sandbox HOME is the read-only /homeless-shelter, so an
// XDG-only override left the cache mkdir failing on macOS builds.
func isolateRadarCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
}

// The PR cache round-trips through disk, and entries past the save horizon
// are dropped rather than resurrected stale.
func TestRadarCacheRoundTrip(t *testing.T) {
	isolateRadarCache(t)
	now := time.Now().Round(time.Second)
	prs := map[string][]rigPR{
		"fresh":   {{Repo: "o/r", Branch: "b", prInfo: prInfo{Number: 7, State: "OPEN", Checks: "passing"}}},
		"ancient": {{Repo: "o/r", Branch: "c", prInfo: prInfo{Number: 8, State: "MERGED"}}},
	}
	at := map[string]time.Time{
		"fresh":   now,
		"ancient": now.Add(-2 * time.Hour),
	}
	saveRadarCache(prs, at)

	got := loadRadarCache()
	if _, ok := got["ancient"]; ok {
		t.Error("entry past the save horizon survived")
	}
	e, ok := got["fresh"]
	if !ok {
		t.Fatal("fresh entry missing after round-trip")
	}
	if len(e.PRs) != 1 || e.PRs[0].Number != 7 || e.PRs[0].Checks != "passing" {
		t.Errorf("fresh entry PRs = %+v", e.PRs)
	}
	if !e.At.Equal(now) {
		t.Errorf("fresh entry At = %v, want %v", e.At, now)
	}
}

// loadRadarCache degrades to empty on a missing or torn file.
func TestRadarCacheDegrades(t *testing.T) {
	isolateRadarCache(t)
	if got := loadRadarCache(); got != nil {
		t.Errorf("missing file: got %v, want nil", got)
	}
	path, err := radarCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{torn"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadRadarCache(); got != nil {
		t.Errorf("torn file: got %v, want nil", got)
	}
}

// The tail is append-only detail: "…" while loading, nothing for a bare
// in-flight rig, disposition-word-first for parked, repo prefixes only when
// the rig spans repos.
func TestRadarTailSegs(t *testing.T) {
	plains := func(segs []tailSeg) []string {
		out := make([]string, len(segs))
		for i, s := range segs {
			out[i] = s.plain
		}
		return out
	}
	cases := []struct {
		name    string
		s       rigStatus
		fetched bool
		want    []string
	}{
		{"loading", rigStatus{}, false, []string{"…"}},
		{"inflight no pr", rigStatus{}, true, nil},
		{"parked no pr", rigStatus{Parked: true}, true, []string{"no PR"}},
		{"single pr", rigStatus{PRs: []rigPR{
			{Repo: "o/api", prInfo: prInfo{Number: 7, State: "OPEN", Checks: "passing"}},
		}}, true, []string{"#7 "}},
		{"multi repo", rigStatus{PRs: []rigPR{
			{Repo: "o/api", prInfo: prInfo{Number: 7, State: "OPEN"}},
			{Repo: "o/web", prInfo: prInfo{Number: 9, State: "OPEN", Checks: "failing"}},
		}}, true, []string{"api #7", "web #9 "}},
		{"parked changes", rigStatus{Parked: true, PRs: []rigPR{
			{Repo: "o/api", prInfo: prInfo{Number: 7, State: "OPEN", Review: "CHANGES_REQUESTED"}},
		}}, true, []string{"changes requested", "#7"}},
	}
	for _, c := range cases {
		got := plains(radarTailSegs(c.s, c.fetched))
		if len(got) != len(c.want) {
			t.Errorf("%s: segs = %q, want %q", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: seg[%d] = %q, want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}

// The fuzzy query is space-separated subsequence terms, ANDed, case-insensitive.
func TestFuzzyMatch(t *testing.T) {
	cases := []struct {
		query string
		hay   string
		want  bool
	}{
		{"", "anything", true},
		{"rig", "~/src/github.com/phinze/rig", true},
		{"ghrig", "~/src/github.com/phinze/rig", true},
		{"RIG", "~/src/github.com/phinze/rig", true},
		{"phinze rig", "~/src/github.com/phinze/rig", true},
		{"phinze zzz", "~/src/github.com/phinze/rig", false},
		{"xyz", "~/src/github.com/phinze/rig", false},
	}
	for _, c := range cases {
		if got := fuzzyMatch(c.query, c.hay); got != c.want {
			t.Errorf("fuzzyMatch(%q, %q) = %v, want %v", c.query, c.hay, got, c.want)
		}
	}
}

// A tight, boundary-aligned match outscores a scattered one, so the ranking
// floats the obvious hit to the top.
func TestMatchScoreRanking(t *testing.T) {
	tight, ok := matchScore("rig", "/rig")
	if !ok {
		t.Fatal("tight match should match")
	}
	loose, ok := matchScore("rig", "/raging")
	if !ok {
		t.Fatal("loose match should still match")
	}
	if !(tight > loose) {
		t.Errorf("tight %.4f should outscore loose %.4f", tight, loose)
	}
	if _, ok := matchScore("zzz", "/rig"); ok {
		t.Error("non-subsequence should not match")
	}
}

// matchPositions marks the runes the needle aligned to, preferring the
// consecutive, boundary-aligned run the score rewards over a scattered earlier
// subsequence — so highlighting lands on the obvious match.
func TestMatchPositions(t *testing.T) {
	// "rig" in "/rig" is the trailing three runes, not the stray r/i/g.
	got := matchPositions("rig", "/rig")
	want := []int{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("positions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("positions = %v, want %v", got, want)
		}
	}
	if matchPositions("zzz", "/rig") != nil {
		t.Error("non-subsequence should yield nil positions")
	}
}

// radarMatchFields buckets matched positions into the rendered columns: a rig
// lights up its id and title, while the joining space and a bare session's raw
// name (no column of their own) never do.
func TestRadarMatchFields(t *testing.T) {
	s := rigStatus{ID: "MIR-1", Title: "add radar"}
	idHits, titleHits := radarMatchFields("mir radar", s)
	for _, i := range []int{0, 1, 2} { // "mir" at the head of the id
		if !idHits[i] {
			t.Errorf("id rune %d not highlighted", i)
		}
	}
	for _, i := range []int{4, 5, 6, 7, 8} { // "radar" run in the title ("add ")
		if !titleHits[i] {
			t.Errorf("title rune %d not highlighted", i)
		}
	}

	// A bare session matched only through its raw name lights up no cell.
	b := bareSession(tmuxSession{Name: "notes-box", Path: "/n"}, "")
	_, tHits := radarMatchFields("box", b)
	if len(tHits) != 0 {
		t.Errorf("session-name-only match lit the title: %v", tHits)
	}
}

// highlightRunes bolds only the hit runes, coalescing a consecutive run into one
// styled segment and leaving an unmatched string untouched.
func TestHighlightRunes(t *testing.T) {
	if got := highlightRunes("plain", nil); got != "plain" {
		t.Errorf("no hits should pass through: got %q", got)
	}
	got := highlightRunes("radar", map[int]bool{0: true, 1: true})
	want := radarMatchStyle.Render("ra") + "dar"
	if got != want {
		t.Errorf("highlightRunes = %q, want %q", got, want)
	}
}

// Under a filter, rows() collapses to one list ranked best-first: the exact-ish
// hit sorts above the scattered one regardless of section order.
func TestRankedRowsOrder(t *testing.T) {
	m := radarModel{
		sessions: []rigStatus{
			bareSession(tmuxSession{Name: "raging", Path: "/raging"}, ""),
			bareSession(tmuxSession{Name: "rig", Path: "/rig"}, ""),
		},
	}
	m.setFilter("rig")
	rows := m.rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].session != "rig" {
		t.Errorf("best match = %q, want rig", rows[0].session)
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0 (snapped to best)", m.cursor)
	}
}

// A bare session renders as a neutral row: the open-ring glyph, no PR tail, and
// its haystack matches on both the path-title and the raw session name.
func TestRadarBareSession(t *testing.T) {
	s := bareSession(tmuxSession{Name: "~-src-rig", Path: "/home/me/src/rig", LastAttached: 42}, "/home/me")
	if !s.bare || s.session != "~-src-rig" {
		t.Fatalf("bareSession identity = %+v", s)
	}
	if s.Title != "~/src/rig" {
		t.Errorf("title = %q, want ~/src/rig", s.Title)
	}
	if !s.Created.Equal(time.Unix(42, 0)) {
		t.Errorf("created = %v, want last-attached", s.Created)
	}
	if g, _ := radarGlyph(s, false); g != "○" {
		t.Errorf("glyph = %q, want ○", g)
	}
	if segs := radarTailSegs(s, false); segs != nil {
		t.Errorf("bare tail = %v, want nil", segs)
	}
	if !fuzzyMatch("rig", radarHaystack(s)) {
		t.Error("haystack should match on path")
	}
	if !fuzzyMatch("src-rig", radarHaystack(s)) {
		t.Error("haystack should match on session name")
	}
}

// The filter narrows the flattened rows across all three sections while the
// PR-enrichment list (rigRows) stays whole, so hiding a rig never starves it of
// its PR fetch.
func TestRadarFilterRows(t *testing.T) {
	m := radarModel{
		inflight: []rigStatus{{Slug: "a", ID: "PROJ-1", Title: "add radar"}},
		parked:   []rigStatus{{Slug: "b", Parked: true, ID: "PROJ-2", Title: "fix waiting"}},
		sessions: []rigStatus{bareSession(tmuxSession{Name: "notes", Path: "/n"}, "")},
	}
	if got := len(m.rows()); got != 3 {
		t.Fatalf("unfiltered rows = %d, want 3", got)
	}
	if got := len(m.rigRows()); got != 2 {
		t.Fatalf("rigRows = %d, want 2", got)
	}

	m.filter = "radar"
	rows := m.rows()
	if len(rows) != 1 || rows[0].Slug != "a" {
		t.Fatalf("filtered rows = %+v, want just PROJ-1", rows)
	}
	// rigRows ignores the filter — enrichment shouldn't depend on what's shown.
	if got := len(m.rigRows()); got != 2 {
		t.Errorf("rigRows under filter = %d, want 2", got)
	}
}

// The NEW picker lists zoxide dirs as create-rows, skipping any dir that
// already has a session (rig, bare session, or the current one), and stands up
// a session at the dir on Enter.
func TestRadarNewRows(t *testing.T) {
	m := radarModel{
		home:     "/home/me",
		current:  tmuxSessionName("/home/me/here"),
		inflight: []rigStatus{{Slug: "rig", Path: "/home/me/work/rig"}},
		sessions: []rigStatus{bareSession(tmuxSession{Name: tmuxSessionName("/home/me/open"), Path: "/home/me/open"}, "/home/me")},
		newDirs: []string{
			"/home/me/work/rig", // has a rig session — skip
			"/home/me/open",     // has a bare session — skip
			"/home/me/here",     // the current session — skip
			"/home/me/fresh",    // no session — keep
			"/home/me/notes",    // no session — keep
		},
	}
	rows := m.newRows()
	if len(rows) != 2 {
		t.Fatalf("newRows = %d (%+v), want 2", len(rows), rows)
	}
	for _, r := range rows {
		if !r.create {
			t.Errorf("row %q not marked create", r.Title)
		}
	}
	if rows[0].Title != "~/fresh" || rows[1].Title != "~/notes" {
		t.Errorf("titles = %q, %q; want ~/fresh, ~/notes", rows[0].Title, rows[1].Title)
	}
	// A create-row reads as a fresh session: plus glyph, no PR tail.
	if g, _ := radarGlyph(rows[0], false); g != "+" {
		t.Errorf("create glyph = %q, want +", g)
	}
	if segs := radarTailSegs(rows[0], false); segs != nil {
		t.Errorf("create tail = %v, want nil", segs)
	}
}

// windowBody keeps the cursor's line on screen: the list holds at the top until
// the cursor would drop off the bottom, then scrolls just enough to follow it,
// and never returns more than the budget.
func TestWindowBody(t *testing.T) {
	cases := []struct {
		name               string
		n, cursor, budget  int
		wantStart, wantEnd int
	}{
		{"fits whole", 5, 2, 10, 0, 5},
		{"zero budget shows all", 5, 2, 0, 0, 5},
		{"cursor near top holds", 30, 3, 10, 0, 10},
		{"cursor at fold holds", 30, 9, 10, 0, 10},
		{"cursor past fold scrolls", 30, 10, 10, 1, 11},
		{"cursor deep scrolls", 30, 25, 10, 16, 26},
		{"cursor at end clamps", 30, 29, 10, 20, 30},
	}
	for _, c := range cases {
		start, end := windowBody(c.n, c.cursor, c.budget)
		if start != c.wantStart || end != c.wantEnd {
			t.Errorf("%s: windowBody(%d,%d,%d) = [%d,%d), want [%d,%d)",
				c.name, c.n, c.cursor, c.budget, start, end, c.wantStart, c.wantEnd)
		}
		if c.budget > 0 && c.n > c.budget {
			if end-start != c.budget {
				t.Errorf("%s: window size = %d, want budget %d", c.name, end-start, c.budget)
			}
			if c.cursor < start || c.cursor >= end {
				t.Errorf("%s: cursor %d not visible in [%d,%d)", c.name, c.cursor, start, end)
			}
		}
	}
}

// The board must never render taller than the popup, whatever the mode or how
// far the cursor has scrolled — that's the whole point of the viewport. Also
// checks the selected row survives into the visible window.
func TestViewFitsHeight(t *testing.T) {
	m := radarModel{
		width: 120,
		inflight: []rigStatus{
			{Slug: "a", ID: "MIR-1", Title: "one", agents: []agentChild{
				{Window: "runtime", Target: "a:0", Context: "doing runtime work"},
				{Window: "rfd", Target: "a:1", Context: "doing rfd work"},
			}},
			{Slug: "b", ID: "MIR-2", Title: "two"},
		},
	}
	for i := range 30 {
		name := "sess" + string(rune('a'+i))
		s := bareSession(tmuxSession{Name: name, Path: "/home/me/" + name, LastAttached: int64(1000 - i)}, "/home/me")
		// Dangle an agent under every other session so children pad the viewport.
		if i%2 == 0 {
			s.agents = []agentChild{{Window: "claude", Target: name + ":0", Context: "working on " + name}}
		}
		m.sessions = append(m.sessions, s)
	}

	for _, height := range []int{8, 14, 20, 40, 200} {
		m.height = height
		for _, cursor := range []int{0, 5, 15, 31} {
			m.cursor = cursor
			if cursor >= len(m.rows()) {
				continue
			}
			lines := strings.Count(m.View(), "\n") + 1
			if lines > height {
				t.Errorf("height=%d cursor=%d: View is %d lines, exceeds popup", height, cursor, lines)
			}
		}
	}
}

// The NEW picker no longer caps its list — windowing handles overflow — so every
// session-less zoxide dir is reachable by scrolling.
func TestRadarNewPickerNoCap(t *testing.T) {
	m := radarModel{mode: modeNew}
	for i := range 200 {
		m.newDirs = append(m.newDirs, "/home/me/dir"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	if got := len(m.rows()); got != 200 {
		t.Errorf("new rows = %d, want all 200", got)
	}
}

// Switching modes wipes the query and snaps the cursor to the top, so each mode
// opens clean on its first row.
func TestRadarEnterMode(t *testing.T) {
	m := radarModel{mode: modeBoard, filter: "stale", cursor: 4}
	m.enter(modeNew)
	if m.mode != modeNew || m.filter != "" || m.cursor != 0 {
		t.Errorf("enter(modeNew) = {mode:%d filter:%q cursor:%d}, want {new empty 0}", m.mode, m.filter, m.cursor)
	}
}

// Drive the input state machine through keystrokes: the board is always a fuzzy
// filter (printable keys narrow it), backspace trims, esc clears a live query
// before it would quit, and ctrl+t opens the NEW picker. Guards the wiring the
// UI depends on.
func TestRadarKeyFlow(t *testing.T) {
	m := radarModel{
		inflight: []rigStatus{{Slug: "a", ID: "PROJ-1", Title: "add radar"}},
	}

	// Bare letters land straight in the filter — no mode to enter first.
	m, _ = m.handleKey("r")
	m, _ = m.handleKey("a")
	if m.mode != modeBoard || m.filter != "ra" {
		t.Fatalf("typing did not filter the board: mode=%d filter=%q", m.mode, m.filter)
	}

	// Backspace trims the query a rune at a time.
	m, _ = m.handleKey("backspace")
	if m.filter != "r" {
		t.Fatalf("backspace: filter = %q, want r", m.filter)
	}

	// esc with a live query clears it but stays on the board.
	m, _ = m.handleKey("esc")
	if m.mode != modeBoard || m.filter != "" {
		t.Fatalf("esc did not clear to a clean board: mode=%d filter=%q", m.mode, m.filter)
	}

	// ctrl+t opens the NEW picker; esc walks back out.
	m, _ = m.handleKey("ctrl+t")
	if m.mode != modeNew {
		t.Fatalf("ctrl+t did not open NEW picker: mode=%d", m.mode)
	}
	m, _ = m.handleKey("esc")
	if m.mode != modeBoard {
		t.Fatalf("esc did not leave NEW picker: mode=%d", m.mode)
	}
}

// stripAgentGlyph peels Claude Code's leading state glyph — the ✳ star or a
// braille spinner frame — leaving the task text; a title with no glyph (a plain
// shell) is returned unchanged so callers can tell agent panes apart.
func TestStripAgentGlyph(t *testing.T) {
	cases := []struct{ in, want string }{
		{"✳ Replace emoji in story", "Replace emoji in story"},
		{"⠂ Add non-rig tmux sessions to radar", "Add non-rig tmux sessions to radar"},
		{"✳ Claude Code", agentPlaceholder},
		{"~/s/g/m/iso - fish", "~/s/g/m/iso - fish"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripAgentGlyph(c.in); got != c.want {
			t.Errorf("stripAgentGlyph(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The board dangles each parent's claude windows as selectable child rows: in
// order, right under the parent, with the window label shown only when there's
// more than one to disambiguate, and the last child closing the tree.
func TestDisplayItemsChildren(t *testing.T) {
	m := radarModel{
		inflight: []rigStatus{{Slug: "a", ID: "mir-1", Title: "build the thing", agents: []agentChild{
			{Window: "runtime", Target: "s:0", Context: "Plan the saga"},
			{Window: "rfd", Target: "s:1", Context: "Draft the RFD"},
		}}},
		sessions: []rigStatus{{bare: true, session: "meet", Title: "~/src/meet", agents: []agentChild{
			{Window: "claude", Target: "meet:0", Context: "Replace emoji"},
		}}},
	}

	items := m.displayItems()
	// Flat, no section headers: parent, child, child, parent, child.
	if len(items) != 5 {
		t.Fatalf("items = %d, want 5", len(items))
	}
	if items[0].header != "" || items[3].header != "" {
		t.Fatalf("unexpected header in flat board: %q ... %q", items[0].header, items[3].header)
	}
	// The rig's two children carry their window labels and the second closes.
	c1, c2 := items[1], items[2]
	if !c1.child || c1.row.childKey != "runtime" || c1.last {
		t.Errorf("child 1 = %+v, want runtime non-last", c1)
	}
	if !c2.child || c2.row.childKey != "rfd" || !c2.last {
		t.Errorf("child 2 = %+v, want rfd last", c2)
	}
	// The session's lone child drops the label (nothing to disambiguate).
	c3 := items[4]
	if !c3.child || c3.row.childKey != "" || c3.row.Title != "Replace emoji" || !c3.last {
		t.Errorf("session child = %+v, want unlabeled last 'Replace emoji'", c3)
	}

	// Children are selectable rows with distinct keys, and switch to their window.
	rows := m.rows()
	if len(rows) != 5 { // 2 parents + 3 children
		t.Fatalf("selectable rows = %d, want 5", len(rows))
	}
	if rowKey(rows[1]) == rowKey(rows[2]) {
		t.Error("sibling children share a rowKey")
	}
	if rows[1].session != "s:0" {
		t.Errorf("child switch target = %q, want s:0", rows[1].session)
	}

	// Under a filter the HUD holds: a matched parent still dangles its children,
	// and rows that miss drop out entirely.
	m.filter = "build" // matches the rig "build the thing", not the meet session
	frows := m.rows()
	if len(frows) != 3 { // matched parent + its two children
		t.Fatalf("filtered rows = %d, want 3 (matched parent keeps its HUD)", len(frows))
	}
	kids := 0
	for _, r := range frows {
		if r.child {
			kids++
		}
	}
	if kids != 2 {
		t.Errorf("filtered children = %d, want 2", kids)
	}
}

// A board's screen lines number the selectable rows — parents and dangled
// children — the same layout the mouse handler hit-tests, so a click maps to the
// row drawn under it. With sections gone the list is flat: no headers, no blanks.
func mouseBoard() radarModel {
	return radarModel{
		height: 40, width: 100,
		prs:       map[string][]rigPR{},
		fetchedAt: map[string]time.Time{},
		inflight: []rigStatus{
			{Slug: "a", ID: "A", Title: "aa", agents: []agentChild{{Window: "w", Target: "a:0", Context: "doing"}}},
			{Slug: "b", ID: "B", Title: "bb"},
		},
		sessions: []rigStatus{{bare: true, session: "s", Title: "~/s"}},
	}
}

func TestBoardLinesAndHitTest(t *testing.T) {
	m := mouseBoard()
	// Flat MRU list (all recency 0 → append order): A(0), child(1), B(2), S(3).
	want := []int{0, 1, 2, 3}
	lines := m.boardLines()
	if len(lines) != len(want) {
		t.Fatalf("lines = %d, want %d", len(lines), len(want))
	}
	for i, w := range want {
		if lines[i].cursor != w {
			t.Errorf("line %d cursor = %d, want %d", i, lines[i].cursor, w)
		}
	}
	// rowAtY maps each screen row directly (no prompt, no windowing at height 40).
	for y, wantCur := range map[int]int{0: 0, 1: 1, 2: 2, 3: 3} {
		if got, ok := m.rowAtY(y); !ok || got != wantCur {
			t.Errorf("rowAtY(%d) = (%d,%v), want (%d,true)", y, got, ok, wantCur)
		}
	}
	// A click past the last row lands nowhere.
	if _, ok := m.rowAtY(4); ok {
		t.Error("rowAtY(4) hit a row past the end")
	}
}

// The wheel walks the cursor; a click selects the row under it, and clicking the
// already-selected row activates it.
func TestRadarMouse(t *testing.T) {
	m := mouseBoard()

	nm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	m = nm.(radarModel)
	if m.cursor != 1 {
		t.Fatalf("wheel down: cursor = %d, want 1", m.cursor)
	}

	// Click the session row (screen line 3 → cursor 3): selects, doesn't act.
	nm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 3})
	m = nm.(radarModel)
	if m.cursor != 3 || m.chosen != nil {
		t.Fatalf("first click: cursor=%d chosen=%v, want select to 3, no activate", m.cursor, m.chosen)
	}

	// Click the same row again: activates it.
	nm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 3})
	m = nm.(radarModel)
	if m.chosen == nil || m.chosen.session != "s" {
		t.Fatalf("second click: chosen = %v, want the session", m.chosen)
	}

	// A release event is ignored (only the press acts).
	m2 := mouseBoard()
	nm, _ = m2.Update(tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, Y: 3})
	if nm.(radarModel).cursor != 0 {
		t.Error("release event moved the cursor")
	}
}

// parseAgentPanes keeps one child per distinct agent: two panes sharing a window
// and the exact same context collapse (a claude mirrored across a split), but
// two different tasks in one window both survive. Shells drop out; the
// placeholder title becomes an empty context.
func TestParseAgentPanes(t *testing.T) {
	now := int64(1_000_000)
	recent, stale := "999990", "900000" // 10s ago (working) vs ~28h ago (idle)
	// fields: session, window, pane, window name, command, window activity, title
	out := strings.Join([]string{
		"s\t0\t0\twin\tclaude\t" + recent + "\t✳ Task A",
		"s\t0\t1\twin\tclaude\t" + recent + "\t✳ Task A",    // dup: same window + context → collapse
		"s\t0\t2\twin\tclaude\t" + recent + "\t⠂ Task B",    // distinct task, same window → keep
		"s\t1\t0\twin\tfish\t" + recent + "\t~/x - fish",    // shell → skip
		"s\t2\t0\tcw\tclaude\t" + stale + "\t✳ Claude Code", // placeholder, idle
	}, "\n")

	kids := parseAgentPanes(out, now)["s"]
	if len(kids) != 3 {
		t.Fatalf("children = %d (%+v), want 3", len(kids), kids)
	}
	if kids[0].Context != "Task A" || kids[0].Target != "s:0.0" || !kids[0].Working {
		t.Errorf("child 0 = %+v, want working Task A at s:0.0", kids[0])
	}
	if kids[1].Context != "Task B" || kids[1].Target != "s:0.2" || !kids[1].Working {
		t.Errorf("child 1 = %+v, want working Task B at s:0.2 (distinct agent kept)", kids[1])
	}
	if kids[2].Context != "" || kids[2].Target != "s:2.0" || kids[2].Working {
		t.Errorf("child 2 = %+v, want idle empty context at s:2.0", kids[2])
	}
}

// Two agents sharing a window are told apart by their context, so the repeated
// window label is suppressed; agents across different windows keep their labels.
func TestDisplayItemsChildLabels(t *testing.T) {
	sameWindow := radarModel{inflight: []rigStatus{{Slug: "a", ID: "A", agents: []agentChild{
		{Window: "claude", Target: "a:0.0", Context: "one"},
		{Window: "claude", Target: "a:0.1", Context: "two"},
	}}}}
	for _, it := range sameWindow.displayItems() {
		if it.child && it.row.childKey != "" {
			t.Errorf("same-window child kept a label: %q", it.row.childKey)
		}
	}

	diffWindow := radarModel{inflight: []rigStatus{{Slug: "a", ID: "A", agents: []agentChild{
		{Window: "runtime", Target: "a:0.0", Context: "one"},
		{Window: "rfd", Target: "a:1.0", Context: "two"},
	}}}}
	labels := 0
	for _, it := range diffWindow.displayItems() {
		if it.child && it.row.childKey != "" {
			labels++
		}
	}
	if labels != 2 {
		t.Errorf("cross-window children labeled = %d, want 2", labels)
	}
}

func TestRadarTruncate(t *testing.T) {
	cases := []struct {
		in   string
		w    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly ten chars!", 18, "exactly ten chars!"},
		{"this one is too long", 10, "this one …"},
		{"clipped", 1, "…"},
		{"clipped", 0, "…"},
	}
	for _, c := range cases {
		if got := radarTruncate(c.in, c.w); got != c.want {
			t.Errorf("radarTruncate(%q, %d) = %q, want %q", c.in, c.w, got, c.want)
		}
	}
}
