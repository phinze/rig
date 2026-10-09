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
if [ -n "$REX_FAKE_DOWN" ]; then echo 'Error: connecting to the server timed out after 3s: context deadline exceeded' >&2; exit 1; fi
case "$*" in
*"client ls --json"*)
  echo '{"clients":[{"client_id":"client:app","info":{"kind":"app"}},{"client_id":"client:cli","info":{"kind":"cli"}}]}'
  exit 0 ;;
*" do session.select "*)
  if [ -n "$REX_FAKE_NO_REMOTE_CONTROL" ]; then
    echo 'Error: Rex.app is not accepting actions. Turn on Remote Control in its Settings under Rex Server.' >&2; exit 1
  fi
  echo null; exit 0 ;;
esac
method=""
for a in "$@"; do case "$a" in *.*) method="$a";; esac; done
case "$method" in
session.list)
  if [ -n "$REX_FAKE_SECOND" ]; then
    echo '{"sessions":[{"session_id":"session:1","label":"~-workspaces-alpha"},{"session_id":"session:2","label":"~-workspaces-beta"}]}'
  else
    echo '{"sessions":[{"session_id":"session:1","label":"~-workspaces-alpha"}]}'
  fi ;;
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
    *block:r*) if [ -n "$REX_FAKE_R_EXITED" ]; then echo '{"last_exit":{"exit_code":255,"error":"exit status 255"}}'; exit 0; fi
      echo '{"foreground":{"name":"recto","cwd":"/w/x"}}' ;;
    *block:a*) echo '{"foreground":{"name":".claude-unwrapped","argv0":"/etc/profiles/per-user/phinze/bin/claude","cwd":"/w/alpha"}}' ;;
    *) echo '{"foreground":{"name":"recto","cwd":"/w/x"}}' ;;
  esac ;;
com.superlogical.terminal.title) echo '{"title":"✳ Task A"}' ;;
com.superlogical.terminal.size) echo '{"columns":300,"rows":100}' ;;
session.new_layer) echo '{"layer_id":"layer:1","block_ids":["block:p"],"revision":3}' ;;
session.new_window) echo '{"window_id":"window:n","block_ids":["block:n"],"revision":4}' ;;
block.close) echo '{"block_id":"block:r","revision":5}' ;;
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
	if a.Surface != "rex" || a.PaneID != "block:a" || a.Target != "block:a" || a.WindowID != "window:m" || a.WindowName != "main/alpha" {
		t.Errorf("identity = %+v", a)
	}
	if a.WindowIdx != "0" || a.PaneIdx != "0" || panes[1].PaneIdx != "1" || panes[2].WindowIdx != "1" {
		t.Errorf("ordering = %s.%s %s.%s %s.%s", a.WindowIdx, a.PaneIdx, panes[1].WindowIdx, panes[1].PaneIdx, panes[2].WindowIdx, panes[2].PaneIdx)
	}
	if a.Role != "agent" || a.Repo != "alpha" || a.WindowRole != "main" || a.WindowRepo != "alpha" {
		t.Errorf("marks = %+v", a)
	}
	// argv0's base is the honest name; `name` is the wrapped binary's.
	if a.Command != "claude" || a.Path != "/w/alpha" || a.Title != "✳ Task A" {
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

// Popup opens one layer in the session running the command through a login
// shell, sized from the focused block's grid (120 columns of 300 and 32 rows
// of 100, centred) and told that size and its colour.
// A respawn adds the command in a focused window of the same session and
// closes only the tiled blocks whose command has ended: the live shell, the
// other window, and the floating radar popup all stay.
func TestRespawnCommandSessionKeepsTheSession(t *testing.T) {
	log := fakeRex(t)
	t.Setenv("REX_FAKE_R_EXITED", "1")
	if err := (Backend{}).RespawnCommandSession("~-workspaces-alpha", "local", "/home", "tmux attach -t foo"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(log)
	calls := string(raw)
	if strings.Contains(calls, "session.destroy") || strings.Contains(calls, " kill ") {
		t.Errorf("calls:\n%s\nwant the session kept", calls)
	}
	var window string
	for line := range strings.SplitSeq(calls, "\n") {
		if strings.Contains(line, "session.new_window") {
			window = line
		}
	}
	if !strings.Contains(window, `"focus":true`) || !strings.Contains(window, "tmux attach -t foo") {
		t.Errorf("new_window call = %q, want a focused window running the command", window)
	}
	if n := strings.Count(calls, "block.close"); n != 1 || !strings.Contains(calls, `block.close {"block_id":"block:r"}`) {
		t.Errorf("calls:\n%s\nwant exactly the exited block:r closed", calls)
	}
}

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

// Attach decides among its moves from what's on screen: the session already
// shown is a no-op, a block in it is a focus, and anything elsewhere is the
// app told to show it by id, a block's own window included. A session that
// isn't there is an error rather than a select aimed at nothing.
func TestAttachInsideRexChoosesItsMove(t *testing.T) {
	log := fakeRex(t)
	t.Setenv("REX_SESSION", "session:1")

	b := Backend{}
	if err := b.Attach("~-workspaces-alpha"); err != nil {
		t.Errorf("attaching to the shown session = %v, want nil", err)
	}
	if err := b.Attach("block:a"); err != nil {
		t.Errorf("attaching to a block in the shown session = %v, want nil", err)
	}
	if got := selects(t, log); len(got) != 0 {
		t.Errorf("selected %v; neither case needed the app", got)
	}
	if err := b.Attach("~-workspaces-beta"); err == nil {
		t.Error("attaching to an absent session succeeded")
	}
	if got := selects(t, log); len(got) != 0 {
		t.Errorf("selected a session that does not exist: %v", got)
	}

	t.Setenv("REX_FAKE_SECOND", "1")
	if err := b.Attach("~-workspaces-beta"); err != nil {
		t.Errorf("attaching to a live second session = %v", err)
	}
	got := selects(t, log)
	if len(got) != 1 || !strings.HasPrefix(got[0], `-C client:app do session.select --args {"session_id":"session:2"}`) {
		t.Errorf("selects = %q, want the app told to show session:2", got)
	}
}

// A block in another session is that session shown at the block's window,
// with the block focused first so the window opens on it.
func TestAttachToABlockElsewhereSelectsItsWindow(t *testing.T) {
	log := fakeRex(t)
	t.Setenv("REX_SESSION", "session:2")
	t.Setenv("REX_FAKE_SECOND", "1")

	if err := (Backend{}).Attach("block:z"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(log)
	focus := strings.Index(string(raw), "session.focus_block")
	sel := strings.Index(string(raw), "do session.select")
	if focus < 0 || sel < 0 || focus > sel {
		t.Errorf("want focus_block then session.select, got:\n%s", raw)
	}
	got := selects(t, log)
	if len(got) != 1 || !strings.Contains(got[0], `{"session_id":"session:1","window_id":"window:z"}`) {
		t.Errorf("selects = %q, want session:1 at window:z", got)
	}
}

// With the app's Remote Control off, the switch is refused with the app's
// own reason attached, as ErrNoClientSwitch so the radar keeps it on screen.
func TestAttachReportsRemoteControlOff(t *testing.T) {
	fakeRex(t)
	t.Setenv("REX_SESSION", "session:1")
	t.Setenv("REX_FAKE_SECOND", "1")
	t.Setenv("REX_FAKE_NO_REMOTE_CONTROL", "1")

	err := (Backend{}).Attach("~-workspaces-beta")
	if !errors.Is(err, mux.ErrNoClientSwitch) {
		t.Fatalf("attach = %v, want ErrNoClientSwitch", err)
	}
	if !strings.Contains(err.Error(), "Turn on Remote Control") {
		t.Errorf("attach = %v, want the app's reason", err)
	}
}

func selects(t *testing.T, log string) []string {
	t.Helper()
	raw, _ := os.ReadFile(log)
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if strings.Contains(line, "do session.select") {
			out = append(out, line)
		}
	}
	return out
}

// A Backend aimed at another server sends every call there, tags what it
// lists with its own surface, is never the session this process is inside,
// and never tells the app to switch.
func TestRemoteInstanceTargetsItsServer(t *testing.T) {
	log := fakeRex(t)
	t.Setenv("REX_SESSION", "session:1")
	t.Setenv("REX_FAKE_SECOND", "1")

	b := Backend{Server: "https://devbox.example.ts.net", Place: "devbox"}
	if got := b.Surface(); got != "rex@devbox" {
		t.Errorf("surface = %q, want rex@devbox", got)
	}
	if got := (Backend{}).Surface(); got != "rex" {
		t.Errorf("local surface = %q, want rex", got)
	}
	sessions := b.Sessions()
	if len(sessions) != 2 || sessions[0].Surface != "rex@devbox" {
		t.Errorf("sessions = %+v, want both tagged rex@devbox", sessions)
	}
	panes, err := b.Panes("~-workspaces-alpha")
	if err != nil || len(panes) == 0 || panes[0].Surface != "rex@devbox" {
		t.Errorf("panes = %+v (%v), want tagged rex@devbox", panes, err)
	}
	if cur := b.CurrentSession(); cur != "" {
		t.Errorf("remote CurrentSession = %q, want empty", cur)
	}
	if ep := b.Endpoint(); ep != "https://devbox.example.ts.net" {
		t.Errorf("endpoint = %q, want the instance's server", ep)
	}
	if err := b.Attach("~-workspaces-beta"); !errors.Is(err, mux.ErrNoClientSwitch) {
		t.Errorf("remote attach = %v, want ErrNoClientSwitch", err)
	}
	if got := selects(t, log); len(got) != 0 {
		t.Errorf("selected %v for a remote row", got)
	}

	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !strings.HasPrefix(line, "-S https://devbox.example.ts.net --autostart=false --timeout 3s ") {
			t.Errorf("call went to the default server: %q", line)
		}
	}
}

// A server that couldn't be reached is left alone for a while, so a devbox
// off the network costs the board one timeout, not two per scan forever. A
// server that answers, even with an error, is up.
func TestUnreachableServerIsLeftAlone(t *testing.T) {
	log := fakeRex(t)
	b := Backend{Server: "https://gone.example.ts.net", Place: "gone"}
	t.Cleanup(func() { noteReachability(b.Server, "") })

	t.Setenv("REX_FAKE_DOWN", "1")
	if s := b.Sessions(); len(s) != 0 {
		t.Fatalf("sessions from a down server = %+v", s)
	}
	if panes, _ := b.AllPanes(); len(panes) != 0 {
		t.Errorf("panes from a down server = %+v", panes)
	}
	if n := calls(t, log); n != 1 {
		t.Errorf("down server was asked %d times, want once", n)
	}
	if !b.Unreachable() {
		t.Error("a server that failed to connect doesn't report itself unreachable")
	}
	if (Backend{}).Unreachable() {
		t.Error("the local server reported unreachable; only a remote one can be")
	}

	// Another server isn't tarred with the same brush.
	t.Setenv("REX_FAKE_DOWN", "")
	other := Backend{Server: "https://up.example.ts.net", Place: "up"}
	if s := other.Sessions(); len(s) != 1 {
		t.Errorf("sessions from a live server = %+v, want one", s)
	}
	// And once it answers again, the breaker lets it through.
	noteReachability(b.Server, "Error: session not found")
	if s := b.Sessions(); len(s) != 1 {
		t.Errorf("sessions once back = %+v, want one", s)
	}
}

func calls(t *testing.T, log string) int {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(string(raw)), "\n"))
}

// A viewer has no Rex session of its own and no terminal, so it tells the app
// to show the target rather than falling through to a text-mode attach.
func TestViewerAttachSelectsFromOutsideRex(t *testing.T) {
	log := fakeRex(t)
	t.Setenv("REX_SESSION", "")
	t.Setenv("REX_FAKE_SECOND", "1")

	if err := (Backend{Viewer: true}).Attach("~-workspaces-beta"); err != nil {
		t.Fatal(err)
	}
	got := selects(t, log)
	if len(got) != 1 || !strings.Contains(got[0], `{"session_id":"session:2"}`) {
		t.Errorf("selects = %q, want the app told to show session:2", got)
	}
	raw, _ := os.ReadFile(log)
	if strings.Contains(string(raw), "attach ") {
		t.Errorf("viewer fell through to a text-mode attach:\n%s", raw)
	}
}
