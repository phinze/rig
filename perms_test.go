package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// modCacheTree builds the shape that has stranded every teardown job so far:
// Go stamps its module cache read-only, so the files are ours and the
// directories holding them deny us the write access an unlink needs.
func modCacheTree(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "go", "pkg", "mod", "example.com", "mod@v1.2.3")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session_manager.go"), []byte("package mod\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	// Innermost first: sealing a parent before its child leaves the child
	// unreachable to chmod.
	for _, seal := range []string{dir, filepath.Dir(dir)} {
		if err := os.Chmod(seal, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = repairRemovablePerms(root)
		_ = os.RemoveAll(root)
	})
}

// The fixture is only worth anything if it actually reproduces the failure, so
// assert that plain RemoveAll still trips on it. If Go ever stops minting
// read-only module caches this is the test that should start failing.
func TestRemoveAllStillFailsOnReadOnlyDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rig-trash")
	modCacheTree(t, root)

	err := os.RemoveAll(root)
	if err == nil {
		t.Fatal("expected os.RemoveAll to fail on 0555 directories")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("expected a permission error, got %v", err)
	}
}

func TestRemoveAllForceClearsReadOnlyDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rig-trash")
	modCacheTree(t, root)

	if err := removeAllForce(root); err != nil {
		t.Fatalf("removeAllForce: %v", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be gone, got %v", root, err)
	}
}

// Nothing in the tree is foreign-uid here, so the repair must resolve it
// outright rather than reporting residue for an elevated pass to chase.
func TestRemoveAllForceReportsNoResidueForOwnedTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rig-trash")
	modCacheTree(t, root)

	blocked, err := repairRemovablePerms(root)
	if err != nil {
		t.Fatalf("repairRemovablePerms: %v", err)
	}
	if len(blocked) != 0 {
		t.Fatalf("a tree this user owns must need no elevation, got %v", blocked)
	}
}

// Removing a rig's trash must never reach outside it. A workspace routinely
// holds symlinks to the nix store and other shared trees, and chmod follows
// symlinks even though the walk does not, so the guard is that we only ever
// chmod a path Lstat already called a directory.
func TestRemoveAllForceDoesNotFollowSymlinksOutOfTree(t *testing.T) {
	tmp := t.TempDir()
	outside := filepath.Join(tmp, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(outside, "store"), 0o555); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(tmp, "rig-trash")
	modCacheTree(t, root)
	if err := os.Symlink(filepath.Join(outside, "store"), filepath.Join(root, "result")); err != nil {
		t.Fatal(err)
	}

	if err := removeAllForce(root); err != nil {
		t.Fatalf("removeAllForce: %v", err)
	}
	info, err := os.Stat(filepath.Join(outside, "store"))
	if err != nil {
		t.Fatalf("the symlink target must survive: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o555 {
		t.Fatalf("the symlink target's mode must be untouched, got %04o", got)
	}
}

// A failure that isn't about permissions must come back as itself, so callers
// can't mistake it for residue that elevation would clear.
func TestRemoveAllForcePassesThroughNonPermissionErrors(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := removeAllForce(filepath.Join(file, "child"))
	if err != nil && errors.As(err, new(*permissionResidue)) {
		t.Fatalf("ENOTDIR must not be reported as permission residue: %v", err)
	}
}

func TestPermissionResidueNamesItsBlockers(t *testing.T) {
	residue := &permissionResidue{
		Path:    "/home/phinze/workspaces/.rig-trash/mir-1324",
		Blocked: []string{"/home/phinze/workspaces/.rig-trash/mir-1324/runtime/bin"},
		Err:     fs.ErrPermission,
	}
	msg := residue.Error()
	for _, want := range []string{"runtime/bin", "1 directory"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q should mention %q", msg, want)
		}
	}
	if !errors.Is(residue, fs.ErrPermission) {
		t.Error("residue must unwrap to the underlying permission error")
	}
}

// foreignOwnedDir hands dir to root so a test can exercise residue that chmod
// genuinely cannot repair.
//
// It has to actually change the owner. The tests this replaces stood root
// ownership in with a 0555 mode and described the two as equivalent, and they
// are not: an owner can always chmod their way back in, which is why every
// teardown job stranded so far was repairable all along. There is no way to
// mint a second uid unprivileged, so this skips unless it is explicitly
// allowed to use sudo, and the suite stays green without it.
func foreignOwnedDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can unlink anything")
	}
	if os.Getenv("RIG_TEST_PRIVILEGED") == "" {
		t.Skip("set RIG_TEST_PRIVILEGED=1 (needs passwordless sudo) to exercise foreign-uid residue")
	}
	if sudoPath == "" || chownPath == "" {
		t.Skip("sudo or chown not found")
	}
	// -v validates credentials without running anything, which is the only
	// probe that survives the cleared PATH above: sudo would not find a bare
	// `true` to run.
	if err := exec.Command(sudoPath, "-n", "-v").Run(); err != nil {
		t.Skip("passwordless sudo unavailable")
	}
	if out, err := exec.Command(sudoPath, "-n", "--", chownPath, "-h", "root:root", "--", dir).CombinedOutput(); err != nil {
		t.Fatalf("giving %s to root: %v: %s", dir, err, out)
	}
	t.Cleanup(func() { giveBackBestEffort(dir) })
}

// sudoPath and chownPath are resolved at init rather than at call time because
// tests that need a foreign-uid fixture also tend to clobber PATH to stub out
// iso and docker, and a binary that vanished that way would silently skip the
// assertion instead of running it.
var (
	sudoPath, _  = exec.LookPath("sudo")
	chownPath, _ = exec.LookPath("chown")
)

// The whole point of the residue type is that it fires for foreign ownership
// and not for a mode we could have fixed ourselves.
func TestRemoveAllForceReportsForeignOwnedResidue(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rig-trash")
	bin := filepath.Join(root, "runtime", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "miren"), []byte("elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	foreignOwnedDir(t, bin)

	err := removeAllForce(root)
	var residue *permissionResidue
	if !errors.As(err, &residue) {
		t.Fatalf("removeAllForce(%s) = %v, want permission residue", root, err)
	}
	if len(residue.Blocked) != 1 || residue.Blocked[0] != bin {
		t.Fatalf("residue should name exactly the root-owned directory, got %v", residue.Blocked)
	}
}

// giveBack returns a directory foreignOwnedDir handed to root, so a test can
// prove the retry succeeds once the residue is resolved. It stands in for what
// an elevated pass will do.
func giveBack(t *testing.T, dir string) {
	t.Helper()
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if out, err := exec.Command(sudoPath, "-n", "--", chownPath, "-Rh", owner, "--", dir).CombinedOutput(); err != nil {
		t.Fatalf("reclaiming %s: %v: %s", dir, err, out)
	}
}

// giveBackBestEffort is the cleanup half, which must not fail a passing test
// but must still run, or t.TempDir's own removal inherits the problem.
func giveBackBestEffort(dir string) {
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	_ = exec.Command(sudoPath, "-n", "--", chownPath, "-Rh", owner, "--", dir).Run()
}
