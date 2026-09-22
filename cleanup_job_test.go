package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A teardown job identifies everything it destroys by the rig's identity — tmux
// session, iso session, jj workspace names, agent scratch — and a rebuilt rig
// answers to every one of those names. So a job that outlives its rig has to
// notice, or it dismantles the replacement. This is not theoretical: a job stuck
// on an undeletable trash dir since 14:34 forgot the jj workspace of a fresh rig
// created at 17:31, leaving it orphaned for five days.
func TestTeardownJobDetectsRebuiltRig(t *testing.T) {
	basedir := t.TempDir()
	original := time.Now().Add(-3 * time.Hour).Truncate(time.Second)

	if err := writeManifest(basedir, manifest{ID: "mir-822", Created: original}); err != nil {
		t.Fatal(err)
	}
	job := &teardownJob{Version: teardownJobVersion, ID: "mir-822", Basedir: basedir, RigCreated: original}

	if job.supersededByNewRig() {
		t.Error("the rig that created the job must not read as superseded")
	}

	// `rig up` again: same path, same id, new rig.
	if err := writeManifest(basedir, manifest{ID: "mir-822", Created: time.Now().Truncate(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if !job.supersededByNewRig() {
		t.Error("a rig rebuilt at the same path must read as superseded")
	}

	// The ordinary case: we quarantined the basedir, so there's no manifest at
	// all. That's our own handiwork, not someone else's rig.
	if err := os.Remove(filepath.Join(basedir, manifestName)); err != nil {
		t.Fatal(err)
	}
	if job.supersededByNewRig() {
		t.Error("a basedir we already tore down must not read as superseded")
	}

	// Jobs written before the field existed carry no stamp and cannot judge;
	// they have to fall back to the old behaviour rather than refuse to run.
	legacy := &teardownJob{Version: teardownJobVersion, ID: "mir-822", Basedir: basedir}
	if legacy.supersededByNewRig() {
		t.Error("a job with no recorded stamp must not claim supersession")
	}
}

// A superseded job still owns the trash it made, and nothing else. It must clear
// that and retire, rather than either lingering forever or reaching into the new
// rig.
func TestSupersededJobCleansOnlyItsOwnTrash(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	basedir := t.TempDir()
	trash := t.TempDir()
	if err := os.WriteFile(filepath.Join(trash, "leftover"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A live rig sits at the basedir, created after the job.
	if err := writeManifest(basedir, manifest{ID: "mir-822", Created: time.Now().Truncate(time.Second)}); err != nil {
		t.Fatal(err)
	}

	job := &teardownJob{
		Version: teardownJobVersion, ID: "mir-822", Basedir: basedir,
		RigCreated:  time.Now().Add(-3 * time.Hour).Truncate(time.Second),
		Quarantined: trash,
		// If the guard failed, this is the step that would decapitate the new rig.
		ForgetGroups: map[string][]string{"/some/repo": {"mir-822-runtime"}},
	}
	if err := writeTeardownJob(job); err != nil {
		t.Fatal(err)
	}

	if err := executeTeardownJobForPlatform(job, "linux"); err != nil {
		t.Fatalf("superseded job should complete quietly: %v", err)
	}
	if _, err := os.Stat(trash); !os.IsNotExist(err) {
		t.Error("the job's own quarantined copy should be gone")
	}
	if _, err := os.Stat(filepath.Join(basedir, manifestName)); err != nil {
		t.Error("the new rig's basedir must be left completely alone")
	}
	if _, err := os.Stat(job.path); !os.IsNotExist(err) {
		t.Error("the job should have retired rather than queue another retry")
	}
}

// Trash whose directories deny their own owner write access is the shape that
// stranded every teardown job rig has ever accumulated, and it needs no
// privilege at all: a rig that populated GOMODCACHE leaves thousands of 0555
// directories full of files we own. This used to be diagnosed as root-owned
// container output and handed to the operator with a note about sudo, which
// sent them after an elevation they never needed.
func TestQuarantineRemovalRepairsOwnReadOnlyDirs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can unlink anything")
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "artifact"), []byte("big binary"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if err := removeQuarantined(&teardownJob{Version: teardownJobVersion, ID: "mir-822", Quarantined: locked}); err != nil {
		t.Fatalf("a tree this user owns should need no elevation: %v", err)
	}
	if _, err := os.Stat(locked); !os.IsNotExist(err) {
		t.Errorf("quarantined trash still exists: %v", err)
	}
}

// Root-owned residue is what the iso dev environment leaves behind routinely,
// and by teardown time the decision to destroy it has already been made and
// written down. So it gets reclaimed and removed rather than handed back to
// the operator with homework.
func TestQuarantineRemovalReclaimsForeignOwners(t *testing.T) {
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "artifact"), []byte("big binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	foreignOwnedDir(t, locked)

	job := &teardownJob{Version: teardownJobVersion, ID: "mir-822", Quarantined: locked}
	if err := removeQuarantined(job); err != nil {
		t.Fatalf("root-owned trash inside the quarantine should be reclaimed: %v", err)
	}
	if _, err := os.Stat(locked); !os.IsNotExist(err) {
		t.Errorf("quarantined trash still exists: %v", err)
	}
}

// When elevation is not available at all the job has to survive for a later
// retry, and say which directories a human would have to deal with. This is
// the shape a machine without passwordless sudo sees on every reap.
func TestQuarantineRemovalKeepsJobWhenElevationUnavailable(t *testing.T) {
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "artifact"), []byte("big binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	foreignOwnedDir(t, locked)
	t.Setenv("PATH", t.TempDir()) // no sudo to be found

	err := removeQuarantined(&teardownJob{Version: teardownJobVersion, ID: "mir-822", Quarantined: locked})
	if err == nil {
		t.Fatal("unreclaimable trash should keep the job for retry")
	}
	for _, want := range []string{"reclaimed by hand", locked} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// Forgetting is the irreversible step, so it's recorded as it happens. A job
// that dies afterwards must not replay it — that replay is what aims a stale
// job at a live rig's workspace.
func TestForgetProgressIsRecordedPerSource(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	basedir := t.TempDir()
	job := &teardownJob{
		Version: teardownJobVersion, ID: "mir-1", Basedir: basedir,
		Session: "no-such-session",
		// Neither workspace is registered anywhere, so the forget is a no-op —
		// but the bookkeeping still has to happen.
		ForgetGroups: map[string][]string{
			"/repo/a": {"mir-1-a"},
			"/repo/b": {"mir-1-b"},
		},
	}
	if err := writeTeardownJob(job); err != nil {
		t.Fatal(err)
	}
	if err := executeTeardownJobForPlatform(job, "darwin"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if len(job.ForgetGroups) != 0 {
		t.Errorf("ForgetGroups = %v, want every source consumed", job.ForgetGroups)
	}
}

// sortedKeys keeps a resumed job working through its sources in the same order
// it started, so partial progress stays comprehensible.
func TestSortedKeysIsStable(t *testing.T) {
	got := sortedKeys(map[string][]string{"c": nil, "a": nil, "b": nil})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("sortedKeys = %v, want a,b,c", got)
	}
}
