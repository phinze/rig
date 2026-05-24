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
