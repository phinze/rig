package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One setting carries web-first mode: review rigs stop opening with a
// terminal Recto, and rig recto stops reaching for the session's panes
// unless --pane asks.
func TestWebFirstKeepsTheTerminalRectoBack(t *testing.T) {
	isolateRigConfig(t)

	review := manifest{Kind: "review"}
	if !review.opensRecto() || webFirst() {
		t.Fatal("by default a review rig opens with a terminal Recto")
	}
	if err := runConfigCmd([]string{"recto", "tab"}); err == nil {
		t.Error("recto takes web or terminal only")
	}
	if err := runConfigCmd([]string{"recto", "web"}); err != nil {
		t.Fatal(err)
	}
	if !webFirst() || review.opensRecto() || pickupRectoCommand() != "" {
		t.Fatal("web first should keep a review rig's terminal Recto closed, PR pickups included")
	}

	// A rig with no session: promoting a pane would fail, so getting through
	// shows rig recto never reached for one.
	basedir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(basedir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(basedir, manifest{ID: "web-first-test", Repos: map[string]string{"repo": "o/repo"}}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(basedir)
	if err := runRecto([]string{"repo"}); err != nil {
		t.Fatalf("web first, rig recto repo = %v; want it to leave the panes alone", err)
	}
	if err := runRecto([]string{"--pane", "repo"}); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("--pane should still reach for the pane, got %v", err)
	}

	if err := runConfigCmd([]string{"recto", "--unset"}); err != nil {
		t.Fatal(err)
	}
	if err := runRecto([]string{"repo"}); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("one unset should bring the panes back, got %v", err)
	}
}
