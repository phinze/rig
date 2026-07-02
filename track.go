package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// runTrack records an additional PR branch for the repo you're standing in, so
// down/reap gate on it alongside the rig's primary. It's the accounting hook
// for the "found a bug while in here and spun off a second PR on the same repo"
// case: rig keys everything off recorded branches, and a jj workspace can't tell
// which of a repo's (shared) bookmarks belong to this rig, so the second PR has
// to be declared rather than discovered. With no argument it records the branch
// backing the current work; pass one explicitly to track a branch you're not
// sitting on.
func runTrack(args []string) error {
	var branchArg string
	switch len(args) {
	case 0:
	case 1:
		branchArg = args[0]
	default:
		return fmt.Errorf("usage: rig track [branch]")
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

	branch := branchArg
	if branch == "" {
		branch, err = jjPRBranch(repoDest)
		if err != nil {
			return err
		}
	}
	if branch == "" {
		return fmt.Errorf(
			"no branch to track: the work here isn't on a bookmark.\n" +
				"Name one (rig track <branch>) or create and push a bookmark first.",
		)
	}

	added, err := addBranchToManifest(basedir, subdir, branch)
	if err != nil {
		return err
	}
	if !added {
		fmt.Fprintf(os.Stderr, "rig: %s already tracked for %s\n", branch, m.Repos[subdir])
		return nil
	}
	fmt.Fprintf(os.Stderr, "rig: tracking %s for %s\n", branch, m.Repos[subdir])
	return nil
}
