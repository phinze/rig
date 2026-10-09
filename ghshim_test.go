package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Agents in rigs that predate colocated workspaces still run gh through the
// shim, carrying whatever GH_REPO they started with. The shim has to drop it so
// gh reads the repo from cwd, and must never find itself as the real gh.
func TestGHShimDropsStaleRepoContext(t *testing.T) {
	root := t.TempDir()
	basedir := filepath.Join(root, "rig")
	shimDir := filepath.Join(basedir, ".rig", "bin")
	staleBasedir := filepath.Join(root, "stale-rig")
	staleShimDir := filepath.Join(staleBasedir, ".rig", "bin")
	realBin := filepath.Join(root, "real-bin")
	cloud := filepath.Join(basedir, "cloud")
	for _, dir := range []string{realBin, cloud} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{shimDir, staleShimDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(ghShim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(root, "invocation")
	realGH := "#!/bin/sh\nprintf '%s\\n%s\\n%s\\n' \"$GH_REPO\" \"$PATH\" \"$*\" > " + shellQuote(marker) + "\n"
	if err := os.WriteFile(filepath.Join(realBin, "gh"), []byte(realGH), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Chdir(cloud)
	t.Setenv("RIG_BASEDIR", basedir)
	t.Setenv("GH_REPO", "mirendev/runtime") // stale agent-start context
	t.Setenv("PATH", strings.Join([]string{shimDir, staleShimDir, realBin}, string(os.PathListSeparator)))
	if err := runGHShim([]string{"pr", "create", "--dry-run"}); err != nil {
		t.Fatal(err)
	}

	blob, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(blob), "\n"), "\n")
	if got := lines[0]; got != "" {
		t.Errorf("GH_REPO = %q, want it unset", got)
	}
	if strings.Contains(lines[1], shimDir) {
		t.Errorf("real gh PATH still contains current shim (would recurse): %q", lines[1])
	}
	if strings.Contains(lines[1], staleShimDir) {
		t.Errorf("real gh PATH still contains stale shim (would recurse): %q", lines[1])
	}
	if got, want := lines[2], "pr create --dry-run"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestCreateBasedirWritesNoGHShim(t *testing.T) {
	basedir := filepath.Join(t.TempDir(), "rig")
	if err := createBasedir(basedir, manifest{ID: "mir-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(basedir, ".rig", "bin", "gh")); !os.IsNotExist(err) {
		t.Errorf("createBasedir wrote a gh shim (stat err = %v); colocated workspaces don't need one", err)
	}
}
