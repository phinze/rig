package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Trust is authority, so the seeding call site is bounded to the tree rig
// creates. A fixture built somewhere else — which is every test that calls
// createBasedir with a bare t.TempDir() — must leave the real configs alone.
func TestSeedTrustForStaysInsideRigsRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	codexPath := filepath.Join(home, ".codex", "config.toml")
	claudePath := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudePath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	seedTrustFor(filepath.Join(t.TempDir(), "rig"), "/etc")
	if _, err := os.Stat(codexPath); !os.IsNotExist(err) {
		t.Errorf("seeded codex outside the rigs root: %s", readConfig(t, codexPath))
	}
	if got := readConfig(t, claudePath); got != "{}" {
		t.Errorf("seeded claude outside the rigs root: %s", got)
	}

	inside := filepath.Join(home, "workspaces", "mir-9")
	seedTrustFor(inside)
	if got := readConfig(t, codexPath); !strings.Contains(got, inside) {
		t.Errorf("codex config missing the rig it should have seeded:\n%s", got)
	}
	if !claudeTrusts(t, claudePath, inside) {
		t.Errorf("claude config missing the rig it should have seeded:\n%s", readConfig(t, claudePath))
	}
}
