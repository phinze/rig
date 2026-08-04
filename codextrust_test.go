package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexHome builds a fake ~ with a ~/.codex holding body, and returns the home
// and the config path.
func codexHome(t *testing.T, body string) (string, string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "config.toml")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	return home, path
}

func readConfig(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return string(body)
}

func TestSeedCodexTrustAppends(t *testing.T) {
	home, path := codexHome(t, "model = \"gpt-5\"\n")
	if err := seedCodexTrust(home, "/w/rig-a/repo"); err != nil {
		t.Fatalf("seedCodexTrust: %v", err)
	}
	got := readConfig(t, path)
	if !strings.Contains(got, "[projects.\"/w/rig-a/repo\"]\ntrust_level = \"trusted\"\n") {
		t.Errorf("config missing trust stanza:\n%s", got)
	}
	if !strings.HasPrefix(got, "model = \"gpt-5\"\n") {
		t.Errorf("existing config not preserved:\n%s", got)
	}
}

// A duplicate TOML table is a parse error, not a harmless repeat: re-seeding an
// entry codex (or an earlier rig) already recorded would break its config load.
func TestSeedCodexTrustIsIdempotent(t *testing.T) {
	home, path := codexHome(t, "[projects.\"/w/rig-a/repo\"]\ntrust_level = \"trusted\"\n")
	for range 2 {
		if err := seedCodexTrust(home, "/w/rig-a/repo"); err != nil {
			t.Fatalf("seedCodexTrust: %v", err)
		}
	}
	if n := strings.Count(readConfig(t, path), "[projects."); n != 1 {
		t.Errorf("project stanzas = %d, want 1:\n%s", n, readConfig(t, path))
	}
}

func TestSeedCodexTrustCreatesMissingConfig(t *testing.T) {
	home, path := codexHome(t, "")
	if err := seedCodexTrust(home, "/w/rig-a"); err != nil {
		t.Fatalf("seedCodexTrust: %v", err)
	}
	if got := readConfig(t, path); !strings.Contains(got, "/w/rig-a") {
		t.Errorf("config = %q, want the seeded dir", got)
	}
}

// No ~/.codex means codex was never set up here, and rig shouldn't invent a
// config directory for a tool that isn't installed.
func TestSeedCodexTrustSkipsWithoutCodexHome(t *testing.T) {
	home := t.TempDir()
	if err := seedCodexTrust(home, "/w/rig-a"); err != nil {
		t.Fatalf("seedCodexTrust: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Errorf("created ~/.codex when codex isn't set up (err = %v)", err)
	}
}

// A rig reached through a symlinked ~/workspaces is a different string than the
// one codex canonicalizes cwd to, and the trust gate matches on the string.
func TestSeedCodexTrustSeedsResolvedSpelling(t *testing.T) {
	home, path := codexHome(t, "")
	real := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := seedCodexTrust(home, link); err != nil {
		t.Fatalf("seedCodexTrust: %v", err)
	}
	got := readConfig(t, path)
	for _, want := range []string{link, resolvePath(real)} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
}

func TestDropCodexTrustRemovesRigEntries(t *testing.T) {
	home, path := codexHome(t, strings.Join([]string{
		"model = \"gpt-5\"",
		"",
		"[projects.\"/w/keep\"]",
		"trust_level = \"trusted\"",
		"",
		"[projects.\"/w/rig-a\"]",
		"trust_level = \"trusted\"",
		"",
		"[projects.\"/w/rig-a/repo\"]",
		"trust_level = \"trusted\"",
		"",
		"[hooks.state]",
		"",
	}, "\n"))

	if err := dropCodexTrust(home, "/w/rig-a"); err != nil {
		t.Fatalf("dropCodexTrust: %v", err)
	}
	got := readConfig(t, path)
	for _, gone := range []string{"/w/rig-a\"", "/w/rig-a/repo\""} {
		if strings.Contains(got, gone) {
			t.Errorf("config still has %s:\n%s", gone, got)
		}
	}
	// The rig's stanzas sat between two tables that must survive intact —
	// dropping to the next header, not to EOF.
	for _, want := range []string{"model = \"gpt-5\"", "[projects.\"/w/keep\"]", "[hooks.state]"} {
		if !strings.Contains(got, want) {
			t.Errorf("config lost %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("removal left a widening gap:\n%q", got)
	}
}

// /w/rig-abc is not under /w/rig-a, however much the prefix suggests it.
func TestDropCodexTrustKeepsSiblingPrefixes(t *testing.T) {
	home, path := codexHome(t, strings.Join([]string{
		"[projects.\"/w/rig-abc\"]",
		"trust_level = \"trusted\"",
		"",
	}, "\n"))
	if err := dropCodexTrust(home, "/w/rig-a"); err != nil {
		t.Fatalf("dropCodexTrust: %v", err)
	}
	if got := readConfig(t, path); !strings.Contains(got, "/w/rig-abc") {
		t.Errorf("dropped a sibling rig:\n%s", got)
	}
}

func TestDropCodexTrustNoopWithoutMatch(t *testing.T) {
	body := "[projects.\"/w/other\"]\ntrust_level = \"trusted\"\n"
	home, path := codexHome(t, body)
	if err := dropCodexTrust(home, "/w/rig-a"); err != nil {
		t.Fatalf("dropCodexTrust: %v", err)
	}
	if got := readConfig(t, path); got != body {
		t.Errorf("config rewritten with nothing to drop:\n%q", got)
	}
}

// Round trip: what seed writes is what drop recognizes.
func TestCodexTrustRoundTrip(t *testing.T) {
	home, path := codexHome(t, "")
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := seedCodexTrust(home, base, repo); err != nil {
		t.Fatalf("seedCodexTrust: %v", err)
	}
	if err := dropCodexTrust(home, resolvePath(base)); err != nil {
		t.Fatalf("dropCodexTrust: %v", err)
	}
	if got := readConfig(t, path); strings.Contains(got, "[projects.") {
		t.Errorf("seeded entries survived teardown:\n%s", got)
	}
}

// Trust is authority, so the seeding call site is bounded to the tree rig
// creates. A fixture built somewhere else — which is every test that calls
// createBasedir with a bare t.TempDir() — must leave the real config alone.
func TestSeedCodexTrustForStaysInsideRigsRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".codex", "config.toml")

	seedCodexTrustFor(filepath.Join(t.TempDir(), "rig"), "/etc")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("seeded a directory outside the rigs root: %s", readConfig(t, path))
	}

	inside := filepath.Join(home, "workspaces", "mir-9")
	seedCodexTrustFor(inside)
	if got := readConfig(t, path); !strings.Contains(got, inside) {
		t.Errorf("config missing the rig it should have seeded:\n%s", got)
	}
}

func TestCodexProjectHeader(t *testing.T) {
	for _, tc := range []struct {
		line string
		dir  string
		ok   bool
	}{
		{"[projects.\"/w/rig\"]", "/w/rig", true},
		{"  [projects.\"/w/rig\"]  ", "/w/rig", true},
		{"[hooks.state]", "", false},
		{"trust_level = \"trusted\"", "", false},
		{"[projects.unquoted]", "", false},
	} {
		dir, ok := codexProjectHeader(tc.line)
		if ok != tc.ok || dir != tc.dir {
			t.Errorf("codexProjectHeader(%q) = %q, %v; want %q, %v", tc.line, dir, ok, tc.dir, tc.ok)
		}
	}
}
