package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phinze/rig/internal/mux/rex"
)

// The [surfaces] table is hand-edited, but the file is rewritten whole by
// every `rig config` write, so it has to survive one: every entry, including
// one rig can't use, and without a later table's keys leaking into it.
func TestSurfacesSurviveAConfigWrite(t *testing.T) {
	isolateRigConfig(t)
	path, err := rigConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	hand := `agent = "claude"

[surfaces]
devbox = "rex+https://devbox.example.ts.net"
"Bad Name" = "rex+https://x"

[someday]
elsewhere = "not a surface"
`
	if err := os.WriteFile(path, []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runConfigCmd([]string{"agent", "cdx"}); err != nil {
		t.Fatal(err)
	}
	c := readRigConfig()
	if c.Agent != "codex" {
		t.Errorf("agent = %q, want codex", c.Agent)
	}
	want := map[string]string{"devbox": "rex+https://devbox.example.ts.net", "Bad Name": "rex+https://x"}
	if len(c.Surfaces) != len(want) {
		t.Fatalf("surfaces = %v, want %v", c.Surfaces, want)
	}
	for k, v := range want {
		if c.Surfaces[k] != v {
			t.Errorf("surface %q = %q, want %q", k, c.Surfaces[k], v)
		}
	}

	if rex.Installed() {
		var surfaces []string
		for _, b := range surfaceBackends() {
			surfaces = append(surfaces, b.Surface())
		}
		if strings.Join(surfaces, ",") != "rex@devbox" {
			t.Errorf("configured surfaces = %v, want only rex@devbox", surfaces)
		}
		if b, err := backendByName("rex"); err != nil || b.Surface() != "rex" {
			t.Errorf("the rex kind resolved to %v (%v), want the local instance", b, err)
		}
	}
}

func TestParseSurface(t *testing.T) {
	for _, tc := range []struct {
		place, spec, surface, problem string
	}{
		{"devbox", "rex+https://devbox.example.ts.net", "rex@devbox", ""},
		{"scratch", "rex+unix:///tmp/s.sock", "rex@scratch", ""},
		{"devbox", "rex+tailnet://devbox", "", "listen-only"},
		{"devbox", "https://devbox", "", "kind+endpoint"},
		{"devbox", "tmux+ssh://devbox", "tmux@devbox", ""},
		{"devbox", "tmux+devbox", "", "ssh://host"},
		{"devbox", "screen+ssh://devbox", "", "no remote form"},
		{"Dev Box", "rex+https://x", "", "surface name"},
	} {
		b, err := parseSurface(tc.place, tc.spec)
		switch {
		case tc.problem == "" && (err != nil || b.Surface() != tc.surface):
			t.Errorf("%s = %s: got %v, %v; want %s", tc.place, tc.spec, b, err, tc.surface)
		case tc.problem != "" && (err == nil || !strings.Contains(err.Error(), tc.problem)):
			t.Errorf("%s = %s: err %v, want one mentioning %q", tc.place, tc.spec, err, tc.problem)
		}
	}
}

func TestRemoteTitleFoldsTheFarHome(t *testing.T) {
	for path, want := range map[string]string{
		"/home/phinze/workspaces/foo":  "devbox:~/workspaces/foo",
		"/Users/phinze/workspaces/foo": "devbox:~/workspaces/foo",
		"/srv/app":                     "devbox:/srv/app",
		"":                             "devbox:",
	} {
		if got := remoteTitle("devbox", path); got != want {
			t.Errorf("remoteTitle(%q) = %q, want %q", path, got, want)
		}
	}
}
