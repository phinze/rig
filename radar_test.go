package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestRadarActionFailureStaysInCurrentModel(t *testing.T) {
	wantErr := errors.New("destination busy")
	m := radarModel{
		filter:        "shell",
		cursor:        0,
		actionPending: true,
		sessions:      []rigStatus{{bare: true, session: "shell", Title: "shell"}},
	}
	updated, cmd := m.Update(radarActionMsg{err: wantErr})
	m = updated.(radarModel)
	if cmd != nil || m.actionPending || m.chosen != nil {
		t.Fatalf("failure left model in action state: pending=%v chosen=%v cmd=%v", m.actionPending, m.chosen, cmd != nil)
	}
	if m.filter != "shell" || m.cursor != 0 || !errors.Is(m.actionErr, wantErr) {
		t.Fatalf("failure lost board state: filter=%q cursor=%d err=%v", m.filter, m.cursor, m.actionErr)
	}
	if view := m.View(); !strings.Contains(view, wantErr.Error()) {
		t.Fatalf("failure is not rendered in current board:\n%s", view)
	}
}

func TestRadarActionSuccessQuitsForFinalSwitch(t *testing.T) {
	m := radarModel{sessions: []rigStatus{{bare: true, session: "shell", Title: "shell"}}}
	m, cmd := m.handleKey("enter")
	if cmd == nil || !m.actionPending || m.chosen != nil {
		t.Fatalf("enter = pending:%v chosen:%v cmd:%v", m.actionPending, m.chosen, cmd != nil)
	}
	updated, quit := m.Update(cmd())
	m = updated.(radarModel)
	if quit == nil || m.actionPending || m.chosen == nil || m.chosen.session != "shell" {
		t.Fatalf("success = pending:%v chosen:%v quit:%v", m.actionPending, m.chosen, quit != nil)
	}
}

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

// In-flight rigs and plain sessions share one last-touched section. Parked rigs
// follow in their own section, also ordered by last touch. Agent activity is a
// live state signal, not an ordering signal.
func TestBoardRowsMRU(t *testing.T) {
	at := func(u int64) *time.Time { tm := time.Unix(u, 0); return &tm }
	m := radarModel{
		attached: map[string]int64{
			tmuxSessionName("/w/live"): 500, // live rig, attached recently
			"sess-old":                 100, // bare session, old attach
		},
		inflight: []rigStatus{
			{Slug: "live", Path: "/w/live", Created: time.Unix(1, 0)},                        // recency 500 (attach)
			{Slug: "agent", Path: "/w/agent", Created: time.Unix(1, 0), LastActive: at(400)}, // activity does not change recency
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
	want := []string{"live", "sess-old", "agent", "parked"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("MRU order = %v, want %v", got, want)
		}
	}
}

func TestRadarCurrentBareSessionIsContextNotDestination(t *testing.T) {
	m := radarModel{home: "/home/me", current: "here", prs: map[string][]rigPR{}}
	m.apply(radarScanMsg{
		attached: map[string]int64{"here": 300, "newer": 200, "older": 100},
		sessions: []tmuxSession{
			{Name: "here", Path: "/home/me/current", LastAttached: 300},
			{Name: "older", Path: "/home/me/older", LastAttached: 100},
			{Name: "newer", Path: "/home/me/newer", LastAttached: 200},
		},
	})
	if m.currentRow == nil || !m.currentRow.bare || m.currentRow.session != "here" {
		t.Fatalf("current row = %+v, want bare session here", m.currentRow)
	}
	rows := m.rows()
	if len(rows) != 2 || rows[0].session != "newer" || rows[1].session != "older" {
		t.Fatalf("destinations = %+v, want newer then older", rows)
	}
	if m.cursor != 0 {
		t.Fatalf("cursor = %d, want most recent other row", m.cursor)
	}
	lines := m.boardLines()
	if len(lines) != 6 || lines[0].item.header != "CURRENT" || !lines[1].item.label || lines[4].cursor != 0 {
		t.Fatalf("board lines = %+v", lines)
	}
	for y := range 4 {
		if _, ok := m.rowAtY(y); ok {
			t.Errorf("current context line %d was selectable", y)
		}
	}
	if got, ok := m.rowAtY(4); !ok || got != 0 {
		t.Errorf("first destination hit = (%d,%v), want (0,true)", got, ok)
	}

	m.setFilter("no-such-session")
	if rows := m.rows(); len(rows) != 0 {
		t.Fatalf("filtered destinations = %+v, want none", rows)
	}
	view := m.View()
	if !strings.Contains(view, "CURRENT") || !strings.Contains(view, "~/current") || !strings.Contains(view, "no other matches") {
		t.Fatalf("filtered board lost current context:\n%s", view)
	}
}

func TestRadarCurrentRigKeepsRigIdentity(t *testing.T) {
	path := "/work/current"
	session := tmuxSessionName(path)
	m := radarModel{current: session, prs: map[string][]rigPR{}}
	m.apply(radarScanMsg{
		statuses: []rigStatus{
			{Slug: "current", ID: "MIR-1", Title: "durable title", Path: path},
			{Slug: "other", ID: "MIR-2", Title: "other", Path: "/work/other"},
		},
		sessions: []tmuxSession{{Name: session, Path: path}},
		agents: map[string][]agentChild{
			session: {{Target: session + ":0", Context: "live task context"}},
		},
		attached: map[string]int64{},
	})
	if m.currentRow == nil || m.currentRow.bare || m.currentRow.Slug != "current" {
		t.Fatalf("current row = %+v, want MIR-1 rig", m.currentRow)
	}
	if len(m.rigRows()) != 2 {
		t.Fatalf("rigRows = %+v, want current and other for PR enrichment", m.rigRows())
	}
	items := m.displayItems()
	// The row keeps its own subject and carries the agent's context alongside
	// it, rather than being renamed by whatever the agent called its session.
	if len(items) < 2 || items[1].row.Title != "durable title" || !items[1].label {
		t.Fatalf("current display = %+v, want the durable title on a non-selectable row", items)
	}
	// The CURRENT row is never the cursor row, so it carries no activity at all.
	if items[1].row.activity != "" {
		t.Fatalf("current activity = %q, want nothing on a non-selectable row", items[1].row.activity)
	}
	for _, row := range m.rows() {
		if row.Slug == "current" {
			t.Fatal("current rig appeared in selectable destinations")
		}
	}
}

// The timestamp rendered in radar's first column is the same timestamp used
// for ordering. A durable touch wins over creation, while a live tmux attach
// can make the row fresher without rewriting the manifest.
func TestRadarTouchedAt(t *testing.T) {
	s := rigStatus{
		Path:        "/w/a",
		Created:     time.Unix(100, 0),
		LastTouched: time.Unix(200, 0),
	}
	m := radarModel{attached: map[string]int64{tmuxSessionName(s.Path): 300}}
	if got := m.touchedAt(s); !got.Equal(time.Unix(300, 0)) {
		t.Fatalf("touchedAt = %v, want live attach time", got)
	}
	delete(m.attached, tmuxSessionName(s.Path))
	if got := m.touchedAt(s); !got.Equal(s.LastTouched) {
		t.Fatalf("touchedAt without session = %v, want durable touch %v", got, s.LastTouched)
	}
}

func TestRadarParentSections(t *testing.T) {
	m := radarModel{
		prs: map[string][]rigPR{
			"waiting":  {{prInfo: prInfo{State: "OPEN"}}},
			"approved": {{prInfo: prInfo{State: "OPEN", Review: "APPROVED"}}},
			"changes":  {{prInfo: prInfo{State: "OPEN", Review: "CHANGES_REQUESTED"}}},
		},
		inflight: []rigStatus{{Slug: "live", Title: "live"}},
		sessions: []rigStatus{{bare: true, session: "shell", Title: "shell"}},
		parked: []rigStatus{
			{Slug: "waiting", Parked: true, Created: time.Unix(1, 0), LastTouched: time.Unix(30, 0), PRs: []rigPR{{prInfo: prInfo{State: "OPEN"}}}},
			{Slug: "approved", Parked: true, Created: time.Unix(2, 0), LastTouched: time.Unix(20, 0), PRs: []rigPR{{prInfo: prInfo{State: "OPEN", Review: "APPROVED"}}}},
			{Slug: "changes", Parked: true, Created: time.Unix(3, 0), LastTouched: time.Unix(10, 0), PRs: []rigPR{{prInfo: prInfo{State: "OPEN", Review: "CHANGES_REQUESTED"}}}},
		},
	}
	sections := m.parentSections()
	if len(sections) != 2 || sections[0].header != "IN FLIGHT" || sections[1].header != "PARKED / AWAITING REVIEW" {
		t.Fatalf("sections = %+v", sections)
	}
	if got := []string{sections[1].rows[0].Slug, sections[1].rows[1].Slug, sections[1].rows[2].Slug}; !slices.Equal(got, []string{"waiting", "approved", "changes"}) {
		t.Fatalf("parked order = %v", got)
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
		}}, true, []string{"#7  "}},
		{"multi repo", rigStatus{PRs: []rigPR{
			{Repo: "o/api", prInfo: prInfo{Number: 7, State: "OPEN"}},
			{Repo: "o/web", prInfo: prInfo{Number: 9, State: "OPEN", Checks: "failing"}},
		}}, true, []string{"api #7 ", "web #9  "}},
		{"parked changes", rigStatus{Parked: true, PRs: []rigPR{
			{Repo: "o/api", prInfo: prInfo{Number: 7, State: "OPEN", Review: "CHANGES_REQUESTED"}},
		}}, true, []string{"changes requested", "#7 "}},
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

// highlightRunesBase wraps each un-matched run in the base style (so a faint
// child line keeps its faint between bolded hits, no reset bleed) while hits
// still get the match style. A nil base is plain passthrough.
func TestHighlightRunesBase(t *testing.T) {
	if got := highlightRunesBase("plain", nil, nil); got != "plain" {
		t.Errorf("nil base, no hits should pass through: got %q", got)
	}
	if got := highlightRunesBase("radar", nil, &radarFaintStyle); got != radarFaintStyle.Render("radar") {
		t.Errorf("no hits with a base should render the whole string faint: got %q", got)
	}
	got := highlightRunesBase("radar", map[int]bool{0: true, 1: true}, &radarFaintStyle)
	want := radarMatchStyle.Render("ra") + radarFaintStyle.Render("dar")
	if got != want {
		t.Errorf("highlightRunesBase = %q, want %q", got, want)
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

// A row is findable by its live agent's task context, not just its path/title:
// two near-identical dev-session paths both match "ups" through path junk, but
// the one whose agent is doing "Evaluate UPS options" carries a clean
// consecutive hit and must rank first. Guards the fix for bare sessions being
// searched by directory instead of the task text the board actually shows.
func TestRankByAgentContext(t *testing.T) {
	nix := rigStatus{
		bare: true, session: "~/src/github.com/phinze/nix-config",
		Title:  "~/src/github.com/phinze/nix-config",
		agents: []agentChild{{Context: "Evaluate UPS options for homelab rack"}},
	}
	media := rigStatus{
		bare: true, session: "~/src/github.com/phinze/media-stuff",
		Title: "~/src/github.com/phinze/media-stuff",
	}
	// Sanity: the decoy really does match "ups" on its path alone, so the win
	// below is the agent context outscoring it, not the decoy dropping out.
	if !fuzzyMatch("ups", radarHaystack(media)) {
		t.Fatal("expected the decoy path to match ups on its own")
	}
	m := radarModel{sessions: []rigStatus{media, nix}}
	m.setFilter("ups")
	rows := m.rows()
	if len(rows) == 0 || rows[0].session != nix.session {
		t.Fatalf("best match = %+v, want the nix-config row (agent context win)", rows[0])
	}
}

// A rig is findable by the repo it works in, not just its ticket id and title —
// the basedir is flat, so the repo is otherwise nowhere on the row. Every repo of
// a multi-repo rig counts, the owner half matches too, and the repo composes with
// a title term the way any two query terms do.
func TestRadarMatchByRepo(t *testing.T) {
	s := rigStatus{
		ID:    "MIR-1484",
		Title: "download metrics: add per-replica label",
		Repos: []string{"mirendev/cloud", "mirendev/infra", "mirendev/rfd"},
	}
	for _, q := range []string{"cloud", "infra", "rfd", "mirendev", "rfd label"} {
		if !fuzzyMatch(q, radarHaystack(s)) {
			t.Errorf("query %q should match a rig in %v", q, s.Repos)
		}
	}
	if fuzzyMatch("runtime", radarHaystack(s)) {
		t.Error("a repo the rig doesn't touch should not match")
	}

	// Repos are match-only: surfacing the row is the whole job, and there's no
	// repo column for a hit to light up.
	idHits, titleHits := radarMatchFields("cloud", s)
	if len(idHits) != 0 || len(titleHits) != 0 {
		t.Errorf("repo-only match lit a cell: id=%v title=%v", idHits, titleHits)
	}
}

// A repo match ranks a rig honestly against the field: typing a repo name puts
// the rigs actually in that repo above one that merely spells it out in prose.
func TestRankByRepo(t *testing.T) {
	inRepo := rigStatus{Slug: "a", ID: "MIR-1", Title: "fix the deploy blip", Repos: []string{"mirendev/runtime"}}
	decoy := rigStatus{Slug: "b", ID: "MIR-2", Title: "document the runtime upgrade path"}
	m := radarModel{inflight: []rigStatus{decoy, inRepo}}
	m.setFilter("runtime")
	rows := m.rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want both (decoy matches on its title)", len(rows))
	}
	if rows[0].Slug != inRepo.Slug {
		t.Errorf("best match = %q, want the rig in mirendev/runtime", rows[0].Slug)
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

// The board must never render taller than the popup, however far the cursor has
// scrolled. That's the whole point of the viewport. Also
// checks the selected row survives into the visible window.
func TestViewFitsHeight(t *testing.T) {
	m := radarModel{
		width: 120, actionErr: errRigBusy,
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

// Drive the input state machine through keystrokes: the board is always a fuzzy
// filter (printable keys narrow it), backspace trims, esc clears a live query
// before it would quit, and ctrl+n embeds the shared new-rig wizard. Guards the
// wiring the UI depends on.
func TestRadarKeyFlow(t *testing.T) {
	m := radarModel{
		inflight: []rigStatus{{Slug: "a", ID: "PROJ-1", Title: "add radar"}},
	}

	// Bare letters land straight in the filter.
	m, _ = m.handleKey("r")
	m, _ = m.handleKey("a")
	if m.filter != "ra" {
		t.Fatalf("typing did not filter the board: filter=%q", m.filter)
	}

	// Backspace trims the query a rune at a time.
	m, _ = m.handleKey("backspace")
	if m.filter != "r" {
		t.Fatalf("backspace: filter = %q, want r", m.filter)
	}

	// esc with a live query clears it but stays on the board.
	m, _ = m.handleKey("esc")
	if m.filter != "" {
		t.Fatalf("esc did not clear to a clean board: filter=%q", m.filter)
	}

	// ctrl+n opens the shared wizard inside the radar; esc returns to the board.
	m, _ = m.handleKey("ctrl+n")
	if m.newRig == nil || m.newRig.phase != newRigKickoff {
		t.Fatalf("ctrl+n did not open new-rig wizard: %#v", m.newRig)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(radarModel)
	if m.newRig != nil {
		t.Fatal("esc did not return from new-rig wizard to board")
	}
}

func TestRadarNewRigSuccessBecomesDestination(t *testing.T) {
	m := radarModel{}
	m, _ = m.handleKey("ctrl+n")
	updated, cmd := m.Update(newRigCreatedMsg{result: newRigResult{
		ID: "new-rig", Basedir: "/work/new-rig", Session: "~/workspaces/new-rig",
	}})
	m = updated.(radarModel)
	if cmd == nil || m.newRig != nil || m.chosen == nil {
		t.Fatalf("creation did not finish radar: wizard=%v chosen=%+v cmd=%v", m.newRig != nil, m.chosen, cmd != nil)
	}
	if !m.chosen.bare || m.chosen.session != "~/workspaces/new-rig" {
		t.Fatalf("created destination = %+v", *m.chosen)
	}
}

func TestRadarParkWakeToggle(t *testing.T) {
	m := radarModel{
		attached: map[string]int64{},
		inflight: []rigStatus{{Slug: "a", ID: "MIR-1", Title: "build it", Path: "/work/a"}},
	}
	m, cmd := m.handleKey("ctrl+p")
	if cmd == nil || !m.parkPending["/work/a"] {
		t.Fatalf("ctrl-p did not start park: pending=%v cmd=%v", m.parkPending, cmd != nil)
	}

	updated, scan := m.Update(radarParkMsg{path: "/work/a", label: "MIR-1", parked: true})
	m = updated.(radarModel)
	if scan == nil || len(m.inflight) != 0 || len(m.parked) != 1 || !m.parked[0].Parked {
		t.Fatalf("park result = inflight:%+v parked:%+v scan:%v", m.inflight, m.parked, scan != nil)
	}
	if m.rows()[0].Slug != "a" {
		t.Fatalf("selection did not follow rig into parked section: %+v", m.rows())
	}

	m, cmd = m.handleKey("ctrl+p")
	if cmd == nil || !m.parkPending["/work/a"] {
		t.Fatalf("ctrl-p did not start wake: pending=%v cmd=%v", m.parkPending, cmd != nil)
	}
	updated, _ = m.Update(radarParkMsg{path: "/work/a", label: "MIR-1", parked: false})
	m = updated.(radarModel)
	if len(m.parked) != 0 || len(m.inflight) != 1 || m.inflight[0].Parked {
		t.Fatalf("wake result = inflight:%+v parked:%+v", m.inflight, m.parked)
	}
}

func TestRadarParkToggleUsesChildRigIdentity(t *testing.T) {
	m := radarModel{inflight: []rigStatus{{
		Slug: "a", ID: "MIR-1", Title: "build it", Path: "/work/a",
		agents: []agentChild{{Target: "s:0", Context: "one"}, {Target: "s:1", Context: "two"}},
	}}}
	rows := m.rows()
	if len(rows) != 2 || rows[0].Path != "/work/a" || rows[0].Slug != "a" {
		t.Fatalf("agent child lost parent identity: %+v", rows)
	}
	m, cmd := m.handleKey("ctrl+p")
	if cmd == nil || !m.parkPending["/work/a"] {
		t.Fatalf("ctrl-p on child did not target parent rig: %+v", m.parkPending)
	}
}

func TestRadarParkToggleRejectsBareSession(t *testing.T) {
	m := radarModel{sessions: []rigStatus{{bare: true, session: "shell", Title: "shell"}}}
	m, cmd := m.handleKey("ctrl+p")
	if cmd != nil || m.actionErr == nil || !strings.Contains(m.actionErr.Error(), "cannot be parked") {
		t.Fatalf("bare-session toggle = err:%v cmd:%v", m.actionErr, cmd != nil)
	}
}

func TestRadarParkToggleReportsBusy(t *testing.T) {
	m := radarModel{parkPending: map[string]bool{"/work/a": true}}
	updated, _ := m.Update(radarParkMsg{path: "/work/a", label: "MIR-1", parked: true, err: errRigBusy})
	m = updated.(radarModel)
	if m.parkPending["/work/a"] || m.actionErr == nil || !strings.Contains(m.actionErr.Error(), "MIR-1") {
		t.Fatalf("busy result = pending:%v err:%v", m.parkPending, m.actionErr)
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

// A rig with one agent collapses to one rig-shaped choice. The rig keeps its own
// title — that is what tells you which rig it is — and the agent's context
// trails it when it adds something. Enter still carries the rig metadata and
// jumps to the exact pane, which was always the useful half of the fold.
func TestDisplayItemsSingleRigAgent(t *testing.T) {
	m := radarModel{inflight: []rigStatus{{
		Slug: "a", ID: "mir-1", Title: "build the thing", Path: "/work/a",
		agents: []agentChild{{Window: "runtime", Target: "s:0.1", Context: "Plan the saga"}},
	}}}

	items := m.displayItems()
	if len(items) != 2 || items[0].header != "IN FLIGHT" {
		t.Fatalf("items = %+v, want header plus one collapsed row", items)
	}
	if items[1].child || items[1].label || items[1].row.Title != "build the thing" {
		t.Errorf("display row = %+v, want a selectable rig-shaped row keeping its subject", items[1])
	}
	if items[1].row.activity != "Plan the saga" {
		t.Errorf("activity = %q, want the agent's title carried verbatim", items[1].row.activity)
	}
	rows := m.rows()
	if len(rows) != 1 || !rows[0].child || rows[0].session != "s:0.1" || rows[0].Slug != "a" {
		t.Fatalf("selectable row = %+v, want exact child target with rig metadata", rows)
	}

	m.filter = "build"
	if rows := m.rows(); len(rows) != 1 || rows[0].Title != "build the thing" {
		t.Fatalf("rows filtered by rig title = %+v, want the collapsed row", rows)
	}
	// The agent's context is in the haystack too, so filtering by what the agent
	// is doing still finds the rig it's doing it in.
	m.filter = "saga"
	if rows := m.rows(); len(rows) != 1 || rows[0].Slug != "a" {
		t.Fatalf("rows filtered by agent context = %+v, want the collapsed row", rows)
	}

	m.inflight[0].agents[0].Context = ""
	m.filter = ""
	row := m.displayItems()[1].row
	if row.Title != "build the thing" || row.activity != "" {
		t.Errorf("contextless row = %q / %q, want the rig title and no activity", row.Title, row.activity)
	}
}

// With several agents the rig is a non-selectable group label and its children
// are the choices, in order. Bare sessions retain their path parent even with a
// lone child because that row is their visible identity.
func TestDisplayItemsChildren(t *testing.T) {
	m := radarModel{
		inflight: []rigStatus{{Slug: "a", ID: "mir-1", Title: "build the thing", Path: "/work/a", agents: []agentChild{
			{Window: "runtime", Target: "s:0", Context: "Plan the saga"},
			{Window: "rfd", Target: "s:1", Context: "Draft the RFD"},
		}}},
		sessions: []rigStatus{{bare: true, session: "meet", Title: "~/src/meet", agents: []agentChild{
			{Window: "claude", Target: "meet:0", Context: "Replace emoji"},
		}}},
	}

	items := m.displayItems()
	// One in-flight header, then parent, child, child, parent, child.
	if len(items) != 6 {
		t.Fatalf("items = %d, want 6", len(items))
	}
	if items[0].header != "IN FLIGHT" {
		t.Fatalf("header = %q, want IN FLIGHT", items[0].header)
	}
	if !items[1].label {
		t.Error("multi-agent rig parent is selectable; want group label")
	}
	if items[4].label {
		t.Error("bare session parent became a group label")
	}
	// The rig's two children carry their window labels and the second closes.
	c1, c2 := items[2], items[3]
	if !c1.child || c1.row.childKey != "runtime" || c1.last {
		t.Errorf("child 1 = %+v, want runtime non-last", c1)
	}
	if !c2.child || c2.row.childKey != "rfd" || !c2.last {
		t.Errorf("child 2 = %+v, want rfd last", c2)
	}
	// The session's lone child drops the label (nothing to disambiguate).
	c3 := items[5]
	if !c3.child || c3.row.childKey != "" || c3.row.Title != "Replace emoji" || !c3.last {
		t.Errorf("session child = %+v, want unlabeled last 'Replace emoji'", c3)
	}

	// The rig label is skipped; its children remain distinct exact-window choices.
	// The bare session and its lone child retain their existing separate choices.
	rows := m.rows()
	if len(rows) != 4 { // 2 rig children + bare parent + bare child
		t.Fatalf("selectable rows = %d, want 4", len(rows))
	}
	if rowKey(rows[0]) == rowKey(rows[1]) {
		t.Error("sibling children share a rowKey")
	}
	if rows[0].session != "s:0" {
		t.Errorf("child switch target = %q, want s:0", rows[0].session)
	}
	if rows[0].Slug != "a" {
		t.Errorf("child lost parent rig identity: %+v", rows[0])
	}
	if !rows[2].bare || rows[2].session != "meet" || !rows[3].child {
		t.Errorf("bare session choices = %+v, want parent then lone child", rows[2:])
	}

	// Under a filter the HUD holds: a matched parent still dangles its children,
	// and rows that miss drop out entirely.
	m.filter = "build" // matches the rig "build the thing", not the meet session
	frows := m.rows()
	if len(frows) != 2 { // matched rig label is skipped; its children remain
		t.Fatalf("filtered rows = %d, want 2 agent choices", len(frows))
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

// A board's screen lines number selectable destinations, the same layout the
// mouse handler hit-tests. A single-agent rig occupies one synthesized line.
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
	// Header, then collapsed A(0), B(1), S(2).
	want := []int{-1, 0, 1, 2}
	lines := m.boardLines()
	if len(lines) != len(want) {
		t.Fatalf("lines = %d, want %d", len(lines), len(want))
	}
	for i, w := range want {
		if lines[i].cursor != w {
			t.Errorf("line %d cursor = %d, want %d", i, lines[i].cursor, w)
		}
	}
	if _, ok := m.rowAtY(0); ok {
		t.Error("section header was mouse-selectable")
	}
	// rowAtY maps the three rows below the header.
	for y, wantCur := range map[int]int{1: 0, 2: 1, 3: 2} {
		if got, ok := m.rowAtY(y); !ok || got != wantCur {
			t.Errorf("rowAtY(%d) = (%d,%v), want (%d,true)", y, got, ok, wantCur)
		}
	}
	// A click past the last row lands nowhere.
	if _, ok := m.rowAtY(4); ok {
		t.Error("rowAtY(4) hit a row past the end")
	}
}

// The inbox banner takes two rows at the very top, so everything below it shifts
// down and every click has to shift with it. It didn't: viewportChrome counted
// the filter prompt but not the banner, so any notification sitting in the inbox
// made a click land two rows low and open the wrong rig.
func TestHitTestShiftsForInboxBanner(t *testing.T) {
	m := mouseBoard()
	m.inbox = []notification{{
		Source: "nix-config-sync", Key: "bump-stalled",
		Level: "error", Title: "input bump stalled",
	}}

	// Banner, blank, header, then A(0), B(1), S(2).
	for _, y := range []int{0, 1, 2} {
		if _, ok := m.rowAtY(y); ok {
			t.Errorf("rowAtY(%d) was selectable; banner and header are not", y)
		}
	}
	for y, wantCur := range map[int]int{3: 0, 4: 1, 5: 2} {
		if got, ok := m.rowAtY(y); !ok || got != wantCur {
			t.Errorf("rowAtY(%d) = (%d,%v), want (%d,true)", y, got, ok, wantCur)
		}
	}
	if _, ok := m.rowAtY(6); ok {
		t.Error("rowAtY(6) hit a row past the end")
	}

	// The banner also spends body budget. Without that, a full board overruns
	// the popup and the last rows render under the footer.
	noInbox := mouseBoard()
	_, want := noInbox.viewportChrome()
	if _, got := m.viewportChrome(); got != want-2 {
		t.Errorf("budget with banner = %d, want %d", got, want-2)
	}
}

// A multi-agent rig still renders its parent, but the label has no cursor index
// and mouse hit-testing lands only on concrete children.
func TestBoardLinesSkipMultiAgentParent(t *testing.T) {
	m := radarModel{
		height: 40, width: 100, prs: map[string][]rigPR{}, fetchedAt: map[string]time.Time{},
		inflight: []rigStatus{{Slug: "a", ID: "A", Title: "aa", agents: []agentChild{
			{Target: "a:0.0", Context: "one"},
			{Target: "a:1.0", Context: "two"},
		}}},
		sessions: []rigStatus{{bare: true, session: "s", Title: "~/s"}},
	}

	lines := m.boardLines()
	want := []int{-1, -1, 0, 1, 2}
	if len(lines) != len(want) {
		t.Fatalf("lines = %d, want %d", len(lines), len(want))
	}
	for i, w := range want {
		if lines[i].cursor != w {
			t.Errorf("line %d cursor = %d, want %d", i, lines[i].cursor, w)
		}
	}
	if _, ok := m.rowAtY(0); ok {
		t.Error("section header was mouse-selectable")
	}
	if _, ok := m.rowAtY(1); ok {
		t.Error("multi-agent parent label was mouse-selectable")
	}
	for y, wantCur := range map[int]int{2: 0, 3: 1, 4: 2} {
		if got, ok := m.rowAtY(y); !ok || got != wantCur {
			t.Errorf("rowAtY(%d) = (%d,%v), want (%d,true)", y, got, ok, wantCur)
		}
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

	// Click the session row (screen line 3 → cursor 2): selects, doesn't act.
	nm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 3})
	m = nm.(radarModel)
	if m.cursor != 2 || m.chosen != nil {
		t.Fatalf("first click: cursor=%d chosen=%v, want select to 2, no activate", m.cursor, m.chosen)
	}

	// Click the same row again: prepares it, then quits for the final switch.
	nm, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 3})
	m = nm.(radarModel)
	if cmd == nil || !m.actionPending || m.chosen != nil {
		t.Fatalf("second click: pending=%v chosen=%v cmd=%v", m.actionPending, m.chosen, cmd != nil)
	}
	nm, quit := m.Update(cmd())
	m = nm.(radarModel)
	if quit == nil || m.chosen == nil || m.chosen.session != "s" {
		t.Fatalf("prepared click: chosen=%v quit=%v, want session", m.chosen, quit != nil)
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
// placeholder and cwd-basename titles become empty contexts.
func TestParseAgentPanes(t *testing.T) {
	now := int64(1_000_000)
	recent, stale := "999990", "900000" // 10s ago (working) vs ~28h ago (idle)
	// fields: session, window, pane, window name, command, window activity, cwd, title
	out := strings.Join([]string{
		"s\t0\t0\twin\tclaude\t" + recent + "\t/work/one\t✳ Task A",
		"s\t0\t1\twin\tclaude\t" + recent + "\t/work/one\t✳ Task A",   // dup: same window + context → collapse
		"s\t0\t2\twin\tclaude\t" + recent + "\t/work/one\t⠂ Task B",   // distinct task, same window → keep
		"s\t1\t0\twin\tfish\t" + recent + "\t/work/x\t~/x - fish",     // shell → skip
		"s\t2\t0\tcw\tclaude\t" + stale + "\t/work/cw\t✳ Claude Code", // placeholder, idle
		"s\t3\t0\tmain/rig\tcodex-raw\t" + recent + "\t/work/rig\t⠴ rig",
		"s\t4\t0\tagy\tagy\t" + recent + "\t/work/agy\tAntigravity",
		"s\t5\t0\tmain/rig\tcodex\t" + recent + "\t/work/rig\tFix sparse radar titles",
	}, "\n")

	kids := parseAgentPanes(out, now)["s"]
	if len(kids) != 6 {
		t.Fatalf("children = %d (%+v), want 6", len(kids), kids)
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
	if kids[3].Context != "" || kids[3].Target != "s:3.0" {
		t.Errorf("child 3 = %+v, want cwd-titled codex with empty context at s:3.0", kids[3])
	}
	if kids[4].Context != "" || kids[4].Target != "s:4.0" {
		t.Errorf("child 4 = %+v, want blank antigravity at s:4.0", kids[4])
	}
	if kids[5].Context != "Fix sparse radar titles" || kids[5].Target != "s:5.0" {
		t.Errorf("child 5 = %+v, want self-titled codex at s:5.0", kids[5])
	}
}

// A rig with several agents still gives an untitled child enough context to be
// useful. The window labels distinguish destinations; the stable task title is
// a better child label than a dash or repeated repo basename.
func TestDisplayItemsUntitledRigChildFallsBackToTaskTitle(t *testing.T) {
	m := radarModel{inflight: []rigStatus{{
		Slug: "a", ID: "mir-1", Title: "Canonicalize deployment attempts", Path: "/work/a",
		agents: []agentChild{
			{Window: "main/runtime", Target: "s:0"},
			{Window: "rfd", Target: "s:1", Context: "Draft the RFD"},
		},
	}}}

	items := m.displayItems()
	if got := items[2].row.Title; got != "Canonicalize deployment attempts" {
		t.Errorf("untitled child = %q, want stable rig title", got)
	}
	if got := items[2].action.Title; got != "Canonicalize deployment attempts" {
		t.Errorf("untitled child action = %q, want stable rig title", got)
	}
	if got := items[3].row.Title; got != "Draft the RFD" {
		t.Errorf("self-titled child = %q, want live context preserved", got)
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

// TestRadarHistorySection covers the regret window's place on the board. It is
// hidden at rest so live work keeps the popup, revealed by a filter (the moment
// this feature exists for is searching for a rig you don't yet know is dead),
// and forced open by ctrl+t. When shown it keeps the store's newest-death-first
// order rather than being re-sorted by urgency like the live sections.
func TestRadarHistorySection(t *testing.T) {
	older := &tombstone{ID: "old-rig", Title: "older", Died: time.Unix(100, 0)}
	newer := &tombstone{ID: "new-rig", Title: "newer", Died: time.Unix(200, 0)}
	base := func() radarModel {
		return radarModel{
			inflight: []rigStatus{{Slug: "live", Title: "live"}},
			history: []rigStatus{
				{ID: newer.ID, Slug: "new-rig", Title: newer.Title, Created: newer.Died, stone: newer},
				{ID: older.ID, Slug: "old-rig", Title: older.Title, Created: older.Died, stone: older},
			},
		}
	}

	// At rest: no history, no header, no rows spent.
	if got := base().parentSections(); len(got) != 2 {
		t.Fatalf("history should be hidden with no filter, got %d sections: %+v", len(got), got)
	}

	// ctrl+t opens it deliberately, newest death first.
	shown := base()
	shown.showHistory = true
	sections := shown.parentSections()
	if len(sections) != 3 || sections[2].header != "RECENTLY TORN DOWN" {
		t.Fatalf("ctrl+t sections = %+v", sections)
	}
	if got := []string{sections[2].rows[0].ID, sections[2].rows[1].ID}; !slices.Equal(got, []string{"new-rig", "old-rig"}) {
		t.Fatalf("history order = %v, want newest death first", got)
	}

	// A filter reveals it without the toggle, ranked like any other section, so
	// searching for a rig you didn't know was dead finds it instead of nothing.
	filtered := base()
	filtered.setFilter("old")
	var found bool
	for _, s := range filtered.parentSections() {
		if s.header != "RECENTLY TORN DOWN" {
			continue
		}
		found = true
		if len(s.rows) != 1 || s.rows[0].ID != "old-rig" {
			t.Errorf("filtered history = %+v, want just old-rig", s.rows)
		}
	}
	if !found {
		t.Error("a filter matching a dead rig should surface the history section")
	}

	// A filter matching nothing dead still spends no rows on history.
	miss := base()
	miss.setFilter("live")
	for _, s := range miss.parentSections() {
		if s.header == "RECENTLY TORN DOWN" {
			t.Errorf("history section shown for a query that matches no tombstone: %+v", s.rows)
		}
	}
}

// ctrl+t is the toggle, and it resets the cursor because the row set changes
// shape underneath it. ctrl+h was rejected deliberately: it's ASCII 0x08, which
// some terminals still send for backspace, and backspace edits the live filter.
func TestRadarHistoryToggleKey(t *testing.T) {
	m := radarModel{
		inflight: []rigStatus{{Slug: "live"}},
		history:  []rigStatus{{ID: "dead", stone: &tombstone{ID: "dead"}}},
		cursor:   1,
	}
	m, _ = m.handleBoardKey("ctrl+t")
	if !m.showHistory {
		t.Fatal("ctrl+t did not open history")
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want reset to 0 after the row set changed", m.cursor)
	}
	m, _ = m.handleBoardKey("ctrl+t")
	if m.showHistory {
		t.Error("ctrl+t did not close history again")
	}
}

// TestRadarHistoryRowsAreNotLiveRigs pins the boundary that keeps a torn-down
// rig from being treated as a live one: the PR fan-out and the parked-toggle
// both walk the live sections, and a history row must not appear there. Fetching
// PRs for a rig that no longer exists would be wasted gh calls at best.
func TestRadarHistoryRowsAreNotLiveRigs(t *testing.T) {
	m := radarModel{
		inflight: []rigStatus{{Slug: "live"}},
		history:  []rigStatus{{ID: "dead", Slug: "dead", stone: &tombstone{ID: "dead"}}},
	}
	for _, r := range m.rigRows() {
		if r.stone != nil {
			t.Errorf("history row %q leaked into the PR fan-out", r.Slug)
		}
	}
	if len(m.rigRows()) != 1 {
		t.Errorf("rigRows = %d, want just the live rig", len(m.rigRows()))
	}
}

// TestTombstoneRowsCarryDeathAsCreated pins the one deliberate lie in the row
// mapping: Created holds the death time, because every ordering and age path on
// the board reads Created and the interesting age for a dead rig is how long
// ago you lost it.
func TestTombstoneRowsCarryDeathAsCreated(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now()
	died := now.Add(-3 * time.Hour)
	if err := writeTombstone(&tombstone{
		Version: tombstoneVersion,
		ID:      "gone",
		Title:   "a thing we lost",
		Basedir: "/home/x/workspaces/gone",
		Died:    died,
		Session: &sessionRef{Agent: "codex", ID: "sid"},
	}); err != nil {
		t.Fatal(err)
	}
	rows := tombstoneRows(now)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if !rows[0].Created.Equal(died) {
		t.Errorf("Created = %s, want the death time %s", rows[0].Created, died)
	}
	if rows[0].Title != "a thing we lost" || rows[0].Slug != "gone" {
		t.Errorf("row = %+v", rows[0])
	}
	if rows[0].stone == nil || !rows[0].stone.resurrectable() {
		t.Error("row lost its tombstone, so Enter would have nothing to resurrect")
	}
}

// A history row's state cell answers "can I get it back", not "what is it
// doing" — the live vocabulary is meaningless for a rig that no longer exists.
func TestRadarStateCellForHistoryRows(t *testing.T) {
	withSession := rigStatus{stone: &tombstone{ID: "a", Session: &sessionRef{Agent: "codex", ID: "sid"}}}
	if got := radarStateCell(withSession, false); got != "↺ codex" {
		t.Errorf("recoverable row = %q, want the resume marker and agent", got)
	}
	without := rigStatus{stone: &tombstone{ID: "b"}}
	if got := radarStateCell(without, true); got != "no session" {
		t.Errorf("unrecoverable row = %q", got)
	}
	// And it must not fall through to the live agent vocabulary.
	if got := radarStateCell(without, false); got == "-" {
		t.Error("history row rendered as a live rig with no session")
	}
}

// The glyph and tail columns both have to know about history rows. Neither can
// fall through to the live paths: the glyph would read as an idle agent, and
// the tail's "not fetched yet" ellipsis would be permanent, since history rows
// are deliberately excluded from the PR fan-out and never get enriched.
func TestRadarHistoryRowRendering(t *testing.T) {
	live := rigStatus{stone: &tombstone{ID: "a", Session: &sessionRef{Agent: "codex", ID: "sid"}}}
	dead := rigStatus{stone: &tombstone{ID: "b"}}

	if g, _ := radarGlyph(live, false); g != "↺" {
		t.Errorf("recoverable glyph = %q, want the restore arrow", g)
	}
	if g, _ := radarGlyph(dead, false); g != "·" {
		t.Errorf("unrecoverable glyph = %q", g)
	}

	segs := radarTailSegs(live, false)
	if len(segs) != 1 || segs[0].plain != "codex" {
		t.Errorf("recoverable tail = %+v, want the agent name", segs)
	}
	if segs := radarTailSegs(dead, false); len(segs) != 1 || segs[0].plain != "no session" {
		t.Errorf("unrecoverable tail = %+v", segs)
	}
	// The unfetched ellipsis is the specific wrong answer we're guarding.
	if segs := radarTailSegs(live, false); segs[0].plain == "…" {
		t.Error("history row showed a pending-PR ellipsis it will never resolve")
	}
}

// A history row's rig is gone, so the park toggle has nothing to stamp. It
// carries its old basedir in Path and would otherwise slip past the emptiness
// guard that catches bare sessions.
func TestRadarParkRefusesHistoryRows(t *testing.T) {
	m := radarModel{
		history:     []rigStatus{{ID: "dead", Path: "/home/x/workspaces/dead", stone: &tombstone{ID: "dead"}}},
		showHistory: true,
	}
	m, cmd := m.handleBoardKey("ctrl+p")
	if cmd != nil {
		t.Error("ctrl+p issued a park command for a torn-down rig")
	}
	if m.actionErr == nil || !strings.Contains(m.actionErr.Error(), "torn down") {
		t.Errorf("actionErr = %v, want an explanation naming the state", m.actionErr)
	}
	if len(m.parkPending) != 0 {
		t.Errorf("park marked pending for a rig that doesn't exist: %v", m.parkPending)
	}
}

func TestRadarIDCellSuppressesTitleEcho(t *testing.T) {
	// A `rig new` rig's id is its title slugified, so drawing both prints the
	// same sentence twice in one row.
	title := "brainstorming a way to embed a presentation into circulate"
	loose := rigStatus{ID: kickoffID(title), Title: title}
	if cell := newRadarIDCell(loose); !cell.empty() {
		t.Errorf("loose rig drew its own title back as an id: %q", cell.plain())
	}
	// An authoring rig picked up from an unlinked PR is also kind-loose, but
	// its id says something the title doesn't, so it survives.
	pr := rigStatus{ID: "pr-1153", Title: "Add miren top for cluster-wide resource usage"}
	if cell := newRadarIDCell(pr); cell.id != "pr-1153" {
		t.Errorf("unlinked PR pickup = %q, want pr-1153", cell.id)
	}
}

func TestRadarKindMarksTheLeftEdge(t *testing.T) {
	for name, tc := range map[string]struct {
		row  rigStatus
		want rigKind
	}{
		"ticket":  {rigStatus{ID: "mir-1689", Title: "Expose coordinator startup", Tracker: "linear", TrackerID: "MIR-1689"}, rigKindTicket},
		"review":  {rigStatus{ID: "pr-1153", Title: "Add miren top", Kind: "review"}, rigKindReview},
		"project": {rigStatus{ID: "project-boot-dependency-graph", Title: "Boot dependency graph", Kind: "project", Tracker: "linear"}, rigKindProject},
	} {
		if got := rigKindOf(tc.row); got != tc.want {
			t.Errorf("%s: rigKindOf = %v, want %v", name, got, tc.want)
		}
		want := rigKindGlyph(tc.want)
		m := radarModel{width: 120, inflight: []rigStatus{tc.row}, prs: map[string][]rigPR{tc.row.Slug: nil}}
		if got := m.kindCell(tc.row, m.columns(m.displayItems())); got != want+" " {
			t.Errorf("%s: left-edge cell = %q, want %q", name, got, want+" ")
		}
	}
	// Every kind that draws a glyph draws a distinct one, or the marker tells
	// you nothing it didn't already.
	seen := map[string]rigKind{}
	for _, k := range []rigKind{rigKindTicket, rigKindReview, rigKindProject} {
		g := rigKindGlyph(k)
		if g == "" {
			t.Errorf("kind %v has no glyph", k)
		}
		if prev, dup := seen[g]; dup {
			t.Errorf("kinds %v and %v share glyph %q", prev, k, g)
		}
		seen[g] = k
	}
	if rigKindGlyph(rigKindLoose) != "" {
		t.Error("a loose rig should draw no kind glyph")
	}
}

func TestRadarIDCellSkipsBareAndTombstones(t *testing.T) {
	if cell := newRadarIDCell(rigStatus{bare: true, session: "shell", Title: "shell"}); !cell.empty() {
		t.Errorf("bare session drew an id cell: %q", cell.plain())
	}
	stone := rigStatus{ID: "mir-1755", Title: "Forced server shutdown", stone: &tombstone{ID: "mir-1755"}}
	if cell := newRadarIDCell(stone); cell.id != "mir-1755" {
		t.Errorf("tombstone cell = %q, want a bare id", cell.id)
	}
}

func TestRadarIDCellClipsLongIDs(t *testing.T) {
	long := rigStatus{ID: "project-app-deploy-visibility-in-miren-cloud", Title: "App & Deploy Visibility", Kind: "project", Tracker: "linear"}
	cell := newRadarIDCell(long)
	if !cell.clipped {
		t.Fatal("long id was not clipped")
	}
	if w := len([]rune(cell.id)); w != radarMaxID {
		t.Errorf("clipped id is %d runes, want %d", w, radarMaxID)
	}
}

func TestRadarLongIDDoesNotStarveThePRTail(t *testing.T) {
	// The collapse loop drops the tail before the id, so an uncapped id column
	// meant one long kickoff slug took every PR and CI glyph on the board down
	// with it. This is that board.
	kickoff := "brainstorming a way to embed a presentation plus quiz into circulate on member signup"
	hog := rigStatus{ID: kickoffID(kickoff), Slug: "hog", Title: kickoff}
	prs := []rigPR{{Repo: "mirendev/miren", Branch: "top", prInfo: prInfo{Number: 1153, State: "OPEN", Checks: "passing"}}}
	review := rigStatus{
		ID: "pr-1153", Slug: "pr-1153", Kind: "review",
		Title: "Add miren top for cluster-wide resource usage",
		PRs:   prs,
	}
	m := radarModel{
		width:    100,
		inflight: []rigStatus{hog, review},
		prs: map[string][]rigPR{
			"hog":     {},
			"pr-1153": prs,
		},
	}
	cols := m.columns(m.displayItems())
	if cols.wTail == 0 {
		t.Fatalf("PR tail collapsed on a 100-column board: %+v", cols)
	}
	if cols.wID > radarMaxID {
		t.Errorf("id column is %d wide, want at most %d", cols.wID, radarMaxID)
	}
	if view := m.View(); !strings.Contains(view, "#1153") {
		t.Errorf("PR tail is missing from the board:\n%s", view)
	}
}

func TestRadarRigKindFallsBackToTheID(t *testing.T) {
	// Rigs made before the manifest recorded a tracker still read as tickets,
	// or the marker looks unreliable rather than absent.
	legacy := rigStatus{ID: "mir-1364", Title: "deploy Chatto on Miren guide"}
	if got := rigKindOf(legacy); got != rigKindTicket {
		t.Errorf("trackerless mir-1364 = %v, want ticket", got)
	}
	// But rig's own reserved PR id is not a team prefix.
	for _, row := range []rigStatus{
		{ID: "pr-1153", Title: "Add miren top"},
		{ID: "lets-try-depot-ci-again", Title: "lets try depot ci again"},
		{ID: "fix-500-errors", Title: "fix 500 errors"},
	} {
		if got := rigKindOf(row); got != rigKindLoose {
			t.Errorf("%s = %v, want loose", row.ID, got)
		}
	}
}

// Radar makes no judgment about what an agent called its session; it just never
// lets that name stand in for the rig's own. Identity on every row, the agent's
// title only on the row you've already picked out.
func TestRadarActivityRidesTheCursorRowOnly(t *testing.T) {
	subject := "Report runtime-owned app health alongside entity sync"
	rows := []rigStatus{
		{ID: "mir-1777", Slug: "a", Title: subject, Tracker: "linear",
			agents: []agentChild{{Target: "s:0", Context: "MIR-1777"}}},
		{ID: "mir-1762", Slug: "b", Title: "Exercise entity sync", Tracker: "linear",
			agents: []agentChild{{Target: "s:1", Context: "Plan the saga"}}},
	}
	m := radarModel{width: 120, inflight: rows, prs: map[string][]rigPR{"a": nil, "b": nil}}

	view := m.View()
	if !strings.Contains(view, subject) {
		t.Fatalf("subject missing from the board:\n%s", view)
	}
	// Cursor is on the first row: it shows its agent's title, however little
	// that title says, and the second row shows none of its own.
	if !strings.Contains(view, subject+" · MIR-1777") {
		t.Errorf("cursor row did not carry its agent title:\n%s", view)
	}
	if strings.Contains(view, "Plan the saga") {
		t.Errorf("a row that isn't the cursor drew its agent title:\n%s", view)
	}

	m.cursor = 1
	view = m.View()
	if !strings.Contains(view, "Exercise entity sync · Plan the saga") {
		t.Errorf("moving the cursor did not move the activity:\n%s", view)
	}
	if strings.Contains(view, subject+" · ") {
		t.Errorf("the old cursor row kept its agent title:\n%s", view)
	}
}

// Whatever an agent writes, the row it sits on still says which rig it is.
func TestRadarActivityNeverCrowdsOutTheSubject(t *testing.T) {
	row := rigStatus{
		ID: "mir-1777", Slug: "a", Tracker: "linear",
		Title:  "Report runtime-owned app health alongside entity sync",
		agents: []agentChild{{Target: "s:0", Context: strings.Repeat("a very long agent title ", 12)}},
	}
	for _, width := range []int{50, 80, 200} {
		m := radarModel{width: width, inflight: []rigStatus{row}, prs: map[string][]rigPR{"a": nil}}
		view := m.View()
		if !strings.Contains(view, "Report runtime") {
			t.Errorf("width %d dropped the subject:\n%s", width, view)
		}
		for _, line := range strings.Split(view, "\n") {
			if !strings.Contains(line, "Report runtime") {
				continue // the footer has always run its own width
			}
			if w := lipgloss.Width(line); w > width {
				t.Errorf("width %d produced a %d-cell row: %q", width, w, line)
			}
		}
	}
}

// Kind holds the same hard-left edge on radar that it holds on ls and sweep, and
// pads a kindless row so the age column beneath never drifts.
func TestRadarKindColumnKeepsTheGridAligned(t *testing.T) {
	rows := []rigStatus{
		{ID: "mir-1689", Slug: "a", Title: "Expose coordinator startup", Tracker: "linear"},
		{ID: "pr-1153", Slug: "b", Title: "Add miren top", Kind: "review"},
		{Slug: "c", Title: "lets try depot ci again", ID: kickoffID("lets try depot ci again")},
	}
	m := radarModel{width: 120, inflight: rows, prs: map[string][]rigPR{"a": nil, "b": nil, "c": nil}}
	if got := m.columns(m.displayItems()).wKind; got != 2 {
		t.Fatalf("kind column = %d, want 2", got)
	}
	ageAt := -1
	for _, want := range []string{"Expose coordinator", "Add miren top", "lets try depot"} {
		for _, line := range strings.Split(m.View(), "\n") {
			if !strings.Contains(line, want) {
				continue
			}
			at := runeIndex(line, "0m")
			if ageAt < 0 {
				ageAt = at
			} else if at != ageAt {
				t.Errorf("row %q starts its age at %d, want %d", want, at, ageAt)
			}
			break
		}
	}

	// A board with no kinds anywhere spends no width on the marker.
	loose := []rigStatus{{Slug: "c", Title: "lets try depot ci again", ID: kickoffID("lets try depot ci again")}}
	lm := radarModel{width: 120, inflight: loose, prs: map[string][]rigPR{"c": nil}}
	if got := lm.columns(lm.displayItems()).wKind; got != 0 {
		t.Errorf("all-loose board reserved %d columns for kind, want 0", got)
	}
}
