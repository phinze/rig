package main

import "testing"

// parkedDisposition folds a rig's PRs into a single review disposition, with a
// precedence that surfaces what needs action first.
func TestParkedDisposition(t *testing.T) {
	pr := func(state, review string) rigPR {
		return rigPR{prInfo: prInfo{State: state, Review: review}}
	}
	cases := []struct {
		name string
		prs  []rigPR
		want string
	}{
		{"no PR", nil, "no PR"},
		{"open, unreviewed, is waiting", []rigPR{pr("OPEN", "")}, "waiting"},
		{"open, review required, is waiting", []rigPR{pr("OPEN", "REVIEW_REQUIRED")}, "waiting"},
		{"approved but open is mergeable", []rigPR{pr("OPEN", "APPROVED")}, "approved"},
		{"all merged is merged", []rigPR{pr("MERGED", "APPROVED")}, "merged"},
		{"changes requested outranks all", []rigPR{
			pr("MERGED", "APPROVED"),
			pr("OPEN", "CHANGES_REQUESTED"),
		}, "changes requested"},
		{"merged + still-waiting reads waiting", []rigPR{
			pr("MERGED", "APPROVED"),
			pr("OPEN", ""),
		}, "waiting"},
		{"merged + approved reads approved", []rigPR{
			pr("MERGED", "APPROVED"),
			pr("OPEN", "APPROVED"),
		}, "approved"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parkedDisposition(c.prs); got != c.want {
				t.Errorf("parkedDisposition = %q, want %q", got, c.want)
			}
		})
	}
}
