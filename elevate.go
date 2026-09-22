package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// maxElevationRounds bounds the reclaim/retry cycle. One round is almost
// always enough, because the repair walk descends through a readable
// foreign-owned directory and so reports the whole subtree at once. A second
// round is needed only when a directory denied us even a listing, which hides
// whatever is below it until we own it; anything past a handful of those is a
// tree that is fighting back, and looping on it is worse than reporting it.
const maxElevationRounds = 8

// chownBatch caps how many directories ride on one sudo invocation. Nothing
// realistic comes close to ARG_MAX, but an unbounded argv built from a
// filesystem walk is the kind of thing that works until the one time it
// doesn't.
const chownBatch = 256

// removeAllForJob removes path on the authority of a teardown job.
//
// That authority is the whole reason elevation is allowed here. A teardown job
// is a durable record of a human deciding this rig should stop existing —
// written by `down` or `sweep` before anything is destroyed — and reap only
// ever replays one. So taking ownership of root-owned residue is not a
// judgment reap makes about a rig; it is the judgment already made, carried to
// the files it was always about. Paths with no job behind them (the orphan
// scratch that cleanupOrphanedRigRuntime finds from a scope's RIG_BASEDIR) get
// the unprivileged repair and nothing else, because there is no such record to
// carry.
//
// What gets elevated is chown, never rm. Keeping the privileged step
// non-destructive means a mistake in the scoping below changes an owner rather
// than destroying a tree, the actual deletion stays unprivileged and under
// every guard that already governs it, and a run that dies halfway leaves a
// tree we fully own, so the next retry needs no sudo at all.
func removeAllForJob(job *teardownJob, path string) error {
	err := removeAllForce(path)
	var previous []string
	for range maxElevationRounds {
		var residue *permissionResidue
		if !errors.As(err, &residue) {
			return err
		}
		// A round that reclaimed everything it was given and changed nothing
		// is not going to do better on the next pass.
		if slices.Equal(previous, residue.Blocked) {
			return err
		}
		previous = residue.Blocked
		if reclaimErr := reclaimForJob(job, residue.Blocked); reclaimErr != nil {
			return errors.Join(err, reclaimErr)
		}
		err = removeAllForce(path)
	}
	return err
}

// reclaimForJob takes ownership of residue this job is entitled to reclaim,
// refusing outright if any of it falls outside what the job named. Fail-closed
// is the point: a directory we cannot justify is a bug in the walk or a
// surprise on disk, and neither is something to run sudo against.
func reclaimForJob(job *teardownJob, blocked []string) error {
	roots := job.elevationRoots()
	if len(roots) == 0 {
		return fmt.Errorf("teardown job %s names nothing that may be reclaimed", job.ID)
	}
	for _, dir := range blocked {
		if err := authorizeElevation(roots, dir); err != nil {
			return err
		}
	}
	return reclaimDirs(blocked)
}

// elevationRoots is the exhaustive list of trees a job may reclaim inside: the
// quarantined copy of its basedir, and the agent scratch it recorded. Notably
// not Basedir itself — by the time anything is being removed it has already
// been renamed into the trash, so a live path under the original name belongs
// to somebody else now.
func (job *teardownJob) elevationRoots() []string {
	var roots []string
	if job.Quarantined != "" {
		roots = append(roots, resolvePath(job.Quarantined))
	}
	for _, dir := range job.ScratchDirs {
		if dir != "" {
			roots = append(roots, resolvePath(dir))
		}
	}
	return roots
}

// authorizeElevation is the gate every chown target passes. It re-asks the
// filesystem rather than trusting the walk that produced the path, because the
// walk's job was to find work and this one's is to refuse it.
func authorizeElevation(roots []string, dir string) error {
	if dir == "" || !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("refusing to reclaim non-absolute path %q", dir)
	}
	// Lstat, so a symlink can never be mistaken for the directory it points
	// at and chowned as if it were inside the tree.
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("refusing to reclaim %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing to reclaim %s: not a directory", dir)
	}
	// Scope before ownership, deliberately. Both refuse, but "outside what
	// this teardown owns" is the answer that matters about a path we should
	// never have been handed, and diagnosing a sibling rig by its ownership
	// would describe the accident rather than the trespass.
	resolved := resolvePath(dir)
	if !slices.ContainsFunc(roots, func(root string) bool { return pathInside(root, resolved) }) {
		return fmt.Errorf("refusing to reclaim %s: outside everything this teardown owns", dir)
	}
	if ownerUID(info) == os.Getuid() {
		return fmt.Errorf("refusing to reclaim %s: already owned by this user", dir)
	}
	return nil
}

// reclaimDirs hands the directories to chown under sudo. Only the directories,
// never -R: write access to a directory is all an unlink needs, so the files
// inside it can stay exactly as their owner left them, and the blast radius
// stays the list that authorizeElevation just approved one by one.
func reclaimDirs(dirs []string) error {
	if len(dirs) == 0 {
		return nil
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return fmt.Errorf("sudo is unavailable, so these directories must be reclaimed by hand:\n        %s",
			strings.Join(dirs, "\n        "))
	}
	chown, err := exec.LookPath("chown")
	if err != nil {
		return fmt.Errorf("chown is unavailable, so these directories must be reclaimed by hand:\n        %s",
			strings.Join(dirs, "\n        "))
	}
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	for chunk := range slices.Chunk(dirs, chownBatch) {
		fmt.Fprintf(os.Stderr, "rig: reclaiming %d root-owned director%s with sudo chown\n",
			len(chunk), plural(len(chunk), "y", "ies"))
		args := append([]string{"-n", "--", chown, "-h", owner, "--"}, chunk...)
		out, err := exec.Command(sudo, args...).CombinedOutput()
		if err == nil {
			continue
		}
		// -n first, always: the hourly timer has no terminal and must fail
		// rather than block forever on a password prompt nobody will see. A
		// human running `rig down` does have one, so ask them.
		if !interactiveStdin() {
			return fmt.Errorf("reclaiming %d director%s needs elevation and sudo could not run unattended: %w: %s",
				len(chunk), plural(len(chunk), "y", "ies"), err, strings.TrimSpace(string(out)))
		}
		args = append([]string{"--", chown, "-h", owner, "--"}, chunk...)
		cmd := exec.Command(sudo, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("reclaiming %d director%s: %w",
				len(chunk), plural(len(chunk), "y", "ies"), err)
		}
	}
	return nil
}

// interactiveStdin reports whether somebody is sitting in front of this
// process, which is what decides between failing on a password prompt and
// asking for one.
func interactiveStdin() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
