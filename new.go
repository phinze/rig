package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// runNew starts work that does not have a tracker identity yet. The kickoff is
// both the task's human title and the seed for its local identity; everything
// after that joins the ordinary authoring-rig path, except there is no branch
// hint to record. Like a repo added later with `rig add`, the workspace starts
// at trunk() and branch discovery takes over once the work grows a bookmark.
func runNew(args []string) error {
	agent, args, err := extractAgentFlag(args)
	if err != nil {
		return err
	}
	repoFlag, args := extractRepoFlag(args)

	kickoff, err := resolveKickoff(args)
	if err != nil {
		return err
	}
	if kickoff == "" {
		return nil // empty interactive prompt means never mind
	}

	rigID := kickoffID(kickoff)
	if rigID == "" {
		return fmt.Errorf("kickoff does not produce a usable local name")
	}
	basedir, err := basedirPath(rigID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(basedir); err == nil {
		if _, manifestErr := os.Stat(filepath.Join(basedir, manifestName)); manifestErr == nil {
			return fmt.Errorf("rig %q already exists; run `rig switch %s` (or `rig wake %s` if parked), or use a different kickoff", rigID, rigID, rigID)
		}
		return fmt.Errorf("basedir already exists: %s", basedir)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking basedir: %w", err)
	}

	repo, err := resolveRepo(repoFlag)
	if err != nil {
		return err
	}
	if repo.Path == "" {
		return nil // repo picker cancelled
	}
	if err := ensureJJColocated(repo.Path); err != nil {
		return fmt.Errorf("colocating jj on %s: %w", repo.Path, err)
	}

	m := manifest{ID: rigID, Title: kickoff, Agent: string(agent)}
	if err := createBasedir(basedir, m); err != nil {
		return err
	}

	repoDest, err := addRepoWorkspace(basedir, rigID, repo, "trunk()", "")
	if err != nil {
		return err
	}

	sess := sessionSpec{
		rectoCmd: "recto",
		agent:    agent,
		prompt: fmt.Sprintf(
			"Starting unticketed work with this kickoff: %s. There is no issue to read yet. Help me investigate and decide what, if anything, should be ticketed.",
			kickoff,
		),
	}
	session, err := spawnSession(basedir, repoDest, sess)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "rig: new %s — %s\n", rigID, basedir)
	return attachOrReport(session)
}

// resolveKickoff accepts an inline kickoff (`rig new investigate the flake`)
// or asks for one when run interactively with no positional arguments. The
// inline form keeps scripts and agent shells usable without pretending a pipe
// can answer a terminal prompt.
func resolveKickoff(args []string) (string, error) {
	if len(args) > 0 {
		return strings.TrimSpace(strings.Join(args, " ")), nil
	}
	if !stdinIsTTY() {
		return "", fmt.Errorf("Kickoff prompt needs an interactive terminal; pass it inline, e.g. `rig new investigate the flake`")
	}

	fmt.Fprint(os.Stderr, "Kickoff: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading kickoff: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// kickoffID turns the free-form kickoff into the stable identity used by the
// manifest, basedir, jj workspace, and tmux session. Keep the same 60-character
// ceiling as tracker-derived task slugs so local work does not grow a noisier
// filesystem shape than ticketed work.
func kickoffID(kickoff string) string {
	const maxLen = 60
	id := slugify(kickoff)
	if len(id) > maxLen {
		id = strings.TrimRight(id[:maxLen], "-")
	}
	return id
}
