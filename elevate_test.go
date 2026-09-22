package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// authorizeElevation is the only thing standing between a filesystem walk and
// sudo, so its refusals matter more than its approvals. Every case here is a
// path that must never reach chown.
func TestAuthorizeElevationRefusesWhatTheJobDoesNotOwn(t *testing.T) {
	root := t.TempDir()
	quarantine := filepath.Join(root, "trash", "mir-1-123-456")
	inside := filepath.Join(quarantine, "runtime", "bin")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "live-rig")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(quarantine, "notadir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(quarantine, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	roots := []string{resolvePath(quarantine)}

	for _, tc := range []struct {
		name, dir, want string
	}{
		{"a sibling rig", outside, "outside everything this teardown owns"},
		{"an absolute path elsewhere", "/etc", "outside everything this teardown owns"},
		{"a relative path", "runtime/bin", "non-absolute"},
		{"an empty path", "", "non-absolute"},
		{"a regular file", file, "not a directory"},
		{"a missing path", filepath.Join(quarantine, "gone"), "refusing to reclaim"},
		// The symlink resolves inside a directory we do not own, and Lstat
		// sees a link rather than a directory either way.
		{"a symlink out of the tree", link, "not a directory"},
		// Nothing to gain and a needless sudo: we can already chmod this.
		{"a directory we already own", inside, "already owned by this user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := authorizeElevation(roots, tc.dir)
			if err == nil {
				t.Fatalf("authorizeElevation(%q) allowed what it should refuse", tc.dir)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should explain %q", err, tc.want)
			}
		})
	}
}

func TestAuthorizeElevationAllowsForeignResidueInScope(t *testing.T) {
	root := t.TempDir()
	quarantine := filepath.Join(root, "trash", "mir-1-123-456")
	bin := filepath.Join(quarantine, "runtime", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	foreignOwnedDir(t, bin)

	if err := authorizeElevation([]string{resolvePath(quarantine)}, bin); err != nil {
		t.Fatalf("root-owned residue inside the quarantine must be reclaimable: %v", err)
	}
}

// The roots come from the job, so a job that recorded nothing must reclaim
// nothing rather than falling back to some ambient notion of what is safe.
func TestReclaimRefusesAJobThatNamesNothing(t *testing.T) {
	job := &teardownJob{Version: teardownJobVersion, ID: "mir-1", Basedir: "/home/phinze/workspaces/mir-1"}
	err := reclaimForJob(job, []string{"/home/phinze/workspaces/mir-1/runtime/bin"})
	if err == nil || !strings.Contains(err.Error(), "names nothing that may be reclaimed") {
		t.Fatalf("reclaimForJob = %v, want a refusal", err)
	}
}

// Basedir is deliberately not an elevation root. By the time anything is being
// removed it has been renamed into the trash, so a live directory answering to
// the old name belongs to whatever rig was built there since.
func TestElevationRootsExcludeTheOriginalBasedir(t *testing.T) {
	job := &teardownJob{
		Version:     teardownJobVersion,
		ID:          "mir-1",
		Basedir:     "/home/phinze/workspaces/mir-1",
		Quarantined: "/home/phinze/workspaces/.rig-trash/mir-1-123-456",
		ScratchDirs: []string{"/tmp/claude-1000/-home-phinze-workspaces-mir-1", ""},
	}
	roots := job.elevationRoots()
	if len(roots) != 2 {
		t.Fatalf("roots = %v, want the quarantine and the one non-empty scratch dir", roots)
	}
	for _, root := range roots {
		if root == job.Basedir {
			t.Errorf("the original basedir must never be an elevation root")
		}
	}
}

// A job with real residue must actually reclaim it and finish, rather than
// reporting the residue it was given the authority to clear.
func TestRemoveAllForJobReclaimsForeignResidue(t *testing.T) {
	root := t.TempDir()
	quarantine := filepath.Join(root, "trash", "mir-1-123-456")
	bin := filepath.Join(quarantine, "runtime", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "miren"), []byte("elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Before the 0555 directories below, so an unprivileged run skips without
	// leaving TempDir a tree its own cleanup cannot remove.
	foreignOwnedDir(t, bin)

	// Both causes at once, which is the realistic shape: a rig that ran
	// containers and also built Go code.
	mod := filepath.Join(quarantine, "cache", "example.com", "mod@v1.0.0")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "mod.go"), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mod, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = repairRemovablePerms(quarantine) })

	job := &teardownJob{Version: teardownJobVersion, ID: "mir-1", Quarantined: quarantine}
	if err := removeAllForJob(job, quarantine); err != nil {
		t.Fatalf("removeAllForJob: %v", err)
	}
	if _, err := os.Stat(quarantine); !os.IsNotExist(err) {
		t.Errorf("quarantine still exists: %v", err)
	}
}
