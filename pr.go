package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// runPR opens a PR belonging to the rig you're in. A rig may span repos
// through `rig add`, and each repo may carry several branches through `rig
// track`, so cwd is only used to find the rig rather than select one repo.
//
// When several PRs match, the user chooses one with the usual fzf picker. The
// manifest is the durable accounting record, but the current workspace
// bookmark is included too. That catches an added repo once work grows there,
// and a primary branch that evolved after the rig was created (for example a
// publish-preview branch spun from the issue's original branch).
func runPR(args []string) error {
	return runPRWithPicker(args, fzfSelect)
}

func runPRWithPicker(args []string, picker func([]string, string) (string, error)) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: rig pr")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	basedir, err := findBasedir(cwd)
	if err != nil {
		return err
	}
	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}

	candidates, err := rigPRCandidates(basedir, m)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return fmt.Errorf(
			"no PR branches found for this rig yet: its repos are all on trunk.\n" +
				"Create and push a bookmark (or use `rig track <branch>`), then retry.",
		)
	}

	var prs []rigPR
	seen := map[string]bool{}
	for _, candidate := range candidates {
		pr, err := prForBranch(candidate.Repo, candidate.Branch)
		if err != nil {
			return err
		}
		if pr == nil {
			continue
		}
		key := fmt.Sprintf("%s#%d", candidate.Repo, pr.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		prs = append(prs, rigPR{Repo: candidate.Repo, Branch: candidate.Branch, prInfo: *pr})
	}
	if len(prs) == 0 {
		checked := make([]string, len(candidates))
		for i, candidate := range candidates {
			checked[i] = candidate.Repo + ":" + candidate.Branch
		}
		return fmt.Errorf("no pull requests found for this rig (checked %s)", strings.Join(checked, ", "))
	}

	pr, err := selectRigPR(prs, picker)
	if err != nil {
		return err
	}
	if pr == nil {
		return nil // picker cancelled
	}

	// Select by number after resolving by branch. This avoids asking gh to
	// resolve the same branch a second time, and keeps the open pinned to the
	// repo from the manifest instead of cwd's GH_REPO.
	cmd := exec.Command("gh", "pr", "view", strconv.Itoa(pr.Number), "-R", pr.Repo, "--web")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("opening %s PR #%d: %w", pr.Repo, pr.Number, err)
	}
	return nil
}

// selectRigPR follows rig's usual cardinality rule: one answer proceeds
// directly, many answers go through fzf, and a cancelled picker does nothing.
// The fourth, hidden column is the stable index back into prs.
func selectRigPR(prs []rigPR, picker func([]string, string) (string, error)) (*rigPR, error) {
	if len(prs) == 1 {
		return &prs[0], nil
	}
	rows := make([]string, len(prs))
	for i, pr := range prs {
		title := pr.Title
		if title == "" {
			title = pr.Branch
		}
		rows[i] = fmt.Sprintf("%s\t#%d\t%s\t%d", pr.Repo, pr.Number, title, i)
	}
	sel, err := picker(rows, "Open PR: ")
	if err != nil {
		return nil, err
	}
	if sel == "" {
		return nil, nil
	}
	cols := strings.SplitN(sel, "\t", 4)
	if len(cols) != 4 {
		return nil, fmt.Errorf("unexpected PR picker selection: %q", sel)
	}
	i, err := strconv.Atoi(cols[3])
	if err != nil || i < 0 || i >= len(prs) {
		return nil, fmt.Errorf("unexpected PR picker selection: %q", sel)
	}
	return &prs[i], nil
}

type rigPRCandidate struct {
	Repo   string
	Branch string
}

// rigPRCandidates returns the branches that could have PRs belonging to this
// rig in stable repo order. Recorded branches preserve their manifest order;
// the workspace's current branch follows when it is not already recorded.
func rigPRCandidates(basedir string, m manifest) ([]rigPRCandidate, error) {
	subdirs := make([]string, 0, len(m.Repos))
	for subdir := range m.Repos {
		subdirs = append(subdirs, subdir)
	}
	sort.Strings(subdirs)

	var candidates []rigPRCandidate
	for _, subdir := range subdirs {
		branches := slices.Clone(m.Branches[subdir])
		current, err := jjPRBranch(filepath.Join(basedir, subdir))
		if err != nil {
			return nil, fmt.Errorf("%s: resolving current branch: %w", subdir, err)
		}
		if current != "" && !slices.Contains(branches, current) {
			branches = append(branches, current)
		}
		for _, branch := range branches {
			if branch != "" {
				candidates = append(candidates, rigPRCandidate{Repo: m.Repos[subdir], Branch: branch})
			}
		}
	}
	return candidates, nil
}

// repoSubdirForCwd works out which of the rig's repos the command is aimed at.
// Standing inside a repo's subdir (or below it) picks that repo. From the
// basedir root it's only unambiguous when the rig holds a single repo;
// otherwise the caller has to cd into the one they mean.
func repoSubdirForCwd(basedir, cwd string, m manifest) (string, error) {
	if rel, err := filepath.Rel(basedir, cwd); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		first := strings.SplitN(rel, string(filepath.Separator), 2)[0]
		if _, ok := m.Repos[first]; ok {
			return first, nil
		}
	}
	if len(m.Repos) == 1 {
		for subdir := range m.Repos {
			return subdir, nil
		}
	}
	names := make([]string, 0, len(m.Repos))
	for subdir := range m.Repos {
		names = append(names, subdir)
	}
	sort.Strings(names)
	return "", fmt.Errorf("which repo? cd into one of this rig's repos first (%s)", strings.Join(names, ", "))
}

// jjPRBranch returns the branch backing the workspace's PR: the closest
// non-trunk bookmark in the ancestry of @. trunk() is excluded so a rig whose
// work isn't on a branch yet resolves to "" (the bookmark we'd find would just
// be main) rather than pointing gh at the trunk. Empty is not an error — the
// caller turns it into a hint about pushing a branch first.
func jjPRBranch(workspaceDir string) (string, error) {
	cmd := exec.Command("jj", "log",
		"--no-graph", "--ignore-working-copy",
		"-r", "heads(::@ & bookmarks() ~ trunk())",
		"-T", `bookmarks.map(|b| b.name()).join("\n") ++ "\n"`,
	)
	cmd.Dir = workspaceDir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("jj log: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("jj log: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line, nil
		}
	}
	return "", nil
}
