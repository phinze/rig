package main

import (
	"fmt"
	"os"
)

func runUp(args []string) error {
	id, err := resolveIssueID(args)
	if err != nil {
		return err
	}
	if id == "" {
		return nil // picker cancelled
	}

	tk, err := resolveTask(id)
	if err != nil {
		return err
	}

	repo, err := detectPrimaryRepo()
	if err != nil {
		return err
	}

	basedir, err := basedirPath(tk.basedirName())
	if err != nil {
		return err
	}

	if err := ensureJJColocated(repo.Path); err != nil {
		return fmt.Errorf("colocating jj on %s: %w", repo.Path, err)
	}

	m := manifest{ID: tk.rigID(), Title: tk.Title}
	if err := createBasedir(basedir, m); err != nil {
		return err
	}

	startRev := resolveStartRev(repo.Path, tk.BranchName)
	repoDest, err := addRepoWorkspace(basedir, tk.rigID(), repo, startRev)
	if err != nil {
		return err
	}

	// Layout: recto on the right, claude on the left with an issue-pickup
	// prompt. Linear-specific phrasing for now; when a second tracker
	// arrives we'll dispatch on it.
	sess := sessionSpec{
		rectoCmd: "recto",
		prompt: fmt.Sprintf(
			"Picking up %s (%s). Use the Linear MCP (it may take a few seconds to connect) to read the issue, mark it In Progress and assigned to me, then help me plan.",
			tk.Identifier, tk.Title,
		),
	}
	session, err := spawnSession(basedir, repoDest, sess)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "rig: up %s — %s\n", tk.Identifier, basedir)
	return attachOrReport(session)
}
