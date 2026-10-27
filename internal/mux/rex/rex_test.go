package rex

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
*"do -")
  # A snapshot script. Without a canned answer it fails the way an older
  # server would, so Snapshot's CLI fallback is what the other tests see.
  printf 'env REX_SESSION=%s\n' "$REX_SESSION" >> "` + log + `"
  if [ -n "$REX_FAKE_SCRIPT" ]; then cat "$REX_FAKE_SCRIPT"; exit 0; fi
  echo 'Error: unknown script' >&2; exit 1 ;;
*" session.create "*)
  # Matched here because its params carry the block flavor, which has dots.
  echo '{"session_id":"session:1","initial_windows":[{"window_id":"window:m","block_ids":["block:a"],"revision":1}]}'
  exit 0 ;;
*"client ls --json"*)
  # A relaunched app leaves its old client listed as lost, as preview.12 does.
  echo '{"clients":[{"client_id":"client:gone","info":{"kind":"app"},"connection_state":"lost"},{"client_id":"client:app","info":{"kind":"app"},"connection_state":"connected"},{"client_id":"client:cli","info":{"kind":"cli"},"connection_state":"connected"}]}'
  exit 0 ;;
*" do session.select "*)
  if [ -n "$REX_FAKE_UNKNOWN_HOST" ]; then
    echo 'Error: The operation couldn'"'"'t be completed. (Rex.Workspace.(unknown context at $1057621e8).SessionNotFoundOnServerError error 1.)' >&2; exit 1
  fi
  if [ -n "$REX_FAKE_NO_REMOTE_CONTROL" ]; then
    echo 'Error: Rex.app is not accepting actions. Turn on Remote Control in its Settings under Rex Server.' >&2; exit 1
  fi
  echo null; exit 0 ;;
esac
method=""
for a in "$@"; do case "$a" in *.*) method="$a";; esac; done
case "$method" in
session.list)
  if [ -n "$REX_FAKE_RENAMED" ]; then
    # What an agent leaves behind after Rex's hook tells it to name its session.
    echo '{"sessions":[{"session_id":"session:1","label":"alpha-work"}]}'
  elif [ -n "$REX_FAKE_SECOND" ]; then
    echo '{"sessions":[{"session_id":"session:1","label":"~-workspaces-alpha"},{"session_id":"session:2","label":"~-workspaces-beta"}]}'
  else
    echo '{"sessions":[{"session_id":"session:1","label":"~-workspaces-alpha"}]}'
  fi ;;
session.view)
  if [ -n "$REX_FAKE_SPLIT" ]; then cat <<'EOF'
{"session_id":"session:1","label":"~-workspaces-alpha","active_window_id":"window:m","windows":[
 {"window_id":"window:m","label":"main/alpha","focused_block_id":"block:a","layers":[
   {"kind":"tiled","blocks":[{"block_id":"block:a","label":"shell"},{"block_id":"block:r","label":"recto"}],
    "layout":{"rect":[0,0,1,1],"split":{"direction":"horizontal","ratio":0.5,"before":{"rect":[0,0,0.5,1],"block_id":"block:a"},"after":{"rect":[0.5,0,0.5,1],"block_id":"block:r"}}}}]}]}
EOF
  exit 0; fi
  # A radar open over the session leaves its floating layer focused.
  focus=cat; [ -n "$REX_FAKE_POPUP_FOCUSED" ] && focus="sed s/block:a\",\"layers/block:popup\",\"layers/"
  $focus <<'EOF'
{"session_id":"session:1","label":"~-workspaces-alpha","active_window_id":"window:m","windows":[
 {"window_id":"window:m","label":"main/alpha","focused_block_id":"block:a","layers":[
   {"kind":"tiled","blocks":[{"block_id":"block:a","label":"shell"},{"block_id":"block:r","label":"recto"}]},
   {"kind":"floating","blocks":[{"block_id":"block:popup","label":"rig radar"}]}]},
 {"window_id":"window:z","label":"zeta","focused_block_id":"block:z","layers":[
   {"kind":"tiled","blocks":[{"block_id":"block:z","label":"recto"}]}]}]}
EOF
;;
session.list_blocks) echo '{"blocks":[{"block_id":"block:a"},{"block_id":"block:r"},{"block_id":"block:z"}]}' ;;
com.superlogical.terminal.process)
  case "$*" in
    *block:r*) if [ -n "$REX_FAKE_R_EXITED" ]; then echo '{"last_exit":{"exit_code":255,"error":"exit status 255"}}'; exit 0; fi
      echo '{"foreground":{"name":"recto","cwd":"/w/x"}}' ;;
    *block:a*) echo '{"child":{"pid":4242,"name":"login"},"foreground":{"pid":4300,"name":".claude-unwrapped","argv0":"/etc/profiles/per-user/phinze/bin/claude","cwd":"/w/alpha"}}' ;;
    *block:z*) if [ -n "$REX_FAKE_Z_PORTAL" ]; then echo '{"foreground":{"name":"tmux","cwd":"/home"}}'; exit 0; fi
      echo '{"foreground":{"name":"recto","cwd":"/w/x"}}' ;;
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
	b.forget("~-workspaces-alpha")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("sidecar survived forget: %v", err)
	}
	// Marks disabled: writes are no-ops rather than errors.
	if err := (Backend{}).MarkPane("block:a", "agent", "alpha"); err != nil {
		t.Errorf("markless backend errored: %v", err)
	}
}

// A session rig made stays rig's under the name it was made with after
// something renames it, which Rex's agent hook has every agent do on its first
// turn: still found, still listed by that name, still probed and killed by the
// id Rex minted for it. Without the record it's found only by its label.
func TestRenamedSessionStaysFound(t *testing.T) {
	log := fakeRex(t)
	b := Backend{MarksDir: filepath.Join(t.TempDir(), "marks")}
	const name = "~-workspaces-alpha"
	if _, _, err := b.NewSession(name, "main/alpha", "/w/alpha"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_FAKE_RENAMED", "1")

	if !b.HasSession(name) {
		t.Fatal("renamed session not found by the name rig made it with")
	}
	if got := b.Sessions(); len(got) != 1 || got[0].Name != name {
		t.Errorf("Sessions = %+v, want it listed as %q", got, name)
	}
	if id, err := b.SessionID(name); err != nil || id != "session:1" {
		t.Errorf("SessionID = %q, %v", id, err)
	}
	panes, err := b.Panes(name)
	if err != nil || len(panes) == 0 || panes[0].Session != name {
		t.Fatalf("Panes = %+v, %v", panes, err)
	}
	if session, err := b.sessionOfBlock("block:a"); err != nil || session != name {
		t.Errorf("sessionOfBlock = %q, %v", session, err)
	}
	t.Setenv("REX_SESSION", "session:1")
	if got := b.CurrentSession(); got != name {
		t.Errorf("CurrentSession = %q, want %q", got, name)
	}
	if b.HasSession("alpha-work") {
		t.Error("the new label answered for a session rig knows by another name")
	}

	if err := b.KillSession(name); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(log)
	calls := string(raw)
	if !strings.Contains(calls, "-s session:1 session.view") {
		t.Errorf("view didn't address the session by id:\n%s", calls)
	}
	if !strings.Contains(calls, "kill session:1") {
		t.Errorf("kill didn't address the session by id:\n%s", calls)
	}
	if _, err := os.Stat(b.idPath(name)); !os.IsNotExist(err) {
		t.Errorf("id record survived the kill: %v", err)
	}

	unrecorded := Backend{MarksDir: t.TempDir()}
	if unrecorded.HasSession(name) || !unrecorded.HasSession("alpha-work") {
		t.Error("a session with no record should answer to its current label only")
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

// A portal rig took for gone can still be running: its far-side tty stamp
// went stale while the ssh lived on. The respawn replaces it rather than
// leaving a second tab beside the new one, and still leaves the shell alone.
func TestRespawnCommandSessionReplacesALivePortal(t *testing.T) {
	log := fakeRex(t)
	t.Setenv("REX_FAKE_Z_PORTAL", "1")
	t.Setenv("REX_FAKE_R_EXITED", "1")
	if err := (Backend{}).RespawnCommandSession("~-workspaces-alpha", "local", "/home", "tmux attach -t foo"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(log)
	calls := string(raw)
	for _, want := range []string{"block:r", "block:z"} {
		if !strings.Contains(calls, `block.close {"block_id":"`+want+`"}`) {
			t.Errorf("calls:\n%s\nwant %s closed", calls, want)
		}
	}
	if strings.Contains(calls, `block.close {"block_id":"block:a"}`) {
		t.Errorf("closed the live shell block:a:\n%s", calls)
	}
}

// Popup opens one layer in the session running the command through a login
// shell, sized from the window's grid (120 columns of 300 and 32 rows of 100,
// centred) and told that size and its colour.
func TestPopupOpensASizedLayer(t *testing.T) {
	layer := popupLayer(t)
	for _, want := range []string{`"x":0.3`, `"w":0.7`, `"focus":true`, `"-lc"`, `COLUMNS=120 LINES=32 RIG_RADAR_BG=#181825 /usr/local/bin/rig radar`, `"on_completion":true`} {
		if !strings.Contains(layer, want) {
			t.Errorf("new_layer call lacks %s:\n%s", want, layer)
		}
	}
}

// In a split, the focused block's 300 columns are half a 600-column window,
// and bounds are fractions of the window. So 120 columns is a fifth of it,
// centred, not the 0.4 of the block's width that drew the board off to one
// side of an oversized box.
func TestPopupSizesFromTheWindowInASplit(t *testing.T) {
	t.Setenv("REX_FAKE_SPLIT", "1")
	layer := popupLayer(t)
	for _, want := range []string{`"x":0.4`, `"w":0.6`, `COLUMNS=120 LINES=32 `} {
		if !strings.Contains(layer, want) {
			t.Errorf("new_layer call lacks %s:\n%s", want, layer)
		}
	}
}

// FindPane describes the one tiled block it's asked for, probing only that
// block, and knows neither a floating layer's block nor a made-up id.
func TestFindPaneProbesOneBlock(t *testing.T) {
	log := fakeRex(t)
	p, ok := (Backend{}).FindPane("block:a")
	if !ok || p.Session != "~-workspaces-alpha" || p.Command != "claude" || p.Path != "/w/alpha" || p.WindowID != "window:m" {
		t.Fatalf("FindPane(block:a) = %+v, %v", p, ok)
	}
	raw, _ := os.ReadFile(log)
	if n := strings.Count(string(raw), "com.superlogical.terminal.process"); n != 1 {
		t.Errorf("probed %d blocks, want just the one:\n%s", n, raw)
	}
	for _, target := range []string{"block:popup", "block:nope", "~-workspaces-alpha"} {
		if _, ok := (Backend{}).FindPane(target); ok {
			t.Errorf("FindPane(%q) found something", target)
		}
	}
}

func popupLayer(t *testing.T) string {
	t.Helper()
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
	return layer
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

// A client handed over by the action that started rig is the one told to
// switch, without asking the server which apps are connected.
func TestAttachSelectsTheClientThatAsked(t *testing.T) {
	log := fakeRex(t)
	t.Setenv("REX_SESSION", "session:1")
	t.Setenv("REX_FAKE_SECOND", "1")
	t.Setenv("RIG_REX_CLIENT", "client:phone")

	if err := (Backend{}).Attach("~-workspaces-beta"); err != nil {
		t.Fatal(err)
	}
	got := selects(t, log)
	if len(got) != 1 || !strings.HasPrefix(got[0], "-C client:phone do session.select") {
		t.Errorf("selects = %q, want client:phone told to show session:2", got)
	}
	if raw, _ := os.ReadFile(log); strings.Contains(string(raw), "client ls") {
		t.Errorf("listed clients despite being handed one:\n%s", raw)
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

// Entering a remote row resolves it on its own server and selects it through
// this host's: session.select is a client action a server only relays, and
// the local relay is the one that reaches the app on this screen.
func TestRemoteAttachSelectsThroughTheLocalServer(t *testing.T) {
	log := fakeRex(t)
	t.Setenv("REX_SESSION", "session:1")
	t.Setenv("REX_FAKE_SECOND", "1")

	b := Backend{Server: "https://devbox.example.ts.net", Place: "devbox"}
	if err := b.Attach("~-workspaces-beta"); err != nil {
		t.Fatalf("remote attach = %v", err)
	}
	raw, _ := os.ReadFile(log)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		remote := strings.HasPrefix(line, "-S https://devbox.example.ts.net ")
		switch {
		case strings.Contains(line, "session.list") && !remote:
			t.Errorf("resolved the label on the local server: %q", line)
		case (strings.Contains(line, "client ls") || strings.Contains(line, "session.select")) && remote:
			t.Errorf("selected through the remote server: %q", line)
		}
	}
	got := selects(t, log)
	if len(got) != 1 || !strings.HasPrefix(got[0], `-C client:app do session.select --args {"session_id":"session:2"}`) {
		t.Errorf("selects = %q, want the local app told to show session:2", got)
	}
}

// A host the app hasn't added comes back as a Swift type name; the radar
// should say how to fix it instead.
func TestAttachExplainsAnUnknownHost(t *testing.T) {
	fakeRex(t)
	t.Setenv("REX_SESSION", "session:1")
	t.Setenv("REX_FAKE_SECOND", "1")
	t.Setenv("REX_FAKE_UNKNOWN_HOST", "1")

	err := (Backend{Server: "https://devbox.example.ts.net", Place: "devbox"}).Attach("~-workspaces-beta")
	if !errors.Is(err, mux.ErrNoClientSwitch) || !strings.Contains(err.Error(), "Remote Hosts") {
		t.Errorf("attach = %v, want ErrNoClientSwitch naming Remote Hosts", err)
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

// Outside Rex (a tmux pane, rig serve under launchd) Attach still tells the
// app to show the target. It never falls back to a text-mode `rex attach`.
func TestAttachSelectsFromOutsideRex(t *testing.T) {
	log := fakeRex(t)
	t.Setenv("REX_SESSION", "")
	t.Setenv("REX_FAKE_SECOND", "1")

	if err := (Backend{}).Attach("~-workspaces-beta"); err != nil {
		t.Fatal(err)
	}
	got := selects(t, log)
	if len(got) != 1 || !strings.Contains(got[0], `{"session_id":"session:2"}`) {
		t.Errorf("selects = %q, want the app told to show session:2", got)
	}
	raw, _ := os.ReadFile(log)
	if strings.Contains(string(raw), "attach ") {
		t.Errorf("fell through to a text-mode attach:\n%s", raw)
	}
}

// With nothing on PATH, REX_BIN_DIR is where a Rex terminal says the CLI is.
func TestBinaryFollowsRexBinDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rex"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("REX_BIN_DIR", dir)
	if got, want := binary(), filepath.Join(dir, "rex"); got != want {
		t.Errorf("binary() = %q, want %q", got, want)
	}
}

// A block's root pid is the process it started, which messaging walks down
// from to find the agent. A remote server's pids aren't this machine's, so it
// refuses rather than hand back a number that names a stranger.
func TestPaneRootPID(t *testing.T) {
	fakeRex(t)
	pid, err := Backend{}.PaneRootPID("block:a")
	if err != nil || pid != 4242 {
		t.Errorf("PaneRootPID = %d, %v; want 4242", pid, err)
	}
	if _, err := (Backend{}).PaneRootPID("block:r"); err == nil {
		t.Error("a block with no child pid answered one")
	}
	if _, err := (Backend{Server: "https://devbox"}).PaneRootPID("block:a"); err == nil {
		t.Error("a remote server's pid was taken as this machine's")
	}
}

// Lookup finds the CLI in whatever the Rex Server helper is called this
// release: build 1203 renamed it from `Rex Server.app` to `Rex Server.rex`,
// and rig serve, started by launchd, has nothing else to find it by.
func TestLookupFindsRenamedHelper(t *testing.T) {
	for _, helper := range []string{"Rex Server.app", "Rex Server.rex"} {
		t.Run(helper, func(t *testing.T) {
			bundle := t.TempDir()
			bin := filepath.Join(bundle, "Contents", "Helpers", helper, "Contents", "MacOS", "rex")
			if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			old := appBundle
			appBundle = bundle
			t.Cleanup(func() { appBundle = old })
			t.Setenv("PATH", t.TempDir())
			t.Setenv("REX_BIN_DIR", "")

			got, looked, ok := Lookup()
			if !ok || got != bin {
				t.Fatalf("Lookup = %q, %v (looked %v), want %q", got, ok, looked, bin)
			}
		})
	}
}

// Snapshot's CLI fallback is Sessions and AllPanes in one pass: the same
// answer, with one view per session rather than two, and the focused block
// probed only as a pane unless it's somewhere Panes doesn't look.
func TestSnapshotMatchesSessionsAndAllPanes(t *testing.T) {
	log := fakeRex(t)
	t.Setenv("REX_FAKE_SECOND", "1")
	b := Backend{MarksDir: t.TempDir()}
	for _, popup := range []bool{false, true} {
		if popup {
			t.Setenv("REX_FAKE_POPUP_FOCUSED", "1")
		}
		wantSessions, wantPanes := b.Sessions(), mustPanes(t, b)
		if err := os.WriteFile(log, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		sessions, panes := b.Snapshot()
		if !reflect.DeepEqual(sessions, wantSessions) {
			t.Errorf("popup=%v: sessions = %+v, want %+v", popup, sessions, wantSessions)
		}
		if !reflect.DeepEqual(panes, wantPanes) {
			t.Errorf("popup=%v: panes = %+v, want %+v", popup, panes, wantPanes)
		}
		raw, _ := os.ReadFile(log)
		calls := string(raw)
		wantProcess := 6 // three tiled blocks in each of two sessions
		if popup {
			wantProcess += 2 // and the popup each session has focused
		}
		if got := strings.Count(calls, "session.view"); got != 2 {
			t.Errorf("popup=%v: %d views, want one per session", popup, got)
		}
		if got := strings.Count(calls, "terminal.process"); got != wantProcess {
			t.Errorf("popup=%v: %d process probes, want %d", popup, got, wantProcess)
		}
	}
	if sessions, _ := b.Snapshot(); sessions[0].Path != "/w/x" {
		t.Errorf("popup-focused session path = %q, want the popup's own cwd as Sessions reports it", sessions[0].Path)
	}
}

func mustPanes(t *testing.T, b Backend) []mux.Pane {
	t.Helper()
	panes, err := b.AllPanes()
	if err != nil {
		t.Fatal(err)
	}
	return panes
}

// SessionNames is the one session.list a batch of liveness checks needs.
func TestSessionNamesIsOneList(t *testing.T) {
	log := fakeRex(t)
	t.Setenv("REX_FAKE_SECOND", "1")
	names, ok := Backend{MarksDir: t.TempDir()}.SessionNames()
	if !ok || !reflect.DeepEqual(names, []string{"~-workspaces-alpha", "~-workspaces-beta"}) {
		t.Errorf("SessionNames = %v, %v", names, ok)
	}
	raw, _ := os.ReadFile(log)
	if got := strings.Count(string(raw), "\n"); got != 1 {
		t.Errorf("%d calls, want one session.list:\n%s", got, raw)
	}
}

// What snapshot.lua returns for the fake's two sessions, as Lua encodes it:
// nulls dropped, and the popup layer's empty block list come back as {}.
const fakeScriptAnswer = `{"sessions":[` + fakeScriptSession + `,` + fakeScriptSession2 + `]}`

const fakeScriptView = `{"session_id":"session:1","label":"~-workspaces-alpha","active_window_id":"window:m","windows":[
 {"window_id":"window:m","label":"main/alpha","focused_block_id":"block:a","layers":[
   {"kind":"tiled","blocks":[{"block_id":"block:a","label":"shell"},{"block_id":"block:r","label":"recto"}]},
   {"kind":"floating","blocks":{}}]},
 {"window_id":"window:z","label":"zeta","focused_block_id":"block:z","layers":[
   {"kind":"tiled","blocks":[{"block_id":"block:z","label":"recto"}]}]}]}`

const fakeScriptProbes = `{
 "block:a":{"process":{"child":{"pid":4242,"name":"login"},"foreground":{"pid":4300,"name":".claude-unwrapped","argv0":"/etc/profiles/per-user/phinze/bin/claude","cwd":"/w/alpha"}},"title":{"title":"✳ Task A"}},
 "block:r":{"process":{"foreground":{"name":"recto","cwd":"/w/x"}},"title":{"title":"✳ Task A"}},
 "block:z":{"process":{"foreground":{"name":"recto","cwd":"/w/x"}},"title":{"title":"✳ Task A"}}}`

const fakeScriptSession = `{"session_id":"session:1","label":"~-workspaces-alpha","view":` + fakeScriptView + `,"probes":` + fakeScriptProbes + `}`
const fakeScriptSession2 = `{"session_id":"session:2","label":"~-workspaces-beta","view":` + fakeScriptView + `,"probes":` + fakeScriptProbes + `}`

// With a server that runs scripts, Snapshot is one `rex do` and gives the
// same answer the CLI path does, without the caller's session in its
// environment.
func TestSnapshotRunsOneScript(t *testing.T) {
	log := fakeRex(t)
	t.Setenv("REX_FAKE_SECOND", "1")
	t.Setenv("REX_SESSION", "session:caller")
	b := Backend{MarksDir: t.TempDir()}
	if err := b.MarkPane("block:a", "agent", "alpha"); err != nil {
		t.Fatal(err)
	}
	wantSessions, wantPanes := b.callSnapshot()

	answer := filepath.Join(t.TempDir(), "answer.json")
	if err := os.WriteFile(answer, []byte(fakeScriptAnswer), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_FAKE_SCRIPT", answer)
	if err := os.WriteFile(log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, panes := b.Snapshot()
	if !reflect.DeepEqual(sessions, wantSessions) {
		t.Errorf("sessions = %+v, want %+v", sessions, wantSessions)
	}
	if !reflect.DeepEqual(panes, wantPanes) {
		t.Errorf("panes = %+v, want %+v", panes, wantPanes)
	}
	raw, _ := os.ReadFile(log)
	if want := "do -\nenv REX_SESSION=\n"; string(raw) != want {
		t.Errorf("calls = %q, want one script run with REX_SESSION cleared", raw)
	}
}

// A session the script couldn't attach is still listed, with no panes.
func TestSnapshotScriptKeepsAnUnattachedSession(t *testing.T) {
	fakeRex(t)
	answer := filepath.Join(t.TempDir(), "answer.json")
	if err := os.WriteFile(answer, []byte(`{"sessions":[{"session_id":"session:9","label":"gone","probes":{}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_FAKE_SCRIPT", answer)
	sessions, panes := Backend{MarksDir: t.TempDir()}.Snapshot()
	if len(sessions) != 1 || sessions[0].Name != "gone" || len(panes) != 0 {
		t.Errorf("snapshot = %+v, %+v; want the session alone", sessions, panes)
	}
}

// A block's program status rides on its pane: the root record is the
// program's own, and a child record (an id like build/test) doesn't speak
// for it. A block whose server had no program_status keeps a zero Status.
func TestSnapshotScriptReadsProgramStatus(t *testing.T) {
	fakeRex(t)
	probes := `{
 "block:a":{"process":{"foreground":{"name":".claude-unwrapped","cwd":"/w/alpha"}},"title":{"title":"✳ Task A"},
   "status":{"records":[{"id":"sub","state":"working","msg":"a subagent"},{"state":"blocked","kind":"permission","msg":"Run the tests","updated_at":"2026-10-26T10:00:00-05:00"}]}},
 "block:r":{"process":{"foreground":{"name":"recto","cwd":"/w/x"}},"title":{"title":"recto"}},
 "block:z":{"process":{"foreground":{"name":"recto","cwd":"/w/x"}},"title":{"title":"recto"}}}`
	answer := filepath.Join(t.TempDir(), "answer.json")
	doc := `{"sessions":[{"session_id":"session:1","label":"~-workspaces-alpha","view":` + fakeScriptView + `,"probes":` + probes + `}]}`
	if err := os.WriteFile(answer, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_FAKE_SCRIPT", answer)
	_, panes := Backend{MarksDir: t.TempDir()}.Snapshot()
	got := map[string]mux.ProgramStatus{}
	for _, p := range panes {
		got[p.PaneID] = p.Status
	}
	want := mux.ProgramStatus{State: "blocked", Kind: "permission", Msg: "Run the tests", Updated: 1793026800}
	if got["block:a"] != want {
		t.Errorf("block:a status = %+v, want %+v", got["block:a"], want)
	}
	if got["block:r"] != (mux.ProgramStatus{}) {
		t.Errorf("block:r status = %+v, want none", got["block:r"])
	}
}
