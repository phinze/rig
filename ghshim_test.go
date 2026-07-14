package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGHShimResolvesRepoFromInvocationCwd(t *testing.T) {
	root := t.TempDir()
	basedir := filepath.Join(root, "rig")
	shimDir := filepath.Join(basedir, ".rig", "bin")
	realBin := filepath.Join(root, "real-bin")
	cloud := filepath.Join(basedir, "cloud")
	for _, dir := range []string{shimDir, realBin, cloud} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeManifest(basedir, manifest{Repos: map[string]string{
		"runtime": "mirendev/runtime",
		"cloud":   "mirendev/cloud",
	}}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "invocation")
	realGH := "#!/bin/sh\nprintf '%s\\n%s\\n%s\\n' \"$GH_REPO\" \"$PATH\" \"$*\" > " + shellQuote(marker) + "\n"
	if err := os.WriteFile(filepath.Join(realBin, "gh"), []byte(realGH), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Chdir(cloud)
	t.Setenv("RIG_BASEDIR", basedir)
	t.Setenv("GH_REPO", "mirendev/runtime") // stale agent-start context
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+realBin)
	if err := runGHShim([]string{"pr", "create", "--dry-run"}); err != nil {
		t.Fatal(err)
	}

	blob, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(blob)), "\n")
	if got, want := lines[0], "mirendev/cloud"; got != want {
		t.Errorf("GH_REPO = %q, want %q", got, want)
	}
	if strings.Contains(lines[1], shimDir) {
		t.Errorf("real gh PATH still contains shim (would recurse): %q", lines[1])
	}
	if got, want := lines[2], "pr create --dry-run"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestCreateBasedirWritesGHShim(t *testing.T) {
	basedir := filepath.Join(t.TempDir(), "rig")
	if err := createBasedir(basedir, manifest{ID: "mir-1"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(basedir, ".rig", "bin", "gh")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("%s is not executable: %v", path, info.Mode())
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeRigShims(basedir); err != nil {
		t.Fatal(err)
	}
	if info, err = os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode()&0o111 == 0 {
		t.Errorf("existing shim was not repaired: %v", info.Mode())
	}
}
