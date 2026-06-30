package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// prInfo is the GitHub view of a rig's branch: enough to gate reap on a real
// merge and to show PR state on the ls call-sheet.
type prInfo struct {
	Number int
	State  string // OPEN | CLOSED | MERGED
	URL    string
}

// prForBranch asks gh for the PR whose head is branch in repo (owner/repo).
// It returns nil (and no error) when gh finds no PR for the branch — the
// ordinary "pushed but not PR'd yet" or "no such branch" case, which callers
// read as "nothing merged, nothing to show". A genuine gh failure (offline, no
// auth) comes back as an error so callers can fail closed rather than guess a
// rig is mergeable when they simply couldn't ask.
func prForBranch(nameWithOwner, branch string) (*prInfo, error) {
	cmd := exec.Command("gh", "pr", "view", branch,
		"-R", nameWithOwner, "--json", "number,state,url")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// gh exits non-zero with this on stderr when the branch has no PR.
			// That's an answer ("none"), not a failure, so don't propagate it.
			if strings.Contains(strings.ToLower(string(ee.Stderr)), "no pull requests found") {
				return nil, nil
			}
			return nil, fmt.Errorf("gh pr view %s (%s): %s",
				branch, nameWithOwner, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gh pr view %s (%s): %w", branch, nameWithOwner, err)
	}
	var v struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("parsing gh pr view %s (%s): %w", branch, nameWithOwner, err)
	}
	return &prInfo{Number: v.Number, State: v.State, URL: v.URL}, nil
}
