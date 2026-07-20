package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

func runDown(args []string) error {
	force := false
	for _, a := range args {
		switch a {
		case "--force", "-f":
			force = true
		default:
			return fmt.Errorf("usage: rig down [--force]")
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

	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	lock, err := acquireRigLock(basedir, false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	// The manifest may have changed while we waited for another command.
	m, err = readManifest(basedir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}

	// Safety gate, unless forced. The shared judgment covers local work and asks
	// eagerly about every recorded PR, including a disjoint secondary whose
	// commits are no longer reachable from the working copy.
	if !force {
		if reason := rigTeardownBlocker(basedir, map[string]bool{}); reason != "" {
			return downRefusal(reason)
		}
	}

	// Note if the caller will be stranded by their cwd vanishing. This
	// matters when we're NOT killing the session (running from outside
	// tmux or from a different session) — otherwise the session dies and
	// the question is moot.
	cwdInside := false
	if rel, err := filepath.Rel(basedir, cwd); err == nil && !strings.HasPrefix(rel, "..") {
		cwdInside = true
	}

	// Move our own cwd out so RemoveAll can walk basedir cleanly.
	if err := os.Chdir(os.Getenv("HOME")); err != nil {
		return err
	}

	session := tmuxSessionName(basedir)
	inDoomed := insideTmuxSession(session)

	// Interactive rescue: when down runs from inside the very session it's about
	// to kill, pop the radar so you can pick where to land next instead of the
	// kill dropping you to the outer shell. The teardown above already happened —
	// the pick only chooses a destination, and escaping falls through to the
	// plain detach. We only do this when we're actually in the doomed session:
	// run from elsewhere, the kill never touches your client, so a TUI would just
	// hijack a terminal that was going to stay put anyway.
	if inDoomed && stdinIsTTY() {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dest, err := radarPick(home)
		if err != nil {
			return err
		}
		if dest != nil {
			// switch-client onto the destination before the kill, so by the time
			// this session dies our client is already looking elsewhere.
			if err := radarAct(*dest); err != nil {
				return err
			}
		}
	}

	job, err := prepareTeardownJob(basedir, m)
	if err != nil {
		return err
	}
	if inDoomed && runtime.GOOS == "linux" {
		// Tmux 3.6 launches panes in systemd scopes. The cleanup worker has to
		// run outside the current pane or stopping every RIG_ID scope would kill
		// the process halfway through its own teardown. The durable job is
		// already on disk, and rig reap retries it if systemd or the host dies.
		if err := startTeardownWorker(job); err != nil {
			return fmt.Errorf("starting teardown worker: %w (job retained at %s)", err, job.path)
		}
		fmt.Fprintf(os.Stderr, "rig: teardown scheduled for %s — %s\n", m.ID, basedir)
		return nil
	}
	if err := executeTeardownJob(job); err != nil {
		return fmt.Errorf("teardown incomplete (will retry from %s): %w", job.path, err)
	}

	fmt.Fprintf(os.Stderr, "rig: down %s — %s gone\n", m.ID, basedir)

	if cwdInside && !inDoomed {
		fmt.Fprintf(os.Stderr, "rig: note: your shell's cwd was inside the basedir; run `cd` to recover.\n")
	}
	return nil
}

// downRefusal wraps a blocker reason as the error `rig down` exits with, always
// pointing at the escape hatch so the gate never feels like a dead end.
func downRefusal(reason string) error {
	return fmt.Errorf("refusing to tear down: %s\n      run `rig down --force` to override", reason)
}

// unmergedPRsBlocker reports the first recorded PR that isn't merged, or "" when
// every recorded branch's PR is merged (or has no PR at all). Unlike the lazy
// workspace WIP check it always asks GitHub, which is what lets `rig down`
// refuse on an OPEN PR even when the workspace holds no local commits — the
// disjoint secondary-PR case reap's lazy path skips. A branch with no PR doesn't
// block (nothing to merge); a gh error blocks, fail-closed. Review rigs are
// exempt — merge state isn't their terminal condition (see below).
func unmergedPRsBlocker(basedir string) string {
	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Sprintf("reading manifest: %v", err)
	}
	// Review rigs are exempt: an OPEN PR is the whole point of reviewing it, not
	// a reason to keep the basedir. Their terminal condition ("you've posted a
	// review") is enforced by the shared rigTeardownBlocker, so this eager
	// all-PRs-merged gate would only ever wrongly refuse them.
	if m.isReview() {
		return ""
	}
	subdirs := make([]string, 0, len(m.Repos))
	for sub := range m.Repos {
		subdirs = append(subdirs, sub)
	}
	sort.Strings(subdirs)
	for _, sub := range subdirs {
		branches, err := repoBranches(m, sub, filepath.Join(basedir, sub))
		if err != nil {
			return fmt.Sprintf("%s: resolving branches: %v", sub, err)
		}
		for _, b := range branches {
			pr, err := prForBranch(m.Repos[sub], b)
			if err != nil {
				return fmt.Sprintf("%s: checking PR for %s: %v", sub, b, err)
			}
			if pr != nil && pr.State != "MERGED" {
				return fmt.Sprintf("%s PR #%d (%s) is %s, not merged",
					sub, pr.Number, b, strings.ToLower(pr.State))
			}
		}
	}
	return ""
}

// teardownRig is the synchronous teardown entry point used by reap and focused
// tests. Interactive down prepares the same job but hands it to a systemd
// worker when invoked from inside the session it needs to kill.
func teardownRig(basedir string, m manifest) error {
	job, err := prepareTeardownJob(basedir, m)
	if err != nil {
		return err
	}
	return executeTeardownJob(job)
}

// quarantineBasedir atomically moves basedir into a hidden sibling directory.
// Keeping the trash directory beside the rigs guarantees the rename stays on
// one filesystem, where directory rename is atomic and independent of the
// ownership of anything below basedir.
func quarantineBasedir(basedir string) (string, error) {
	trashRoot := filepath.Join(filepath.Dir(basedir), ".rig-trash")
	if err := os.Mkdir(trashRoot, 0o700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("creating %s: %w", trashRoot, err)
	}

	dest := filepath.Join(trashRoot, fmt.Sprintf("%s-%d-%d",
		filepath.Base(basedir), time.Now().UnixNano(), os.Getpid()))
	if err := os.Rename(basedir, dest); err != nil {
		return "", fmt.Errorf("moving %s to %s: %w", basedir, dest, err)
	}
	return dest, nil
}

// isoStop stops one iso session by exact name, run from the workspace dir
// so iso resolves the right project scope.
func isoStop(workspaceDir, session string) error {
	cmd := exec.Command("iso", "stop", "--session", session)
	cmd.Dir = workspaceDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// resolvePath returns dir with symlinks resolved, falling back to a cleaned
// absolute path when the target can't be resolved (e.g. it no longer exists).
func resolvePath(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return filepath.Clean(dir)
}

// isUnder reports whether child is parent or nested inside it.
func isUnder(child, parent string) bool {
	return child == parent || strings.HasPrefix(child, parent+string(os.PathSeparator))
}
