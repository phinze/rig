package main

import (
	"fmt"
	"os"
	"sort"
	"time"
)

// runWake reverses rig park: it picks a parked rig (fzf when the arg is ambiguous
// or absent), clears the parked mark, rebuilds the tmux carousel in its hot repo,
// resumes the selected agent's recorded conversation, and attaches.
// This is the "a review came back, back to work" path; a rig that was merged
// instead never needs waking, reap collects it.
func runWake(args []string) error {
	rigs, err := listRigs()
	if err != nil {
		return err
	}
	if len(args) == 1 {
		if pr := parsePRURL(args[0]); pr != nil {
			// A PR URL is a natural handle for a review rig, and carries the repo
			// identity that bare pr-<n> lacks. Resolve it against local manifests.
			found, err := existingReviewRig(rigs, pr)
			if err != nil {
				return err
			}
			if found == nil {
				return fmt.Errorf("no rig for %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
			}
			return activateRig(*found)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	statuses := rigStatuses(rigs, home, time.Now())

	var chosen *rigStatus
	if len(args) > 0 {
		// An explicit wake is idempotent: resolve across every rig so an
		// already-awake target switches instead of reporting "no parked rigs."
		chosen, err = pickRigStatus(statuses, args, "wake rig: ")
		if err != nil {
			return err
		}
	} else {
		parked := statuses[:0]
		for _, s := range statuses {
			if s.Parked {
				parked = append(parked, s)
			}
		}
		statuses = parked
		if len(statuses) == 0 {
			return fmt.Errorf("no parked rigs")
		}
		// Newest first, so the freshest parked rig tops the picker.
		sort.SliceStable(statuses, func(i, j int) bool {
			return statuses[i].Created.After(statuses[j].Created)
		})

		chosen, err = pickRigStatus(statuses, nil, "wake rig: ")
		if err != nil {
			return err
		}
	}
	if chosen == nil {
		return nil
	}

	for _, r := range rigs {
		if r.Path == chosen.Path {
			return activateRig(r)
		}
	}
	return fmt.Errorf("rig disappeared: %s", chosen.Path)
}
