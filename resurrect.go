package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// runResurrect implements `rig resurrect <id>`: rebuild a torn-down rig from
// its tombstone and drop back into the conversation it was carrying.
//
// This is the counterweight to teardown being irreversible. Rig's gates can
// only see commits and PR states, so they will occasionally clear a rig whose
// value was never in the VCS at all; rather than making the gates timid, the
// teardown path records enough to undo itself and this command spends it.
//
// What comes back is the shape, not the contents: workspaces are re-added at
// the recorded branches, so uncommitted scratch is still gone for good. That's
// the honest boundary, and it's why the message says so rather than implying a
// full restore.
func runResurrect(args []string) error {
	if len(args) != 1 || args[0] == "" {
		return fmt.Errorf("usage: rig resurrect <rig-id>")
	}
	session, err := prepareResurrect(args[0], false, os.Stderr)
	if err != nil {
		return err
	}
	return attachOrReport(session)
}

// prepareResurrect rebuilds a tombstoned rig without attaching to it. Radar
// runs this inside its live Bubble Tea model and performs the final tmux switch
// only after the TUI exits; the CLI passes stderr and attaches immediately.
func prepareResurrect(id string, nonblocking bool, report io.Writer) (string, error) {
	if report == nil {
		report = io.Discard
	}
	t, err := findTombstone(id, time.Now())
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("no tombstone for %q — nothing torn down in the last %s has that id (`rig history` lists what's left)",
			id, tombstoneRetentionLabel())
	}

	// A live rig at the same id wins outright. Resurrecting on top of one would
	// mean two rigs claiming one basedir, one tmux session, and one set of jj
	// workspace names, which is the same collision teardown jobs guard against.
	if rigs, err := listRigs(); err == nil {
		for _, r := range rigs {
			if r.ID == t.ID {
				fmt.Fprintf(report, "rig: %s is already up — switching instead\n", t.ID)
				if err := setRigParked(r.Path, false, nonblocking, nil); err != nil {
					return "", err
				}
				return tmuxSessionName(r.Path), nil
			}
		}
	}

	if dirExists(t.Basedir) {
		return "", fmt.Errorf("%s already exists; move it aside before resurrecting %s", t.Basedir, t.ID)
	}

	m := manifest{
		ID:       t.ID,
		Title:    t.Title,
		Kind:     t.Kind,
		Agent:    t.Agent,
		Created:  time.Now(),
		Repos:    t.Repos,
		Branches: t.Branches,
		PRs:      t.PRs,
	}
	if err := createBasedir(t.Basedir, m); err != nil {
		return "", err
	}

	// Rebuild workspaces in a stable order so the primary repo (the one the
	// session's panes open in) is the same one every time.
	subdirs := make([]string, 0, len(t.Sources))
	for sub := range t.Sources {
		subdirs = append(subdirs, sub)
	}
	sort.Strings(subdirs)

	primary := ""
	for _, sub := range subdirs {
		source := t.Sources[sub]
		if !dirExists(source) {
			fmt.Fprintf(report, "rig: skipping %s — source repo %s is gone\n", sub, source)
			continue
		}
		owner, _, _ := strings.Cut(t.Repos[sub], "/")
		repo := repoRef{Owner: owner, Name: sub, Path: source}

		// Prefer the branch this rig rode. It may have been merged and deleted
		// since, in which case resolveStartRev falls back to trunk() and the
		// workspace comes back on a clean base rather than failing.
		branch := ""
		if bs := t.Branches[sub]; len(bs) > 0 {
			branch = bs[0]
		}
		dest, err := addRepoWorkspace(t.Basedir, t.ID, repo, resolveStartRev(source, branch), branch)
		if err != nil {
			return "", fmt.Errorf("restoring %s: %w", sub, err)
		}
		if primary == "" {
			primary = dest
		}
	}
	if primary == "" {
		return "", fmt.Errorf("no repos could be restored for %s (sources missing)", t.ID)
	}
	m, err = readManifest(t.Basedir)
	if err != nil {
		return "", err
	}
	m.MainRepo = filepath.Base(primary)
	if err := writeManifest(t.Basedir, m); err != nil {
		return "", err
	}

	agent, err := parseAgent(t.Agent)
	if err != nil {
		agent = agentClaude
	}
	sess := sessionSpec{
		rectoCmd: rectoCommand(),
		repo:     filepath.Base(primary),
		agent:    agent,
	}
	if t.resurrectable() {
		sess.command = agent.resumeCommand(t.Session.ID)
	} else {
		// No conversation on record: this rig predates session capture, or the
		// agent never wrote one. Rebuilding the workspaces is still worth doing,
		// but say plainly that the context isn't coming with it.
		fmt.Fprintf(report, "rig: no session recorded for %s — rebuilding workspaces only\n", t.ID)
		sess.prompt = fmt.Sprintf("This rig (%s) was rebuilt from a tombstone after teardown; its previous agent session could not be recovered. Ask me for context before assuming any.", t.ID)
	}

	session, err := spawnSession(t.Basedir, primary, sess)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(report, "rig: resurrected %s — %s\n", t.ID, t.Basedir)
	if t.resurrectable() {
		fmt.Fprintf(report, "rig: resuming %s session %s (uncommitted work from before teardown is not recoverable)\n",
			t.Session.Agent, t.Session.ID)
	}
	return session, nil
}

// runHistory implements `rig history`: what's died lately and what can still be
// brought back. It's the CLI half of the radar's history section, and the thing
// that makes the regret window discoverable rather than a directory you'd have
// to know about.
func runHistory(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: rig history")
	}
	now := time.Now()
	stones, err := listTombstones(now)
	if err != nil {
		return err
	}
	if len(stones) == 0 {
		fmt.Fprintf(os.Stderr, "rig: no rigs torn down in the last %s\n", tombstoneRetentionLabel())
		return nil
	}
	for _, t := range stones {
		mark := "  "
		if t.resurrectable() {
			mark = "↺ "
		}
		fmt.Printf("%s%-28s %-12s %s\n", mark, t.ID, humanAge(now.Sub(t.Died))+" ago", t.subject())
	}
	fmt.Fprintf(os.Stderr, "\n↺ = session recoverable; `rig resurrect <id>` rebuilds it\n")
	return nil
}

// humanAge renders a duration the way a history row wants it: coarse, no
// decimals, and never longer than the retention window it's bounded by.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
