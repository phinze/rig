package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// claudeHome builds a fake ~ holding a ~/.claude.json with body, and returns the
// home and the config path. An empty body means claude was never run here.
func claudeHome(t *testing.T, body string) (string, string) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	return home, path
}

func claudeProjects(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	var c struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal([]byte(readConfig(t, path)), &c); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return c.Projects
}

func claudeTrusts(t *testing.T, path, dir string) bool {
	t.Helper()
	return claudeProjects(t, path)[dir]["hasTrustDialogAccepted"] == true
}

func TestSeedClaudeTrustPreservesEverythingElse(t *testing.T) {
	home, path := claudeHome(t, `{
  "numStartups": 12,
  "tip": "<b>&</b>",
  "projects": {
    "/w/rig-a/repo": {"allowedTools": ["Bash"], "lastCost": 1.25}
  }
}`)
	if err := seedClaudeTrust(home, "/w/rig-a/repo", "/w/rig-b"); err != nil {
		t.Fatalf("seedClaudeTrust: %v", err)
	}
	for _, dir := range []string{"/w/rig-a/repo", "/w/rig-b"} {
		if !claudeTrusts(t, path, dir) {
			t.Errorf("%s not trusted:\n%s", dir, readConfig(t, path))
		}
	}
	entry := claudeProjects(t, path)["/w/rig-a/repo"]
	if entry["lastCost"] != 1.25 || entry["allowedTools"] == nil {
		t.Errorf("existing project fields lost: %v", entry)
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(readConfig(t, path)), &top); err != nil {
		t.Fatal(err)
	}
	if top["numStartups"] != float64(12) || top["tip"] != "<b>&</b>" {
		t.Errorf("top-level fields not preserved: %v", top)
	}
}

// Nothing to change means nothing written: claude rewrites this file constantly,
// and every write of ours is a chance to step on one of its.
func TestSeedClaudeTrustSkipsWriteWhenTrusted(t *testing.T) {
	body := `{"projects":{"/w/rig-a":{"hasTrustDialogAccepted":true}}}`
	home, path := claudeHome(t, body)
	if err := seedClaudeTrust(home, "/w/rig-a"); err != nil {
		t.Fatalf("seedClaudeTrust: %v", err)
	}
	if got := readConfig(t, path); got != body {
		t.Errorf("config rewritten with nothing to seed:\n%s", got)
	}
}

func TestSeedClaudeTrustFlipsFalse(t *testing.T) {
	home, path := claudeHome(t, `{"projects":{"/w/rig-a":{"hasTrustDialogAccepted":false}}}`)
	if err := seedClaudeTrust(home, "/w/rig-a"); err != nil {
		t.Fatalf("seedClaudeTrust: %v", err)
	}
	if !claudeTrusts(t, path, "/w/rig-a") {
		t.Errorf("false flag not flipped:\n%s", readConfig(t, path))
	}
}

// No ~/.claude.json means claude was never run here, and rig shouldn't invent
// one for a tool that isn't set up.
func TestSeedClaudeTrustSkipsWithoutConfig(t *testing.T) {
	home, path := claudeHome(t, "")
	if err := seedClaudeTrust(home, "/w/rig-a"); err != nil {
		t.Fatalf("seedClaudeTrust: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("created ~/.claude.json when claude isn't set up (err = %v)", err)
	}
}

func TestSeedClaudeTrustHonorsConfigDir(t *testing.T) {
	home, homePath := claudeHome(t, `{}`)
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := seedClaudeTrust(home, "/w/rig-a"); err != nil {
		t.Fatalf("seedClaudeTrust: %v", err)
	}
	if !claudeTrusts(t, path, "/w/rig-a") {
		t.Errorf("CLAUDE_CONFIG_DIR config not seeded:\n%s", readConfig(t, path))
	}
	if got := readConfig(t, homePath); got != "{}" {
		t.Errorf("seeded ~/.claude.json despite CLAUDE_CONFIG_DIR:\n%s", got)
	}
}

// Claude keys projects by realpath, so a symlinked spelling alone would miss.
func TestSeedClaudeTrustSeedsResolvedSpelling(t *testing.T) {
	home, path := claudeHome(t, `{}`)
	real := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := seedClaudeTrust(home, link); err != nil {
		t.Fatalf("seedClaudeTrust: %v", err)
	}
	for _, want := range []string{link, resolvePath(real)} {
		if !claudeTrusts(t, path, want) {
			t.Errorf("%s not trusted:\n%s", want, readConfig(t, path))
		}
	}
}

func TestDropClaudeTrustRemovesRigEntries(t *testing.T) {
	home, path := claudeHome(t, `{
  "numStartups": 3,
  "projects": {
    "/w/keep": {"hasTrustDialogAccepted": true},
    "/w/rig-abc": {"hasTrustDialogAccepted": true},
    "/w/rig-a": {"hasTrustDialogAccepted": true},
    "/w/rig-a/repo": {"hasTrustDialogAccepted": true, "lastCost": 2}
  }
}`)
	if err := dropClaudeTrust(home, "/w/rig-a"); err != nil {
		t.Fatalf("dropClaudeTrust: %v", err)
	}
	projects := claudeProjects(t, path)
	for _, gone := range []string{"/w/rig-a", "/w/rig-a/repo"} {
		if _, ok := projects[gone]; ok {
			t.Errorf("config still has %s:\n%s", gone, readConfig(t, path))
		}
	}
	// /w/rig-abc is not under /w/rig-a, however much the prefix suggests it.
	for _, kept := range []string{"/w/keep", "/w/rig-abc"} {
		if _, ok := projects[kept]; !ok {
			t.Errorf("config lost %s:\n%s", kept, readConfig(t, path))
		}
	}
}

func TestDropClaudeTrustNoopWithoutMatch(t *testing.T) {
	body := `{"projects":{"/w/other":{"hasTrustDialogAccepted":true}}}`
	home, path := claudeHome(t, body)
	if err := dropClaudeTrust(home, "/w/rig-a"); err != nil {
		t.Fatalf("dropClaudeTrust: %v", err)
	}
	if got := readConfig(t, path); got != body {
		t.Errorf("config rewritten with nothing to drop:\n%s", got)
	}
}

// Round trip: what seed writes is what drop recognizes.
func TestClaudeTrustRoundTrip(t *testing.T) {
	home, path := claudeHome(t, `{}`)
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := seedClaudeTrust(home, base, repo); err != nil {
		t.Fatalf("seedClaudeTrust: %v", err)
	}
	if err := dropClaudeTrust(home, resolvePath(base)); err != nil {
		t.Fatalf("dropClaudeTrust: %v", err)
	}
	if projects := claudeProjects(t, path); len(projects) != 0 {
		t.Errorf("seeded entries survived teardown: %v", projects)
	}
}
