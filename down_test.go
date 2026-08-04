package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// unmergedPRsBlocker is `rig down`'s eager gate: every recorded branch's PR must
// be merged (or absent). It's what refuses a teardown while a PR — primary or a
// tracked secondary — is still open, independent of local commit state.
func TestUnmergedPRsBlocker(t *testing.T) {
	fakeGh(t)
	dir := t.TempDir()
	if err := writeManifest(dir, manifest{
		ID:       "mir-1",
		Repos:    map[string]string{"rig": "o/r"},
		Branches: map[string][]string{"rig": {"feat"}},
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("merged clears the gate", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "MERGED")
		if reason := unmergedPRsBlocker(dir); reason != "" {
			t.Errorf("expected merged PR to clear, blocked by: %q", reason)
		}
	})

	t.Run("open PR blocks", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "OPEN")
		if reason := unmergedPRsBlocker(dir); reason == "" {
			t.Error("expected an open PR to block teardown")
		}
	})

	t.Run("no PR does not block", func(t *testing.T) {
		t.Setenv("GH_FAKE_NOPR", "1")
		if reason := unmergedPRsBlocker(dir); reason != "" {
			t.Errorf("expected a branch with no PR to pass, blocked by: %q", reason)
		}
	})

	t.Run("open secondary blocks even when primary merged", func(t *testing.T) {
		// The whole point of rig track: a second PR on the same repo counts.
		if _, err := addBranchToManifest(dir, "rig", "bugfix"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GH_FAKE_STATE", "OPEN")
		if reason := unmergedPRsBlocker(dir); reason == "" {
			t.Error("expected an open secondary PR to block teardown")
		}
	})

	t.Run("gh failure blocks, fail-closed", func(t *testing.T) {
		t.Setenv("GH_FAKE_ERR", "1")
		if reason := unmergedPRsBlocker(dir); reason == "" {
			t.Error("expected a gh error to block teardown")
		}
	})
}

func TestRigTeardownBlockerIncludesDisjointPRs(t *testing.T) {
	fakeGh(t)
	dir := t.TempDir()
	if err := writeManifest(dir, manifest{
		ID:       "mir-2",
		Repos:    map[string]string{"rig": "o/r"},
		Branches: map[string][]string{"rig": {"merged-primary", "open-secondary"}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_FAKE_STATE", "MERGED")
	t.Setenv("GH_FAKE_ALT_BRANCH", "open-secondary")
	t.Setenv("GH_FAKE_ALT_STATE", "OPEN")

	reason := rigTeardownBlocker(dir, map[string]bool{})
	if !strings.Contains(reason, "open-secondary") || !strings.Contains(reason, "not merged") {
		t.Fatalf("reason = %q, want the disjoint open secondary PR to block reap", reason)
	}
}

func TestIsUnder(t *testing.T) {
	tests := []struct {
		child, parent string
		want          bool
	}{
		{"/rigs/task/cloud", "/rigs/task", true},
		{"/rigs/task", "/rigs/task", true},
		{"/rigs/task-other/cloud", "/rigs/task", false}, // prefix but not nested
		{"/rigs/other", "/rigs/task", false},
		{"/rigs/task/a/b/c", "/rigs/task", true},
	}
	for _, tt := range tests {
		if got := isUnder(tt.child, tt.parent); got != tt.want {
			t.Errorf("isUnder(%q, %q) = %v, want %v", tt.child, tt.parent, got, tt.want)
		}
	}
}

func TestTeardownTmuxOrderingByPlatform(t *testing.T) {
	for _, tt := range []struct {
		platform string
		expect   string
	}{
		{platform: "linux", expect: "early"},
		{platform: "darwin", expect: "late"},
	} {
		t.Run(tt.platform, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			tmux := `#!/bin/sh
case "$1" in
  has-session) exit 0 ;;
  kill-session)
    case "$TMUX_EXPECT" in
      early) test -d "$TMUX_BASEDIR" && test -f "$TMUX_JOB" ;;
      late) test ! -e "$TMUX_BASEDIR" && test ! -e "$TMUX_JOB" ;;
    esac
    ;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(tmux), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))

			basedir := filepath.Join(root, "workspaces", "mir-order")
			if err := os.MkdirAll(basedir, 0o755); err != nil {
				t.Fatal(err)
			}
			job := &teardownJob{
				Version: teardownJobVersion,
				ID:      "mir-order",
				Basedir: basedir,
				Session: "rig-order",
				Created: time.Now(),
				path:    "",
			}
			if err := writeTeardownJob(job); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMUX_EXPECT", tt.expect)
			t.Setenv("TMUX_BASEDIR", basedir)
			t.Setenv("TMUX_JOB", job.path)

			if err := executeTeardownJobForPlatform(job, tt.platform); err != nil {
				t.Fatalf("teardown: %v", err)
			}
		})
	}
}

func TestTeardownUsesRecordedTmuxSocket(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	socket := filepath.Join(root, "rig.sock")
	session := "rig-recorded-socket"
	if out, err := exec.Command(realTmux, "-S", socket, "-f", "/dev/null",
		"new-session", "-d", "-s", session).CombinedOutput(); err != nil {
		t.Fatalf("starting tmux server: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(realTmux, "-S", socket, "kill-server").Run() })

	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper := fmt.Sprintf("#!/bin/sh\nexec %s -S %s \"$@\"\n", shellQuote(realTmux), shellQuote(socket))
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if got := tmuxSocketPath(); got != socket {
		t.Fatalf("tmuxSocketPath() = %q, want %q", got, socket)
	}

	// Model the systemd worker: the caller's TMUX connection is gone and its
	// default socket directory points somewhere unrelated. The recorded socket
	// must still be sufficient to find and kill the exact session.
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", filepath.Join(root, "wrong-tmux-dir"))
	if err := tmuxKillSessionAt(session, socket); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(realTmux, "-S", socket, "has-session", "-t", "="+session).Run(); err == nil {
		t.Errorf("tmux session %s survived socket-addressed teardown", session)
	}
}

func TestTeardownRetainsTmuxConnectionErrors(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\necho connection failed >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "tmux.sock")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	err := tmuxKillSessionAt("rig-fail-closed", socket)
	if err == nil || !strings.Contains(err.Error(), "connection failed") {
		t.Fatalf("tmuxKillSessionAt error = %v, want connection failure", err)
	}
}

func TestStopUserScopeAcceptsAlreadyGone(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'Failed to stop test.scope: Unit test.scope not loaded.' >&2\nexit 5\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := stopUserScope("test.scope"); err != nil {
		t.Fatalf("already-removed scope should be idempotent: %v", err)
	}
}

func TestTeardownQuarantinesBeforeRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	t.Setenv("PATH", t.TempDir()) // Skip optional iso and docker cleanup.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	t.Run("permission failure leaves only quarantined debris", func(t *testing.T) {
		root := t.TempDir()
		basedir := filepath.Join(root, "rig")
		bin := filepath.Join(basedir, "runtime", "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "miren"), nil, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(bin, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(bin, 0o755) })

		err := teardownRig(basedir, manifest{ID: "mir-locked"})
		if err == nil {
			t.Fatal("permission failure should retain the durable teardown job")
		}
		if _, err := os.Stat(basedir); !os.IsNotExist(err) {
			t.Errorf("canonical basedir still exists after quarantine: %v", err)
		}

		trashRoot := filepath.Join(root, ".rig-trash")
		entries, err := os.ReadDir(trashRoot)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "rig-") {
			t.Fatalf("expected one clearly named quarantined rig, got %v", entries)
		}
		quarantinedBin := filepath.Join(trashRoot, entries[0].Name(), "runtime", "bin")
		if _, err := os.Stat(filepath.Join(quarantinedBin, "miren")); err != nil {
			t.Errorf("root-owned stand-in was not left in quarantine: %v", err)
		}
		jobs, err := pendingTeardownJobs()
		if err != nil || len(jobs) != 1 {
			t.Fatalf("pending jobs = %v, %v; want one retryable tombstone", jobs, err)
		}
		if err := os.Chmod(quarantinedBin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := executeTeardownJobFile(jobs[0], false); err != nil {
			t.Fatalf("retrying durable teardown: %v", err)
		}
		if _, err := os.Stat(jobs[0]); !os.IsNotExist(err) {
			t.Errorf("completed teardown job still exists: %v", err)
		}
	})

	t.Run("successful removal cleans up the quarantine", func(t *testing.T) {
		root := t.TempDir()
		basedir := filepath.Join(root, "rig")
		if err := os.Mkdir(basedir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(basedir, "scratch"), nil, 0o644); err != nil {
			t.Fatal(err)
		}

		if err := teardownRig(basedir, manifest{}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(basedir); !os.IsNotExist(err) {
			t.Errorf("canonical basedir still exists: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, ".rig-trash")); !os.IsNotExist(err) {
			t.Errorf("empty quarantine directory still exists: %v", err)
		}
	})

	t.Run("agent scratch is removed with the rig", func(t *testing.T) {
		root := t.TempDir()
		tmp := filepath.Join(root, "tmp")
		t.Setenv("TMPDIR", tmp)
		basedir := filepath.Join(root, "workspaces", "mir-3-scratch")
		if err := os.MkdirAll(basedir, 0o755); err != nil {
			t.Fatal(err)
		}
		project := filepath.Join(tmp, "claude-"+strconv.Itoa(os.Getuid()), claudeProjectDirName(basedir))
		scratch := filepath.Join(project, "session-id", "scratchpad")
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scratch, "dev.log"), []byte("runaway"), 0o644); err != nil {
			t.Fatal(err)
		}

		if err := teardownRig(basedir, manifest{ID: "mir-3"}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(project); !os.IsNotExist(err) {
			t.Errorf("Claude scratch project still exists: %v", err)
		}
	})

	// Trust is seeded when the rig's directories are made, so it comes back when
	// they're destroyed — otherwise codex accumulates a stanza per rig you ever
	// tore down.
	t.Run("codex directory trust is dropped with the rig", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
			t.Fatal(err)
		}
		basedir := filepath.Join(home, "workspaces", "mir-4-trust")
		if err := os.MkdirAll(filepath.Join(basedir, "repo"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := seedCodexTrust(home, basedir, filepath.Join(basedir, "repo")); err != nil {
			t.Fatal(err)
		}

		if err := teardownRig(basedir, manifest{ID: "mir-4"}); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), "[projects.") {
			t.Errorf("codex config still trusts the torn-down rig:\n%s", got)
		}
	})
}
