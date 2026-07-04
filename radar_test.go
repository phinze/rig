package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

// The parked section ranks by disposition with created-oldest breaking ties,
// and unfetched rigs sink until their PR fan-out lands.
func TestRadarSortParked(t *testing.T) {
	day := func(n int) time.Time { return time.Date(2026, 7, n, 0, 0, 0, 0, time.UTC) }
	statuses := []rigStatus{
		{Slug: "unfetched", Parked: true, Created: day(1)},
		{Slug: "merged", Parked: true, Created: day(2), PRs: []rigPR{{prInfo: prInfo{State: "MERGED"}}}},
		{Slug: "changes-new", Parked: true, Created: day(4), PRs: []rigPR{{prInfo: prInfo{State: "OPEN", Review: "CHANGES_REQUESTED"}}}},
		{Slug: "changes-old", Parked: true, Created: day(3), PRs: []rigPR{{prInfo: prInfo{State: "OPEN", Review: "CHANGES_REQUESTED"}}}},
		{Slug: "approved", Parked: true, Created: day(5), PRs: []rigPR{{prInfo: prInfo{State: "OPEN", Review: "APPROVED"}}}},
	}
	prs := map[string][]rigPR{}
	for _, s := range statuses {
		if s.Slug != "unfetched" {
			prs[s.Slug] = s.PRs
		}
	}

	radarSortParked(statuses, prs)

	want := []string{"changes-old", "changes-new", "approved", "merged", "unfetched"}
	for i, w := range want {
		if statuses[i].Slug != w {
			t.Fatalf("order[%d] = %q, want %q", i, statuses[i].Slug, w)
		}
	}
}

// In-flight order mirrors switch: most-recently-attached first, sessionless
// rigs sinking, ties broken newest-created.
func TestRadarSortInflight(t *testing.T) {
	day := func(n int) time.Time { return time.Date(2026, 7, n, 0, 0, 0, 0, time.UTC) }
	statuses := []rigStatus{
		{Slug: "stale", Path: "/w/stale", Created: day(1)},
		{Slug: "fresh", Path: "/w/fresh", Created: day(2)},
		{Slug: "recent", Path: "/w/recent", Created: day(1)},
	}
	attached := map[string]int64{tmuxSessionName("/w/recent"): 100}

	radarSortInflight(statuses, attached)

	want := []string{"recent", "fresh", "stale"}
	for i, w := range want {
		if statuses[i].Slug != w {
			t.Fatalf("order[%d] = %q, want %q", i, statuses[i].Slug, w)
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

// The PR cache round-trips through disk, and entries past the save horizon
// are dropped rather than resurrected stale.
func TestRadarCacheRoundTrip(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
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
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
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

// Bare tmux sessions sort most-recently-attached first (Created carries the
// last-attached stamp), never-attached sessions sink, and name breaks ties.
func TestRadarSortSessions(t *testing.T) {
	sess := func(name string, attached int64) rigStatus {
		s := rigStatus{bare: true, session: name}
		if attached > 0 {
			s.Created = time.Unix(attached, 0)
		}
		return s
	}
	sessions := []rigStatus{
		sess("never", 0),
		sess("old", 100),
		sess("recent", 300),
		sess("also-never", 0),
	}

	radarSortSessions(sessions)

	want := []string{"recent", "old", "also-never", "never"}
	for i, w := range want {
		if sessions[i].session != w {
			t.Fatalf("order[%d] = %q, want %q", i, sessions[i].session, w)
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

// Under a filter, rows() collapses to one list ranked best-first: the exact-ish
// hit sorts above the scattered one regardless of section order.
func TestRankedRowsOrder(t *testing.T) {
	m := radarModel{
		mode: modeBoardFilter,
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

	m.mode = modeBoardFilter
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

// The NEW picker caps its list so a long zoxide history can't overflow the
// popup: the fixed fallback before we know a height, then a height-derived
// budget once the popup reports its size, so nothing lands below the fold.
func TestRadarNewPickerCap(t *testing.T) {
	m := radarModel{mode: modeNew}
	for i := range 200 {
		m.newDirs = append(m.newDirs, "/home/me/dir"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}

	// No height yet → fixed fallback.
	if got := len(m.rows()); got != radarNewCap {
		t.Errorf("fallback rows = %d, want %d", got, radarNewCap)
	}

	// A short popup shows only what fits above the footer.
	m.height = 24
	if want := 24 - radarNewChrome; len(m.rows()) != want {
		t.Errorf("rows at height 24 = %d, want %d", len(m.rows()), want)
	}

	// A tiny popup still shows at least one row rather than none.
	m.height = 3
	if got := len(m.rows()); got != 1 {
		t.Errorf("rows at height 3 = %d, want 1", got)
	}
}

// Switching modes wipes the query and snaps the cursor to the top, so each mode
// opens clean on its first row.
func TestRadarEnterMode(t *testing.T) {
	m := radarModel{mode: modeBoardFilter, filter: "stale", cursor: 4}
	m.enter(modeNew)
	if m.mode != modeNew || m.filter != "" || m.cursor != 0 {
		t.Errorf("enter(modeNew) = {mode:%d filter:%q cursor:%d}, want {new empty 0}", m.mode, m.filter, m.cursor)
	}
}

// Drive the mode state machine through keystrokes: the board is command-mode
// (letters are verbs), `/` opens filter entry where letters are text, esc walks
// back, and `n` opens the NEW picker. Guards the wiring the UI depends on.
func TestRadarKeyFlow(t *testing.T) {
	m := radarModel{
		inflight: []rigStatus{{Slug: "a", ID: "PROJ-1", Title: "add radar"}},
	}

	// A bare letter on the board is a command, not filter input.
	m, _ = m.handleKey("x")
	if m.mode != modeBoard || m.filter != "" {
		t.Fatalf("letter on board leaked into filter: mode=%d filter=%q", m.mode, m.filter)
	}

	// Slash opens filter entry; now letters are text.
	m, _ = m.handleKey("/")
	if m.mode != modeBoardFilter {
		t.Fatalf("/ did not open filter: mode=%d", m.mode)
	}
	m, _ = m.handleKey("r")
	m, _ = m.handleKey("a")
	if m.filter != "ra" {
		t.Fatalf("filter = %q, want ra", m.filter)
	}

	// esc walks back to the board and clears the query.
	m, _ = m.handleKey("esc")
	if m.mode != modeBoard || m.filter != "" {
		t.Fatalf("esc did not return to a clean board: mode=%d filter=%q", m.mode, m.filter)
	}

	// n opens the NEW picker.
	m, _ = m.handleKey("n")
	if m.mode != modeNew {
		t.Fatalf("n did not open NEW picker: mode=%d", m.mode)
	}
	m, _ = m.handleKey("esc")
	if m.mode != modeBoard {
		t.Fatalf("esc did not leave NEW picker: mode=%d", m.mode)
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
