package main

import (
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
