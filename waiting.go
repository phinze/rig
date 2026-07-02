package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"
)

// runWaiting sweeps parked rigs and reports where each stands with review, so
// you're told when to care again instead of popping into a rig to find out.
// It's the assist that pairs with park: park sends a rig quiet, this surfaces
// the one whose review came back. It costs a gh round-trip per parked PR, so
// it's an explicit command rather than something the instant switcher pays for.
func runWaiting(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: rig waiting")
	}
	rigs, err := listRigs()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	statuses := rigStatuses(rigs, home, time.Now())

	parked := statuses[:0]
	for _, s := range statuses {
		if s.Parked {
			parked = append(parked, s)
		}
	}
	statuses = parked
	if len(statuses) == 0 {
		fmt.Fprintln(os.Stderr, "rig: no parked rigs")
		return nil
	}
	enrichWithPRs(statuses)

	// Order by how much each wants you: a change request first (go wake it),
	// then approved (go merge it), then merged (done, reap collects it),
	// waiting last, ties broken oldest-first.
	rank := map[string]int{
		"changes requested": 0,
		"approved":          1,
		"merged":            2,
		"waiting":           3,
		"no PR":             4,
	}
	type row struct {
		s    rigStatus
		disp string
	}
	rows := make([]row, len(statuses))
	for i, s := range statuses {
		rows[i] = row{s, parkedDisposition(s.PRs)}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rank[rows[i].disp] != rank[rows[j].disp] {
			return rank[rows[i].disp] < rank[rows[j].disp]
		}
		return rows[i].s.Created.Before(rows[j].s.Created)
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			r.s.ID, r.disp, waitingAction(r.disp, r.s.ID), r.s.Title)
	}
	return w.Flush()
}

// parkedDisposition summarizes where a parked rig stands with review, folded
// from its PRs' state and review decision. Precedence favors what needs you: a
// change request outranks everything (go wake it), then a clean sweep of merges
// (done), then approval (go merge), else it's still waiting. A rig with no PR at
// all is called out rather than silently bucketed.
func parkedDisposition(prs []rigPR) string {
	if len(prs) == 0 {
		return "no PR"
	}
	anyChanges, allMerged, allResolved := false, true, true
	for _, pr := range prs {
		if pr.Review == "CHANGES_REQUESTED" {
			anyChanges = true
		}
		if pr.State != "MERGED" {
			allMerged = false
			// A still-open PR only counts as resolved once it's approved.
			if pr.Review != "APPROVED" {
				allResolved = false
			}
		}
	}
	switch {
	case anyChanges:
		return "changes requested"
	case allMerged:
		return "merged"
	case allResolved:
		return "approved"
	default:
		return "waiting"
	}
}

// waitingAction is the copy-pasteable (or plain-English) next step for a parked
// rig's disposition.
func waitingAction(disp, id string) string {
	switch disp {
	case "changes requested":
		return "rig wake " + id
	case "approved":
		return "merge it"
	case "merged":
		return "done (reap)"
	default:
		return "-"
	}
}
