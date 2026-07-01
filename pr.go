package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// runPR opens the PR for the rig you're in, the jj-land replacement for
// `gh pr view -w`. In a jj workspace there's no git HEAD for gh to read a
// branch off of, so gh's own current-branch detection comes up empty. rig
// knows the layout: it finds which repo you're standing in, asks jj for the
// branch backing the current work, and hands that to gh to crack open.
func runPR(args []string) error {
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

	subdir, err := repoSubdirForCwd(basedir, cwd, m)
	if err != nil {
		return err
	}
	repoDest := filepath.Join(basedir, subdir)

	branch, err := repoBranch(m, subdir, repoDest)
	if err != nil {
		return err
	}
	if branch == "" {
		return fmt.Errorf(
			"no PR branch for %s yet: the work here isn't on a bookmark.\n"+
				"Create and push one (e.g. jj bookmark create <name> -r @, jj git push), then retry.",
			m.Repos[subdir],
		)
	}

	// -R from the manifest is authoritative; don't lean on direnv's GH_REPO,
	// which is keyed to cwd and would be wrong when run from the basedir root.
	cmd := exec.Command("gh", "pr", "view", branch, "-R", m.Repos[subdir], "--web")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
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
