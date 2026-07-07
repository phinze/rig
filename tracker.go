package main

import (
	"encoding/json"
	"fmt"
	"os"
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
// identifier is used directly; anything else (including no args) opens a live
// fzf picker whose list is a fresh Linear search re-run on each keystroke,
// seeded with whatever query the args spelled out. Returns "" (no error) when
// the user cancels the picker.
func resolveIssueID(args []string) (string, error) {
	if len(args) == 1 && linearIDRe.MatchString(args[0]) {
		return args[0], nil
	}

	// fzf shells out to `rig __issues {q}` on every (debounced) keystroke, so the
	// candidate list is whatever Linear returns for the current query rather than
	// a fuzzy filter over one frozen fetch. Point it at our own binary so row
	// formatting stays in one place (runIssueRows).
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating rig binary: %w", err)
	}
	reloadCmd := shellQuote(exe) + " __issues {q}"

	sel, err := fzfLiveSelect(reloadCmd, "Pick issue: ", strings.Join(args, " "))
	if err != nil {
		return "", err
	}
	if sel == "" {
		return "", nil
	}
	id, _, _ := strings.Cut(sel, "\t")
	return strings.TrimSpace(id), nil
}

// runIssueRows backs the live issue picker (the hidden `rig __issues` command
// fzf shells out to). It prints tab-delimited Identifier\tState\tTitle rows for
// the given query — empty query lists the default assigned/open set, anything
// else feeds Linear search. Because fzf runs it on every keystroke, it stays
// quiet on failure: a lookup error yields no rows rather than a stderr splat in
// the middle of the picker UI.
func runIssueRows(args []string) error {
	query := strings.TrimSpace(strings.Join(args, " "))
	var listArgs []string
	if query == "" {
		listArgs = []string{"issues", "list", "--limit", "25"}
	} else {
		listArgs = []string{"issues", "search", query, "--limit", "25"}
	}
	cands, err := fetchIssues(listArgs...)
	if err != nil {
		return nil // stay quiet; the picker just shows no rows for this query
	}
	var b strings.Builder
	for _, c := range cands {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", c.Identifier, c.State, c.Title)
	}
	_, _ = os.Stdout.WriteString(b.String())
	return nil
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
	return stripBranchUserPrefix(t.BranchName)
}

// stripBranchUserPrefix drops a leading "<user>/" from a branch name, the shape
// Linear and most PR branches carry ("phinze/mir-75-add-zig-stack" →
// "mir-75-add-zig-stack"). Shared by the issue flow (basedirName) and the PR
// pickup, which needs the same slug to land a resumed rig at the issue's path.
func stripBranchUserPrefix(branch string) string {
	if _, after, ok := strings.Cut(branch, "/"); ok {
		return after
	}
	return branch
}

// branchIssueIDRe matches the Linear-issue token a branch slug leads with —
// "mir-75" out of "mir-75-add-zig-stack" — mirroring linearIDRe's shape,
// lowercased (branch names are). It must be followed by a dash or the end so a
// bare "mir-75" also matches but "mir-750" (a different issue) doesn't bleed in.
var branchIssueIDRe = regexp.MustCompile(`^([a-z][a-z0-9]*-[0-9]+)(?:-|$)`)

// leadingIssueID pulls the Linear issue id off the front of a branch slug, or
// "" when the branch didn't come from an issue. It's what lets a PR pickup
// recover the id its originating `rig up <issue>` used, so the rig rebuilds
// under the same id and basedir.
func leadingIssueID(slug string) string {
	m := branchIssueIDRe.FindStringSubmatch(slug)
	if m == nil {
		return ""
	}
	return m[1]
}
