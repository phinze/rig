package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// runAdd brings another repo into the rig you're currently in (cwd-derived).
// It clones the repo if needed, colocates jj, drops a workspace at trunk(), and
// opens a tmux window for it in the rig's session.
func runAdd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: rig add <owner/repo>")
	}
	owner, repo, ok := strings.Cut(args[0], "/")
	if !ok || owner == "" || repo == "" {
		return fmt.Errorf("expected owner/repo, got %q", args[0])
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

	repoPath, err := ensureGhqClone(owner, repo)
	if err != nil {
		return err
	}
	if err := ensureJJColocated(repoPath); err != nil {
		return fmt.Errorf("colocating jj on %s: %w", repoPath, err)
	}

	ref := repoRef{Owner: owner, Name: repo, Path: repoPath}
	// No branch hint for an added repo — start it on trunk().
	repoDest, err := addRepoWorkspace(basedir, m.ID, ref, "trunk()")
	if err != nil {
		return err
	}

	// Best-effort: give the new repo its own window in the rig session.
	session := tmuxSessionName(m.ID)
	if tmuxHasSession(session) {
		_ = tmuxNewWindow(session, repo, repoDest)
	}

	fmt.Fprintf(os.Stderr, "rig: added %s → %s\n", ref.nameWithOwner(), repoDest)
	return nil
}

// runLs lists the rigs currently in flight under ~/workspaces.
func runLs(args []string) error {
	rigs, err := listRigs()
	if err != nil {
		return err
	}
	if len(rigs) == 0 {
		fmt.Fprintln(os.Stderr, "rig: no rigs in flight")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, r := range rigs {
		fmt.Fprintf(w, "%s\t%s\n", r.ID, r.Title)
	}
	return w.Flush()
}

// runCd jumps to a rig by attaching (or switching to) its tmux session. With
// no arg it fzf-picks; with an arg it filters by id/slug/title substring and
// only falls back to the picker when the filter is ambiguous.
func runCd(args []string) error {
	rigs, err := listRigs()
	if err != nil {
		return err
	}
	if len(rigs) == 0 {
		return fmt.Errorf("no rigs in flight")
	}

	var chosen *rigInfo
	if len(args) >= 1 {
		q := strings.ToLower(strings.Join(args, " "))
		var matches []rigInfo
		for _, r := range rigs {
			hay := strings.ToLower(r.ID + " " + r.Slug + " " + r.Title)
			if strings.Contains(hay, q) {
				matches = append(matches, r)
			}
		}
		switch len(matches) {
		case 0:
			return fmt.Errorf("no rig matches %q", q)
		case 1:
			chosen = &matches[0]
		default:
			rigs = matches // narrow the picker to the matches
		}
	}

	if chosen == nil {
		rows := make([]string, len(rigs))
		for i, r := range rigs {
			rows[i] = fmt.Sprintf("%s\t%s\t%s", r.ID, r.Title, r.Slug)
		}
		sel, err := fzfSelect(rows, "cd to rig: ")
		if err != nil {
			return err
		}
		if sel == "" {
			return nil
		}
		id, _, _ := strings.Cut(sel, "\t")
		for i := range rigs {
			if rigs[i].ID == id {
				chosen = &rigs[i]
				break
			}
		}
	}
	if chosen == nil {
		return nil
	}

	session := tmuxSessionName(chosen.ID)
	if !tmuxHasSession(session) {
		// Rig dir is present but its session was killed; stand up a bare one.
		if err := tmuxNewSession(session, chosen.Path); err != nil {
			return fmt.Errorf("tmux new-session: %w", err)
		}
	}
	return attachOrReport(session)
}

type rigInfo struct {
	ID    string
	Slug  string // basedir directory name
	Title string
	Path  string // absolute basedir path
}

// listRigs scans ~/workspaces for directories carrying a rig manifest.
func listRigs() ([]rigInfo, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, "workspaces")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var rigs []rigInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		base := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(base, manifestName)); err != nil {
			continue
		}
		m, err := readManifest(base)
		if err != nil {
			continue
		}
		rigs = append(rigs, rigInfo{ID: m.ID, Slug: e.Name(), Title: m.Title, Path: base})
	}
	return rigs, nil
}
