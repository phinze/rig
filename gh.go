package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// prInfo is the GitHub view of a rig's branch: enough to gate reap on a real
// merge and to show PR state on the ls call-sheet. The json tags let ls emit it
// directly under each rig in --format=json.
type prInfo struct {
	Number int    `json:"number"`
	State  string `json:"state"` // OPEN | CLOSED | MERGED
	URL    string `json:"url"`
	Checks string `json:"checks,omitempty"` // passing | failing | pending | ""
}

// checkItem is one entry in a PR's statusCheckRollup. GitHub mixes two shapes:
// modern CheckRuns carry status+conclusion, legacy StatusContexts carry state.
type checkItem struct {
	Typename   string `json:"__typename"`
	Status     string `json:"status"`     // CheckRun: COMPLETED | IN_PROGRESS | QUEUED | ...
	Conclusion string `json:"conclusion"` // CheckRun: SUCCESS | FAILURE | SKIPPED | ...
	State      string `json:"state"`      // StatusContext: SUCCESS | PENDING | FAILURE | ERROR
}

// rollupChecks collapses a PR's checks to one word, mirroring how gh itself
// summarizes: any hard failure wins, else anything still running reads pending,
// else passing. No checks at all is "" so ls shows a bare PR with no CI noise.
func rollupChecks(items []checkItem) string {
	failing, pending := false, false
	for _, it := range items {
		if it.Typename == "StatusContext" {
			switch it.State {
			case "FAILURE", "ERROR":
				failing = true
			case "PENDING", "EXPECTED":
				pending = true
			}
			continue
		}
		// CheckRun: not-yet-COMPLETED is pending; a bad conclusion is failing.
		if it.Status != "COMPLETED" {
			pending = true
			continue
		}
		switch it.Conclusion {
		case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
			failing = true
		}
	}
	switch {
	case failing:
		return "failing"
	case pending:
		return "pending"
	case len(items) > 0:
		return "passing"
	default:
		return ""
	}
}

// prForBranch asks gh for the PR whose head is branch in repo (owner/repo),
// including its CI rollup. It returns nil (and no error) when gh finds no PR
// for the branch — the ordinary "pushed but not PR'd yet" or "no such branch"
// case, which callers read as "nothing merged, nothing to show". A genuine gh
// failure (offline, no auth) comes back as an error so callers can fail closed
// rather than guess a rig is mergeable when they simply couldn't ask. Querying
// one branch at a time keeps it precise (the rig's own recorded branch) and
// light (one PR's rollup, not a repo-wide list), and ls fans these out
// concurrently.
func prForBranch(nameWithOwner, branch string) (*prInfo, error) {
	cmd := exec.Command("gh", "pr", "view", branch,
		"-R", nameWithOwner, "--json", "number,state,url,statusCheckRollup")
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
		Number            int         `json:"number"`
		State             string      `json:"state"`
		URL               string      `json:"url"`
		StatusCheckRollup []checkItem `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("parsing gh pr view %s (%s): %w", branch, nameWithOwner, err)
	}
	return &prInfo{Number: v.Number, State: v.State, URL: v.URL, Checks: rollupChecks(v.StatusCheckRollup)}, nil
}
