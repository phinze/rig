package rex

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phinze/rig/internal/mux"
)

// fakeRex puts a `rex` on PATH that answers the API methods Panes and friends
// call with canned JSON and logs every invocation, so the parsing and the
// sidecar can be tested without a Rex server. It's a shell script because
// that's exactly the boundary the real thing crosses.
func fakeRex(t *testing.T) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "calls.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + log + `"
method=""
for a in "$@"; do case "$a" in *.*) method="$a";; esac; done
case "$method" in
session.list) echo '{"sessions":[{"session_id":"session:1","label":"~-workspaces-alpha"}]}' ;;
session.view) cat <<'EOF'
{"session_id":"session:1","label":"~-workspaces-alpha","windows":[
 {"window_id":"window:m","label":"main/alpha","active":true,"focused_block_id":"block:a","layers":[
   {"kind":"tiled","blocks":[{"block_id":"block:a","label":"shell"},{"block_id":"block:r","label":"recto"}]},
   {"kind":"floating","blocks":[{"block_id":"block:popup","label":"rig radar"}]}]},
 {"window_id":"window:z","label":"zeta","active":false,"focused_block_id":"block:z","layers":[
   {"kind":"tiled","blocks":[{"block_id":"block:z","label":"recto"}]}]}]}
EOF
;;
session.list_blocks) echo '{"blocks":[{"block_id":"block:a"},{"block_id":"block:r"},{"block_id":"block:z"}]}' ;;
com.superlogical.terminal.process)
  case "$*" in
    *block:a*) echo '{"foreground":{"name":".claude-unwrapped","cwd":"/w/alpha"}}' ;;
    *) echo '{"foreground":{"name":"recto","cwd":"/w/x"}}' ;;
  esac ;;
com.superlogical.terminal.title) echo '{"title":"✳ Task A"}' ;;
com.superlogical.terminal.size) echo '{"columns":300,"rows":100}' ;;
session.new_layer) echo '{"layer_id":"layer:1","block_ids":["block:p"],"revision":3}' ;;
session.set_window_label|session.focus_block|session.move_block) echo '{"revision":2}' ;;
*) echo '{}' ;;
esac
`
	path := filepath.Join(bin, "rex")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return log
}

// Panes walks session.view's tiled layers in window order, skips floating
// layers (the radar popup is not a pane), fills Target with the block id, and
// merges the sidecar marks back in.
func TestPanesReadsViewAndMarks(t *testing.T) {
	fakeRex(t)
	b := Backend{MarksDir: t.TempDir()}
	if err := b.MarkPane("block:a", "agent", "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := b.MarkWindow("window:m", "main", "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := b.MarkWindow("window:z", "repo", "zeta"); err != nil {
		t.Fatal(err)
	}

	panes, err := b.Panes("~-workspaces-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(panes) != 3 {
		t.Fatalf("panes = %d (%+v), want 3 with the floating layer skipped", len(panes), panes)
	}
	a := panes[0]
	if a.Backend != "rex" || a.PaneID != "block:a" || a.Target != "block:a" || a.WindowID != "window:m" || a.WindowName != "main/alpha" {
		t.Errorf("identity = %+v", a)
	}
	if a.WindowIdx != "0" || a.PaneIdx != "0" || panes[1].PaneIdx != "1" || panes[2].WindowIdx != "1" {
		t.Errorf("ordering = %s.%s %s.%s %s.%s", a.WindowIdx, a.PaneIdx, panes[1].WindowIdx, panes[1].PaneIdx, panes[2].WindowIdx, panes[2].PaneIdx)
	}
	if a.Role != "agent" || a.Repo != "alpha" || a.WindowRole != "main" || a.WindowRepo != "alpha" {
		t.Errorf("marks = %+v", a)
	}
	if a.Command != ".claude-unwrapped" || a.Path != "/w/alpha" || a.Title != "✳ Task A" {
		t.Errorf("process = %+v", a)
	}
	if r := panes[1]; r.Role != "" || r.WindowRole != "main" {
		t.Errorf("unmarked recto in main = %+v", r)
	}
	if z := panes[2]; z.WindowRole != "repo" || z.WindowRepo != "zeta" || z.WindowName != "zeta" {
		t.Errorf("repo window = %+v", z)
	}

	all, err := b.AllPanes()
	if err != nil || len(all) != 3 {
		t.Errorf("AllPanes = %d, %v; want the same 3", len(all), err)
	}
}

// Marks are a file per session, found from a block or window id via the
// session that owns it, and KillSession takes the file with the session.
func TestMarksSidecarLifecycle(t *testing.T) {
	fakeRex(t)
	b := Backend{MarksDir: filepath.Join(t.TempDir(), "marks")}
	if err := b.MarkPane("block:r", "recto", "alpha"); err != nil {
		t.Fatal(err)
	}
	path := b.marksPath("~-workspaces-alpha")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("sidecar not written at %s: %v", path, err)
	}
	if got := b.readMarks("~-workspaces-alpha")["block:r"]; got.Role != "recto" || got.Repo != "alpha" {
		t.Errorf("read back %+v", got)
	}
	// Marking twice keeps both entries: one file per session, one key per id.
	if err := b.MarkPane("block:a", "agent", "alpha"); err != nil {
		t.Fatal(err)
	}
	if n := len(b.readMarks("~-workspaces-alpha")); n != 2 {
		t.Errorf("entries = %d, want 2", n)
	}
	b.forgetMarks("~-workspaces-alpha")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("sidecar survived forget: %v", err)
	}
	// Marks disabled: writes are no-ops rather than errors.
	if err := (Backend{}).MarkPane("block:a", "agent", "alpha"); err != nil {
		t.Errorf("markless backend errored: %v", err)
	}
}

// A recorded unix endpoint whose socket is gone means the server is gone, and
// that's a clean no-op, not a failure that would keep a teardown job alive.
func TestKillSessionAtMissingSocket(t *testing.T) {
	fakeRex(t)
	gone := filepath.Join(t.TempDir(), "server.sock")
	if err := (Backend{}).KillSessionAt("~-workspaces-alpha", "unix://"+gone); err != nil {
		t.Errorf("missing socket should be a no-op, got %v", err)
	}
}

// Inside Rex there is no client switch, and the caller is told so with the
// sentinel rather than a failed command.
func TestAttachInsideRexIsNoClientSwitch(t *testing.T) {
	t.Setenv("REX_SESSION", "session:1")
	if err := (Backend{}).Attach("~-workspaces-alpha"); !errors.Is(err, mux.ErrNoClientSwitch) {
		t.Errorf("attach inside rex = %v, want ErrNoClientSwitch", err)
	}
}

// Popup opens one layer in the session running the command through a login
// shell, sized from the focused block's grid (120 columns of 300 and 32 rows
// of 100, centred) and told that size and its colour.
func TestPopupOpensASizedLayer(t *testing.T) {
	log := fakeRex(t)
	if err := (Backend{}).Popup("~-workspaces-alpha", "/usr/local/bin/rig radar"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(log)
	var layer string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.Contains(line, "session.new_layer") {
			layer = line
		}
	}
	if layer == "" {
		t.Fatalf("no new_layer call in:\n%s", raw)
	}
	for _, want := range []string{`"x":0.3`, `"w":0.7`, `"focus":true`, `"-lc"`, `COLUMNS=120 LINES=32 RIG_RADAR_BG=#181825 /usr/local/bin/rig radar`, `"on_completion":true`} {
		if !strings.Contains(layer, want) {
			t.Errorf("new_layer call lacks %s:\n%s", want, layer)
		}
	}
}
