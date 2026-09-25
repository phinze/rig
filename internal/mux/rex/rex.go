// Package rex is the mux.Backend for Rex, driven through its `rex` CLI the
// way the tmux backend drives `tmux`. Rex's model maps onto rig's directly: a
// session, windows, and blocks where tmux has panes, each addressed by the
// opaque id Rex mints for it. Rig's role/repo marks live in a sidecar file per
// session under rig's state dir. Every read goes through `rex api call` and
// parses JSON, never the human tables.
package rex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/phinze/rig/internal/mux"
)

// Backend drives one Rex server. With Server empty that's the one the
// environment points at (REX_SERVER when inside Rex, autodiscovery otherwise);
// with it set, every call goes to that endpoint, which is how a board on one
// host lists the sessions on another. Place names that other server on the
// board and in its Surface. MarksDir is where per-session mark sidecars live;
// empty disables marks, which a remote instance and a test both want, since a
// remote session's marks belong to the rig on that host.
type Backend struct {
	Server   string
	Place    string
	MarksDir string
}

var _ mux.Backend = Backend{}

func (b Backend) Name() string { return "rex" }

// Surface is the kind alone for the local server and rex@place for another.
func (b Backend) Surface() string {
	if b.Place == "" {
		return b.Name()
	}
	return b.Name() + "@" + b.Place
}

// remote reports whether this instance drives a server other than the one the
// environment points at.
func (b Backend) remote() bool { return b.Server != "" }

// binary is where Rex.app ships its CLI. PATH is consulted first so a
// standalone install or a test shim wins.
func binary() string {
	if p, err := exec.LookPath("rex"); err == nil {
		return p
	}
	return "/Applications/Rex.app/Contents/Helpers/rex"
}

// remoteTimeout bounds each call to another server. The board lists every
// surface on each refresh, and a host that has dropped off the network should
// cost that surface its rows, not stall the whole radar for the CLI's default
// ten seconds.
const remoteTimeout = "3s"

func (b Backend) command(args ...string) *exec.Cmd {
	if b.remote() {
		// Autostart only ever means a local server, which is never what a
		// remote instance is asking for.
		args = append([]string{"-S", b.Server, "--autostart=false", "--timeout", remoteTimeout}, args...)
	}
	return exec.Command(binary(), args...)
}

// remoteBackoff is how long a server that couldn't be reached is left alone.
// The radar lists every surface every two seconds, twice per scan (sessions,
// then panes), so without it a devbox that has dropped off the tailnet would
// cost every scan two full timeouts; with it, one timeout every half minute.
const remoteBackoff = 30 * time.Second

var breaker = &mux.Breaker{For: remoteBackoff}

// noteReachability records a remote call's outcome. Rex reports a failure to
// connect with a "connecting to" message whether the cause was a timeout or a
// name that didn't resolve; anything else means the server answered.
func noteReachability(server, stderr string) {
	breaker.Note(server, strings.HasPrefix(strings.TrimPrefix(stderr, "Error: "), "connecting to"))
}

// Unreachable reports whether this instance is a remote server that a recent
// call couldn't reach, so a board can say the surface is down rather than
// drawing it as a surface with no sessions.
func (b Backend) Unreachable() bool { return b.remote() && breaker.Down(b.Server) }

// call invokes one session-scoped API method and decodes its JSON reply into
// out (nil to discard). session is a label or id; "" means no session scope.
// A remote server that was just found unreachable fails fast rather than
// costing another timeout; teardown doesn't come through here, so it never
// skips a kill on that account.
func (b Backend) call(session, method string, params any, out any) error {
	if b.remote() && breaker.Down(b.Server) {
		return fmt.Errorf("rex %s: %s is unreachable (retrying within %s)", method, b.Server, remoteBackoff)
	}
	args := []string{"api", "call"}
	if session != "" {
		args = append(args, "-s", session)
	}
	args = append(args, method)
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		args = append(args, string(raw))
	}
	cmd := b.command(args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if b.remote() {
		noteReachability(b.Server, strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("rex %s: %w", method, err)
		}
		return fmt.Errorf("rex %s: %s", method, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// obj is the loosest JSON shape, for params where a typed struct would only
// restate the wire format.
type obj = map[string]any

func shellBlock(label, cwd string, cmdline []string) obj {
	options := obj{"cwd": cwd}
	if len(cmdline) > 0 {
		options["command"] = cmdline
	}
	return obj{"block": obj{
		"flavor":  "com.superlogical.terminal.shell",
		"label":   label,
		"options": options,
	}}
}

// commandLine is how a rig command string becomes a block's argv. Rig hands
// its backends shell-ish strings ("recto", "sleep infinity" in tests), so a
// shell parses them, the same thing tmux does with split-window's command
// argument. It's a login shell because the Rex server's own environment is
// whatever launchd gave the app, and rig's tools (jj, gh, recto) live on the
// PATH /etc/profile sets up.
func commandLine(cmdline string) []string {
	return []string{"/bin/sh", "-lc", cmdline}
}

type sessionEntry struct {
	ID    string `json:"session_id"`
	Label string `json:"label"`
}

func (b Backend) listSessions() ([]sessionEntry, error) {
	var out struct {
		Sessions []sessionEntry `json:"sessions"`
	}
	if err := b.call("", "session.list", nil, &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

func (b Backend) HasSession(name string) bool {
	sessions, err := b.listSessions()
	if err != nil {
		return false
	}
	for _, s := range sessions {
		if s.Label == name {
			return true
		}
	}
	return false
}

// Sessions lists every live session. Rex records neither a session's working
// directory nor when it was last attached, so Path comes from the active
// window's focused block (one process call per session) and LastAttached is
// 0, which lets rig's own touched timestamp decide the order.
func (b Backend) Sessions() []mux.Session {
	sessions, err := b.listSessions()
	if err != nil {
		return nil
	}
	var out []mux.Session
	for _, s := range sessions {
		out = append(out, mux.Session{Surface: b.Surface(), Name: s.Label, Path: b.sessionPath(s.Label)})
	}
	return out
}

func (b Backend) sessionPath(session string) string {
	v, err := b.view(session)
	if err != nil {
		return ""
	}
	for _, w := range v.Windows {
		if w.Active && w.FocusedBlock != "" {
			if p, err := b.process(session, w.FocusedBlock); err == nil && p.Foreground != nil {
				return p.Foreground.Cwd
			}
		}
	}
	return ""
}

// KillSession destroys the session and its marks sidecar together: a parked
// rig's session comes back with fresh block ids, and the old ones would
// otherwise sit in the file forever.
func (b Backend) KillSession(name string) error {
	if !b.HasSession(name) {
		return nil
	}
	cmd := b.command("kill", name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	b.forgetMarks(name)
	return nil
}

func (b Backend) forgetMarks(session string) {
	if b.MarksDir != "" {
		_ = os.Remove(b.marksPath(session))
	}
}

// Endpoint is the server this instance talks to: its own Server when it has
// one, else the one the environment names, in the unix:// form REX_SERVER
// uses, or "" for autodiscovery.
func (b Backend) Endpoint() string {
	if b.remote() {
		return b.Server
	}
	return os.Getenv("REX_SERVER")
}

// KillSessionAt kills on the recorded endpoint. A unix socket that no longer
// exists means that server is gone; anything else that fails is an error, so
// teardown keeps its job.
func (b Backend) KillSessionAt(name, endpoint string) error {
	if endpoint == "" {
		return b.KillSession(name)
	}
	if u, err := url.Parse(endpoint); err == nil && u.Scheme == "unix" {
		if _, err := os.Stat(u.Path); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("checking rex socket %s: %w", u.Path, err)
		}
	}
	at := Backend{Server: endpoint, MarksDir: b.MarksDir}
	raw, err := at.command("api", "call", "session.list").Output()
	if err != nil {
		return fmt.Errorf("listing sessions on %s: %w", endpoint, err)
	}
	var out struct {
		Sessions []sessionEntry `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("listing sessions on %s: %w", endpoint, err)
	}
	for _, s := range out.Sessions {
		if s.Label == name {
			cmd := at.command("kill", name)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				return err
			}
			b.forgetMarks(name)
			return nil
		}
	}
	return nil
}

// CurrentSession maps REX_SESSION, which is an id, back to the label rig
// addresses sessions by. The process runs on the host whose server set that
// variable, so a remote instance is never the one it's inside.
func (b Backend) CurrentSession() string {
	id := os.Getenv("REX_SESSION")
	if id == "" || b.remote() {
		return ""
	}
	sessions, err := b.listSessions()
	if err != nil {
		return ""
	}
	for _, s := range sessions {
		if s.ID == id {
			return s.Label
		}
	}
	return ""
}

func (b Backend) CurrentPane() string {
	if b.remote() {
		return ""
	}
	return os.Getenv("REX_BLOCK")
}

// Attach from a bare terminal is Rex's text-mode attach. From inside Rex the
// client can be moved within the session it's showing (a block target is
// focus_block, the session itself is already there); another session goes
// through hopToSession, and is ErrNoClientSwitch when that isn't available.
func (b Backend) Attach(target string) error {
	if os.Getenv("REX_SESSION") == "" {
		cmd := b.command("attach", target)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	// The picker hop matches a label, and labels collide across hosts (every
	// host slugs ~/workspaces/foo the same), so aiming it at another server's
	// session could land in this one's. Until a client can be moved by
	// session id, a remote row is a switch by hand.
	if b.remote() {
		return mux.ErrNoClientSwitch
	}
	current := b.CurrentSession()
	if target == current {
		return nil
	}
	if strings.HasPrefix(target, "block:") {
		if owner, err := b.sessionOfBlock(target); err == nil && owner == current {
			return b.SelectPane(target)
		}
	}
	// Only ever aim the picker at a session that exists: typing a name it
	// can't match makes a new session by that name, which would turn a
	// mistyped hop into a phantom rig.
	if b.HasSession(target) && hop(target) == nil {
		return nil
	}
	return mux.ErrNoClientSwitch
}

// hop is hopToSession, swappable so a test can assert the decision without
// driving the user's actual windows.
var hop = hopToSession

// hopToSession moves the GUI client to another session by driving the app's
// own session picker through AppleScript. It is a stopgap: no API moves a
// client between sessions, because focus is client-local state the server
// doesn't hold. When one lands this function goes away and Attach calls it.
//
// The work happens in a detached process after a delay, for a caller that is
// usually the radar running inside a floating layer: that layer closes when
// the radar exits, and keystrokes aimed at it before then land in the layer
// rather than the picker. The child gets its own process group so the closing
// layer's SIGHUP doesn't take it with it.
func hopToSession(label string) error {
	if runtime.GOOS != "darwin" {
		return errNoHop
	}
	script := []string{
		"-e", "on run argv",
		"-e", "delay 0.4",
		"-e", "set target to item 1 of argv",
		"-e", `tell application "Rex" to activate`,
		"-e", "delay 0.25",
		"-e", `tell application "System Events" to tell process "Rex"`,
		"-e", `click menu item "Change Session…" of menu 1 of menu bar item "View" of menu bar 1`,
		"-e", "delay 0.45",
		"-e", "keystroke target",
		"-e", "delay 0.45",
		"-e", "key code 36",
		"-e", "end tell",
		"-e", "end run",
	}
	cmd := exec.Command("osascript", append(script, label)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

var errNoHop = errors.New("no way to move this client between sessions")

// Popup opens a floating layer centred over the session's active window
// running the command, closing when it exits. The layer is sized in cells
// from the focused block's grid, aiming at a board's worth (popupCols ×
// popupRows) and never more than most of the window. Bounds go over as the
// client draws them today, and the command is told the box's visible size
// (COLUMNS, LINES) and colour (RIG_RADAR_BG) so it lays out inside the box
// and paints every cell of it.
func (b Backend) Popup(session, cmdline string) error {
	v, err := b.view(session)
	if err != nil {
		return err
	}
	cols, rows := 200.0, 60.0
	for _, win := range v.Windows {
		if !win.Active || win.FocusedBlock == "" {
			continue
		}
		var size struct {
			Columns float64 `json:"columns"`
			Rows    float64 `json:"rows"`
		}
		if err := b.call(session, "com.superlogical.terminal.size", obj{"block_id": win.FocusedBlock, "args": obj{}}, &size); err == nil && size.Columns > 0 && size.Rows > 0 {
			cols, rows = size.Columns, size.Rows
		}
	}
	w := min(popupCols/cols, 0.9)
	h := min(popupRows/rows, 0.9)
	x, y := (1-w)/2, (1-h)/2
	env := fmt.Sprintf("COLUMNS=%d LINES=%d RIG_RADAR_BG=%s ", int(w*cols), int(h*rows), popupBackground)
	block := shellBlock("rig radar", "", commandLine(env+cmdline))
	options := block["block"].(obj)["options"].(obj)
	delete(options, "cwd")
	options["exit"] = obj{"on_completion": true, "quick_exit_threshold_ms": 0}
	options["theme"] = obj{"background": popupBackground, "foreground": popupForeground}
	return b.call(session, "session.new_layer", obj{
		"bounds": obj{"x": x, "y": y, "w": x + w, "h": y + h},
		"focus":  true,
		"layout": block,
	}, nil)
}

const (
	popupCols = 120.0
	popupRows = 32.0
	// Catppuccin mocha's base and text, matching the terminal theme so the
	// popup reads as part of the window rather than a dialog.
	popupBackground = "#181825"
	popupForeground = "#cdd6f4"
)

// NewSession creates the session with its first window holding one shell
// block, and returns that block and window. The block runs the user's login
// shell, which is what tmux's new-session gives rig too; the agent command is
// typed into it afterwards by SendKeys.
func (b Backend) NewSession(name, windowName, cwd string) (string, string, error) {
	return b.createSession(name, windowName, shellBlock("shell", cwd, nil))
}

// NewCommandSession is NewSession with its one block running cmdline instead
// of a bare shell, for a session whose whole job is one long-lived command:
// rig's portal onto another host's tmux.
func (b Backend) NewCommandSession(name, windowName, cwd, cmdline string) (string, string, error) {
	return b.createSession(name, windowName, shellBlock(labelFor(cmdline), cwd, commandLine(cmdline)))
}

func (b Backend) createSession(name, windowName string, layout obj) (string, string, error) {
	var out struct {
		Windows []struct {
			WindowID string   `json:"window_id"`
			BlockIDs []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	params := obj{
		"label": name,
		"initial_windows": []obj{{
			"window_label": windowName,
			"layout":       layout,
		}},
	}
	if err := b.call("", "session.create", params, &out); err != nil {
		return "", "", err
	}
	if len(out.Windows) != 1 || len(out.Windows[0].BlockIDs) != 1 {
		return "", "", fmt.Errorf("unexpected rex session.create reply: %+v", out)
	}
	return out.Windows[0].BlockIDs[0], out.Windows[0].WindowID, nil
}

func (b Backend) split(target, cwd string, cmdline []string, label string) (string, error) {
	session, err := b.sessionOfBlock(target)
	if err != nil {
		return "", err
	}
	var out struct {
		BlockIDs []string `json:"block_ids"`
	}
	params := obj{
		"anchor_block_id": target,
		"direction":       "horizontal",
		"side":            "after",
		"ratio":           0.5,
		"focus":           false,
		"layout":          shellBlock(label, cwd, cmdline),
	}
	if err := b.call(session, "session.new_split", params, &out); err != nil {
		return "", err
	}
	if len(out.BlockIDs) != 1 {
		return "", fmt.Errorf("unexpected rex session.new_split reply: %+v", out)
	}
	return out.BlockIDs[0], nil
}

func (b Backend) SplitCommand(target, cwd, cmdline string) (string, error) {
	return b.split(target, cwd, commandLine(cmdline), labelFor(cmdline))
}

func (b Backend) SplitShell(target, cwd string) (string, error) {
	return b.split(target, cwd, nil, "shell")
}

func (b Backend) NewCommandWindow(session, name, cwd, cmdline string) (string, string, error) {
	var out struct {
		WindowID string   `json:"window_id"`
		BlockIDs []string `json:"block_ids"`
	}
	params := obj{
		"window_label": name,
		"focus":        false,
		"layout":       shellBlock(labelFor(cmdline), cwd, commandLine(cmdline)),
	}
	if err := b.call(session, "session.new_window", params, &out); err != nil {
		return "", "", err
	}
	if len(out.BlockIDs) != 1 {
		return "", "", fmt.Errorf("unexpected rex session.new_window reply: %+v", out)
	}
	return out.BlockIDs[0], out.WindowID, nil
}

// labelFor is the block label a command gets: its first word, the way tmux
// names a window after its command.
func labelFor(cmdline string) string {
	if f := strings.Fields(cmdline); len(f) > 0 {
		return filepath.Base(f[0])
	}
	return "shell"
}

// JoinPane moves src into a split beside dst. Rex's move_block takes a placed
// block as readily as a detached one, and a window emptied by the move closes
// itself, both as tmux's join-pane behaves.
func (b Backend) JoinPane(src, dst string) error {
	session, err := b.sessionOfBlock(dst)
	if err != nil {
		return err
	}
	return b.call(session, "session.move_block", obj{
		"block_id":        src,
		"anchor_block_id": dst,
		"direction":       "horizontal",
		"side":            "after",
		"ratio":           0.5,
	}, nil)
}

// BreakPane moves src into a new window of its own: make the window with a
// placeholder block, move src beside it, close the placeholder. Three calls,
// each cheap.
func (b Backend) BreakPane(src, name string) (string, error) {
	session, err := b.sessionOfBlock(src)
	if err != nil {
		return "", err
	}
	placeholder, window, err := b.NewCommandWindow(session, name, os.TempDir(), "sleep 60")
	if err != nil {
		return "", err
	}
	if err := b.JoinPane(src, placeholder); err != nil {
		return "", err
	}
	if err := b.call(session, "block.close", obj{"block_id": placeholder}, nil); err != nil {
		return "", err
	}
	return window, nil
}

func (b Backend) SelectPane(target string) error {
	session, err := b.sessionOfBlock(target)
	if err != nil {
		return err
	}
	return b.call(session, "session.focus_block", obj{"block_id": target}, nil)
}

// SendKeys types text into the block and presses Enter. `rex send` writes the
// bytes verbatim, so the newline is the Enter.
func (b Backend) SendKeys(target, text string) error {
	session, err := b.sessionOfBlock(target)
	if err != nil {
		return err
	}
	cmd := b.command("send", "-s", session, "-b", target, text+"\n")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func (b Backend) RenameWindow(window, name string) error {
	session, err := b.sessionOfWindow(window)
	if err != nil {
		return err
	}
	return b.call(session, "session.set_window_label", obj{"window_id": window, "label": name}, nil)
}

// Marks live in a JSON sidecar per session, keyed by block or window id. Ids
// are unique per server, so the file can be found from either kind of id
// without knowing the session.

type mark struct {
	Role string `json:"role"`
	Repo string `json:"repo"`
}

func (b Backend) marksPath(session string) string {
	return filepath.Join(b.MarksDir, url.PathEscape(session)+".json")
}

func (b Backend) readMarks(session string) map[string]mark {
	marks := map[string]mark{}
	if b.MarksDir == "" {
		return marks
	}
	raw, err := os.ReadFile(b.marksPath(session))
	if err != nil {
		return marks
	}
	_ = json.Unmarshal(raw, &marks)
	return marks
}

func (b Backend) writeMark(session, id string, m mark) error {
	if b.MarksDir == "" {
		return nil
	}
	marks := b.readMarks(session)
	marks[id] = m
	raw, err := json.MarshalIndent(marks, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(b.MarksDir, 0o755); err != nil {
		return err
	}
	path := b.marksPath(session)
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (b Backend) MarkPane(pane, role, repo string) error {
	session, err := b.sessionOfBlock(pane)
	if err != nil {
		return err
	}
	return b.writeMark(session, pane, mark{Role: role, Repo: repo})
}

func (b Backend) MarkWindow(window, role, repo string) error {
	session, err := b.sessionOfWindow(window)
	if err != nil {
		return err
	}
	return b.writeMark(session, window, mark{Role: role, Repo: repo})
}

// Views.

type viewBlock struct {
	BlockID string `json:"block_id"`
	Label   string `json:"label"`
}

type viewWindow struct {
	WindowID     string `json:"window_id"`
	Label        string `json:"label"`
	Active       bool   `json:"active"`
	FocusedBlock string `json:"focused_block_id"`
	Layers       []struct {
		Kind   string      `json:"kind"`
		Blocks []viewBlock `json:"blocks"`
	} `json:"layers"`
}

type sessionView struct {
	SessionID string       `json:"session_id"`
	Label     string       `json:"label"`
	Windows   []viewWindow `json:"windows"`
}

func (b Backend) view(session string) (sessionView, error) {
	var v sessionView
	err := b.call(session, "session.view", nil, &v)
	return v, err
}

type processInfo struct {
	Foreground *foregroundProcess `json:"foreground"`
}

type foregroundProcess struct {
	Name  string `json:"name"`
	Argv0 string `json:"argv0"`
	Cwd   string `json:"cwd"`
}

// commandName is what the pane is running, in the spelling a person would
// use. Prefer argv0's base: a wrapped binary reports its real name (nix
// builds `claude` as a wrapper around `.claude-unwrapped`), while argv0 keeps
// the path it was invoked by.
func commandName(fg *foregroundProcess) string {
	if fg.Argv0 != "" {
		if base := filepath.Base(fg.Argv0); base != "" && base != "." && base != string(filepath.Separator) {
			return base
		}
	}
	return fg.Name
}

func (b Backend) process(session, block string) (processInfo, error) {
	var p processInfo
	err := b.call(session, "com.superlogical.terminal.process", obj{"block_id": block, "args": obj{}}, &p)
	return p, err
}

func (b Backend) title(session, block string) string {
	var t struct {
		Title string `json:"title"`
	}
	if err := b.call(session, "com.superlogical.terminal.title", obj{"block_id": block, "args": obj{}}, &t); err != nil {
		return ""
	}
	return t.Title
}

// sessionOfBlock finds which session owns a block id. Rig's callers address
// blocks by id alone, as they did tmux panes, so the lookup is a list_blocks
// per session; cheap, and only on layout changes.
func (b Backend) sessionOfBlock(block string) (string, error) {
	sessions, err := b.listSessions()
	if err != nil {
		return "", err
	}
	for _, s := range sessions {
		var out struct {
			Blocks []struct {
				BlockID string `json:"block_id"`
			} `json:"blocks"`
		}
		if err := b.call(s.Label, "session.list_blocks", nil, &out); err != nil {
			continue
		}
		for _, bl := range out.Blocks {
			if bl.BlockID == block {
				return s.Label, nil
			}
		}
	}
	return "", fmt.Errorf("rex block %s not found in any session", block)
}

func (b Backend) sessionOfWindow(window string) (string, error) {
	sessions, err := b.listSessions()
	if err != nil {
		return "", err
	}
	for _, s := range sessions {
		v, err := b.view(s.Label)
		if err != nil {
			continue
		}
		for _, w := range v.Windows {
			if w.WindowID == window {
				return s.Label, nil
			}
		}
	}
	return "", fmt.Errorf("rex window %s not found in any session", window)
}

// Panes lists a session's tiled blocks in window then layout order, with the
// sidecar marks and a process/title probe per block. Floating layers (the
// radar popup) are skipped: they're not part of the carousel.
func (b Backend) Panes(session string) ([]mux.Pane, error) {
	v, err := b.view(session)
	if err != nil {
		return nil, err
	}
	marks := b.readMarks(session)
	var panes []mux.Pane
	for wi, w := range v.Windows {
		wm := marks[w.WindowID]
		for _, layer := range w.Layers {
			if layer.Kind != "tiled" {
				continue
			}
			for pi, bl := range layer.Blocks {
				p := mux.Pane{
					Surface:    b.Surface(),
					Session:    session,
					PaneID:     bl.BlockID,
					PaneIdx:    fmt.Sprint(pi),
					WindowID:   w.WindowID,
					WindowIdx:  fmt.Sprint(wi),
					WindowName: w.Label,
					Target:     bl.BlockID,
					WindowRole: wm.Role,
					WindowRepo: wm.Repo,
				}
				if m, ok := marks[bl.BlockID]; ok {
					p.Role, p.Repo = m.Role, m.Repo
				}
				if info, err := b.process(session, bl.BlockID); err == nil && info.Foreground != nil {
					p.Command = commandName(info.Foreground)
					p.Path = info.Foreground.Cwd
				}
				p.Title = b.title(session, bl.BlockID)
				panes = append(panes, p)
			}
		}
	}
	return panes, nil
}

// AllPanes is Panes over every session. Nil when the server isn't reachable,
// since the radar draws an empty board rather than an error then.
func (b Backend) AllPanes() ([]mux.Pane, error) {
	sessions, err := b.listSessions()
	if err != nil {
		return nil, nil
	}
	var out []mux.Pane
	for _, s := range sessions {
		panes, err := b.Panes(s.Label)
		if err != nil {
			continue
		}
		out = append(out, panes...)
	}
	return out, nil
}

// Installed reports whether the CLI can be run at all. The backend is
// selectable on any machine but only useful where Rex.app is.
func Installed() bool {
	_, err := os.Stat(binary())
	return err == nil
}
