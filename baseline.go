package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A repo's baseline is its recorded start, moved on purpose. A rig resumed
// with new scope still diffs from where it first started, so the window
// shows everything that has landed since, its own earlier work and other
// rigs' alike, around the one change that matters now. Moving the start
// forward makes the window begin again from here, and it keeps showing what
// this rig lands after that, the way it does from a fresh start.

// setBaseline moves repo's start to the commit rev names in its workspace,
// or to where the workspace forks off trunk now when rev is empty. It returns
// the previous start, "" when none was recorded. The commit has to be on
// trunk: the window anchors there only when what lies between it and the
// branch point is landed work, so a commit off trunk would be ignored.
func setBaseline(basedir string, m *manifest, repo, rev string) (old, commit string, err error) {
	if m.Repos[repo] == "" {
		return "", "", fmt.Errorf("this rig has no repo %q", repo)
	}
	ws := filepath.Join(basedir, repo)
	if rev == "" {
		rev = startRevset
	}
	commit, err = jjCommitIDAt(ws, "", rev)
	if err != nil {
		return "", "", err
	}
	if _, err := jjCommitIDAt(ws, "", commit+" & ::trunk()"); err != nil {
		return "", "", fmt.Errorf("%s: %s isn't on trunk, so the diff can't start there", repo, shortOID(commit))
	}
	if m.Starts == nil {
		m.Starts = map[string]string{}
	}
	old = m.Starts[repo]
	m.Starts[repo] = commit
	return old, commit, nil
}

func shortOID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func runBaseline(args []string) error {
	var rev string
	var repos []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--at":
			if i+1 == len(args) {
				return fmt.Errorf("--at needs a revision")
			}
			i++
			rev = args[i]
		case strings.HasPrefix(a, "--at="):
			rev = strings.TrimPrefix(a, "--at=")
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("usage: rig baseline [REPO...] [--at REV]")
		default:
			repos = append(repos, a)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	basedir, err := findBasedir(cwd)
	if err != nil {
		return err
	}
	lock, err := acquireRigMutationLock(basedir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	if len(repos) == 0 {
		for repo := range m.Repos {
			repos = append(repos, repo)
		}
		slices.Sort(repos)
	}
	if len(repos) == 0 {
		return fmt.Errorf("this rig has no repos to diff")
	}
	var lines []string
	for _, repo := range repos {
		old, commit, err := setBaseline(basedir, &m, repo, rev)
		if err != nil {
			return err
		}
		switch {
		case old == commit:
			lines = append(lines, fmt.Sprintf("%s: already from %s", repo, shortOID(commit)))
		case old == "":
			lines = append(lines, fmt.Sprintf("%s: now from %s", repo, shortOID(commit)))
		default:
			// The full old id, so moving back is one paste away.
			lines = append(lines, fmt.Sprintf("%s: now from %s (was %s)", repo, shortOID(commit), old))
		}
	}
	if err := writeManifest(basedir, m); err != nil {
		return err
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}

// postBaseline is the window's "start here": it moves one repo's baseline
// to where it forks off trunk now.
func postBaseline(rw http.ResponseWriter, r *http.Request) {
	rig, err := rigBySlug(r.PathValue("rig"))
	if err != nil {
		jsonError(rw, http.StatusNotFound, err)
		return
	}
	// Without waiting: a handler parked behind a teardown would hang the
	// window's button with no word of why.
	lock, err := acquireRigMutationLockMode(rig.Path, true)
	if err != nil {
		jsonError(rw, http.StatusConflict, err)
		return
	}
	defer func() { _ = lock.Close() }()
	m, err := readManifest(rig.Path)
	if err != nil {
		jsonError(rw, http.StatusInternalServerError, err)
		return
	}
	old, commit, err := setBaseline(rig.Path, &m, r.PathValue("repo"), "")
	if err != nil {
		jsonError(rw, http.StatusBadRequest, err)
		return
	}
	if err := writeManifest(rig.Path, m); err != nil {
		jsonError(rw, http.StatusInternalServerError, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"start": commit, "was": old})
}
