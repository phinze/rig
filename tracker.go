package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

type task struct {
	Identifier string // e.g. "MIR-75"
	Title      string
	BranchName string // e.g. "phinze/mir-75-add-zig-stack"
}

var linearIDRe = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)

// resolveIssueID turns `rig up` args into a Linear identifier. An exact
// identifier is used directly; no args opens an fzf picker over assigned/open
// issues; anything else is treated as a search query feeding the same picker.
// Returns "" (no error) when the user cancels the picker.
func resolveIssueID(args []string) (string, error) {
	if len(args) == 1 && linearIDRe.MatchString(args[0]) {
		return args[0], nil
	}

	var listArgs []string
	if len(args) == 0 {
		listArgs = []string{"issues", "list", "--limit", "25"}
	} else {
		listArgs = []string{"issues", "search", strings.Join(args, " ")}
	}

	cands, err := fetchIssues(listArgs...)
	if err != nil {
		return "", err
	}
	rows := make([]string, len(cands))
	for i, c := range cands {
		rows[i] = fmt.Sprintf("%s\t%s\t%s", c.Identifier, c.State, c.Title)
	}
	sel, err := fzfSelect(rows, "Pick issue: ")
	if err != nil {
		return "", err
	}
	if sel == "" {
		return "", nil
	}
	id, _, _ := strings.Cut(sel, "\t")
	return strings.TrimSpace(id), nil
}

type issueCandidate struct {
	Identifier string
	State      string
	Title      string
}

func fetchIssues(linearisArgs ...string) ([]issueCandidate, error) {
	out, err := exec.Command("linearis", linearisArgs...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("linearis: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("linearis: %w", err)
	}
	var raw []struct {
		Identifier string `json:"identifier"`
		State      struct {
			Name string `json:"name"`
		} `json:"state"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing linearis output: %w", err)
	}
	cands := make([]issueCandidate, len(raw))
	for i, r := range raw {
		cands[i] = issueCandidate{Identifier: r.Identifier, State: r.State.Name, Title: r.Title}
	}
	return cands, nil
}

func resolveTask(id string) (task, error) {
	if !linearIDRe.MatchString(id) {
		return task{}, fmt.Errorf("only Linear identifiers (e.g. MIR-75) are supported right now")
	}

	out, err := exec.Command("linearis", "issues", "read", id).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return task{}, fmt.Errorf("linearis: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return task{}, fmt.Errorf("linearis: %w", err)
	}

	var raw struct {
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
		BranchName string `json:"branchName"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return task{}, fmt.Errorf("parsing linearis output: %w", err)
	}
	if raw.BranchName == "" {
		return task{}, fmt.Errorf("linearis returned no branchName for %s", id)
	}
	return task{Identifier: raw.Identifier, Title: raw.Title, BranchName: raw.BranchName}, nil
}

// rigID returns the lowercased issue identifier used as the rig's id.
func (t task) rigID() string {
	return strings.ToLower(t.Identifier)
}

// basedirName strips any "<user>/" prefix off the Linear branch name so the
// resulting slug is short and matches the issue: "phinze/mir-75-add-zig-stack"
// → "mir-75-add-zig-stack".
func (t task) basedirName() string {
	if i := strings.Index(t.BranchName, "/"); i >= 0 {
		return t.BranchName[i+1:]
	}
	return t.BranchName
}
