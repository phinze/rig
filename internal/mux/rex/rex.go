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
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/phinze/rig/internal/mux"
)

// Backend drives the Rex server the environment points at (REX_SERVER when
// inside Rex, autodiscovery otherwise). MarksDir is where per-session mark
// sidecars live; empty disables marks, which only a test wants.
type Backend struct {
	MarksDir string
}

var _ mux.Backend = Backend{}

func (Backend) Name() string { return "rex" }

// binary is where Rex.app ships its CLI. PATH is consulted first so a
// standalone install or a test shim wins.
func binary() string {
	if p, err := exec.LookPath("rex"); err == nil {
		return p
	}
	return "/Applications/Rex.app/Contents/Helpers/rex"
}

func command(args ...string) *exec.Cmd {
	return exec.Command(binary(), args...)
}

// call invokes one session-scoped API method and decodes its JSON reply into
// out (nil to discard). session is a label or id; "" means no session scope.
func call(session, method string, params any, out any) error {
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
	cmd := command(args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
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
// its backends shell-ish strings ("recto", "sleep infinity" in tests), so the
// user's shell parses them, the same thing tmux does with split-window's
// command argument.
func commandLine(cmdline string) []string {
	return []string{"/bin/sh", "-c", cmdline}
}

type sessionEntry struct {
	ID    string `json:"session_id"`
	Label string `json:"label"`
}

func listSessions() ([]sessionEntry, error) {
	var out struct {
		Sessions []sessionEntry `json:"sessions"`
	}
	if err := call("", "session.list", nil, &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

func (Backend) HasSession(name string) bool {
	sessions, err := listSessions()
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
	sessions, err := listSessions()
	if err != nil {
		return nil
	}
	var out []mux.Session
	for _, s := range sessions {
		out = append(out, mux.Session{Backend: "rex", Name: s.Label, Path: b.sessionPath(s.Label)})
	}
	return out
}

func (Backend) sessionPath(session string) string {
	v, err := view(session)
	if err != nil {
		return ""
	}
	for _, w := range v.Windows {
		if w.Active && w.FocusedBlock != "" {
			if p, err := process(session, w.FocusedBlock); err == nil && p.Foreground != nil {
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
	cmd := command("kill", name)
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

// Endpoint is the server the current process talks to, in the unix:// form
// REX_SERVER uses, or "" for autodiscovery.
func (Backend) Endpoint() string {
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
	raw, err := command("-S", endpoint, "api", "call", "session.list").Output()
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
			cmd := command("-S", endpoint, "kill", name)
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
// addresses sessions by.
func (Backend) CurrentSession() string {
	id := os.Getenv("REX_SESSION")
	if id == "" {
		return ""
	}
	sessions, err := listSessions()
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

func (Backend) CurrentPane() string {
	return os.Getenv("REX_BLOCK")
}

// Attach from a bare terminal is Rex's text-mode attach. From inside Rex it
// is ErrNoClientSwitch.
func (Backend) Attach(target string) error {
	if os.Getenv("REX_SESSION") != "" {
		return mux.ErrNoClientSwitch
	}
	cmd := command("attach", target)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// NewSession creates the session with its first window holding one shell
// block, and returns that block and window. The block runs the user's login
// shell, which is what tmux's new-session gives rig too; the agent command is
// typed into it afterwards by SendKeys.
func (Backend) NewSession(name, windowName, cwd string) (string, string, error) {
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
			"layout":       shellBlock("shell", cwd, nil),
		}},
	}
	if err := call("", "session.create", params, &out); err != nil {
		return "", "", err
	}
	if len(out.Windows) != 1 || len(out.Windows[0].BlockIDs) != 1 {
		return "", "", fmt.Errorf("unexpected rex session.create reply: %+v", out)
	}
	return out.Windows[0].BlockIDs[0], out.Windows[0].WindowID, nil
}

func (b Backend) split(target, cwd string, cmdline []string, label string) (string, error) {
	session, err := sessionOfBlock(target)
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
	if err := call(session, "session.new_split", params, &out); err != nil {
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

func (Backend) NewCommandWindow(session, name, cwd, cmdline string) (string, string, error) {
	var out struct {
		WindowID string   `json:"window_id"`
		BlockIDs []string `json:"block_ids"`
	}
	params := obj{
		"window_label": name,
		"focus":        false,
		"layout":       shellBlock(labelFor(cmdline), cwd, commandLine(cmdline)),
	}
	if err := call(session, "session.new_window", params, &out); err != nil {
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
func (Backend) JoinPane(src, dst string) error {
	session, err := sessionOfBlock(dst)
	if err != nil {
		return err
	}
	return call(session, "session.move_block", obj{
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
	session, err := sessionOfBlock(src)
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
	if err := call(session, "block.close", obj{"block_id": placeholder}, nil); err != nil {
		return "", err
	}
	return window, nil
}

func (Backend) SelectPane(target string) error {
	session, err := sessionOfBlock(target)
	if err != nil {
		return err
	}
	return call(session, "session.focus_block", obj{"block_id": target}, nil)
}

// SendKeys types text into the block and presses Enter. `rex send` writes the
// bytes verbatim, so the newline is the Enter.
func (Backend) SendKeys(target, text string) error {
	session, err := sessionOfBlock(target)
	if err != nil {
		return err
	}
	cmd := command("send", "-s", session, "-b", target, text+"\n")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func (Backend) RenameWindow(window, name string) error {
	session, err := sessionOfWindow(window)
	if err != nil {
		return err
	}
	return call(session, "session.set_window_label", obj{"window_id": window, "label": name}, nil)
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
	session, err := sessionOfBlock(pane)
	if err != nil {
		return err
	}
	return b.writeMark(session, pane, mark{Role: role, Repo: repo})
}

func (b Backend) MarkWindow(window, role, repo string) error {
	session, err := sessionOfWindow(window)
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

func view(session string) (sessionView, error) {
	var v sessionView
	err := call(session, "session.view", nil, &v)
	return v, err
}

type processInfo struct {
	Foreground *struct {
		Name string `json:"name"`
		Cwd  string `json:"cwd"`
	} `json:"foreground"`
}

func process(session, block string) (processInfo, error) {
	var p processInfo
	err := call(session, "com.superlogical.terminal.process", obj{"block_id": block, "args": obj{}}, &p)
	return p, err
}

func title(session, block string) string {
	var t struct {
		Title string `json:"title"`
	}
	if err := call(session, "com.superlogical.terminal.title", obj{"block_id": block, "args": obj{}}, &t); err != nil {
		return ""
	}
	return t.Title
}

// sessionOfBlock finds which session owns a block id. Rig's callers address
// blocks by id alone, as they did tmux panes, so the lookup is a list_blocks
// per session; cheap, and only on layout changes.
func sessionOfBlock(block string) (string, error) {
	sessions, err := listSessions()
	if err != nil {
		return "", err
	}
	for _, s := range sessions {
		var out struct {
			Blocks []struct {
				BlockID string `json:"block_id"`
			} `json:"blocks"`
		}
		if err := call(s.Label, "session.list_blocks", nil, &out); err != nil {
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

func sessionOfWindow(window string) (string, error) {
	sessions, err := listSessions()
	if err != nil {
		return "", err
	}
	for _, s := range sessions {
		v, err := view(s.Label)
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
	v, err := view(session)
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
					Backend:    "rex",
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
				if info, err := process(session, bl.BlockID); err == nil && info.Foreground != nil {
					p.Command = info.Foreground.Name
					p.Path = info.Foreground.Cwd
				}
				p.Title = title(session, bl.BlockID)
				panes = append(panes, p)
			}
		}
	}
	return panes, nil
}

// AllPanes is Panes over every session. Nil when the server isn't reachable,
// since the radar draws an empty board rather than an error then.
func (b Backend) AllPanes() ([]mux.Pane, error) {
	sessions, err := listSessions()
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
