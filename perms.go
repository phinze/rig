package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// dirOwnerWX are the bits a directory's owner needs before it can unlink the
// entries inside it. Unlinking is a write to the *parent*, never to the file,
// which is why a tree of files we own can still refuse to go away.
const dirOwnerWX = 0o300

// permissionResidue reports a removal that got as far as it can unprivileged:
// the directories listed in Blocked deny this user write access and are not
// ours to chmod. It carries them rather than just failing so an elevated pass
// knows exactly which paths to hand to chown, and so the error can say which
// of the two very different causes it hit.
type permissionResidue struct {
	Path    string
	Blocked []string
	Err     error
}

func (e *permissionResidue) Error() string {
	return fmt.Sprintf("%s: %v (blocked by %d director%s this user does not own: %s)",
		e.Path, e.Err, len(e.Blocked), plural(len(e.Blocked), "y", "ies"),
		strings.Join(e.Blocked, ", "))
}

func (e *permissionResidue) Unwrap() error { return e.Err }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// removeAllForce removes path, repairing the permission failure that actually
// strands rig teardown before giving up on it.
//
// Two different things wear the same "permission denied" face here and they
// need opposite treatment. The common one by far is a directory whose mode
// denies its own owner write access: Go stamps its module cache 0555 on
// purpose, so a rig that ever populated GOMODCACHE leaves thousands of
// directories full of files we own and cannot unlink. Nothing privileged is
// involved and nothing about retrying helps; the fix is to put the write bit
// back on directories that are already ours. The rare one is genuinely
// foreign-uid residue, usually root-owned container or iso output, where chmod
// is refused because we are not the owner. That needs elevation, which is not
// this function's business, so it is reported as a permissionResidue naming
// the exact directories to escalate against.
//
// The repair is a single walk rather than a chmod-parent-and-retry loop
// because RemoveAll only ever names the first path it tripped on: repairing
// one directory per retry turns a 1,570-directory module cache into 1,570
// full tree walks.
func removeAllForce(path string) error {
	err := os.RemoveAll(path)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	blocked, repairErr := repairRemovablePerms(path)
	if repairErr != nil {
		return errors.Join(err, repairErr)
	}
	retry := os.RemoveAll(path)
	if retry == nil {
		return nil
	}
	if len(blocked) > 0 && errors.Is(retry, fs.ErrPermission) {
		return &permissionResidue{Path: path, Blocked: blocked, Err: retry}
	}
	return retry
}

// repairRemovablePerms walks root restoring write access to every directory
// this user owns, and returns the directories it could not repair because
// somebody else owns them. Errors reading the tree are collected the same way
// rather than aborting: one root-owned subdirectory must not stop us from
// making the other 99% of the tree removable, since the unprivileged pass that
// follows will clear everything the elevated one then doesn't have to touch.
func repairRemovablePerms(root string) ([]string, error) {
	self := os.Getuid()
	var blocked []string
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			switch {
			case errors.Is(err, fs.ErrNotExist):
				return nil
			case errors.Is(err, fs.ErrPermission):
				// We could not even list it, so it is not ours to fix and
				// everything below it is unreachable from here.
				blocked = append(blocked, p)
				return fs.SkipDir
			default:
				return err
			}
		}
		if !d.IsDir() {
			return nil
		}
		// Ask the kernel rather than deriving it from the mode bits, so
		// supplementary group membership counts the same way it will when the
		// removal actually runs.
		if unix.Access(p, unix.W_OK|unix.X_OK) == nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if ownerUID(info) != self {
			// Recorded, but still descended into: the subtree below a
			// root-owned directory is frequently ours, and clearing it now
			// shrinks what elevation has to cover.
			blocked = append(blocked, p)
			return nil
		}
		// d.IsDir came from an Lstat, so this path is a real directory and
		// Chmod cannot be following a symlink to somewhere else.
		if err := os.Chmod(p, info.Mode().Perm()|dirOwnerWX); err != nil {
			if errors.Is(err, fs.ErrPermission) {
				blocked = append(blocked, p)
				return nil
			}
			return err
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	slices.Sort(blocked)
	return slices.Compact(blocked), nil
}

// ownerUID reports the uid owning a stat result, or -1 when the platform does
// not expose one. -1 never equals a real uid, so an unknown owner is treated
// as somebody else's and escalated rather than silently chmod'ed.
func ownerUID(info fs.FileInfo) int {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(st.Uid)
}
