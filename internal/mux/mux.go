// Package mux is the terminal multiplexer rig drives: the thing that holds a
// session per rig, a window per repo, and an agent | reviewer split, and that
// radar switches between.
//
// The seam sits at rig's own vocabulary rather than tmux's verbs. A caller says
// "mark this pane as the agent for this repo" and never "set a user option",
// and the backend decides how that survives: a tmux `@rig-*` option, or a
// sidecar of its own. What this package deliberately does
// not know is what an agent is. It hands back every pane with its command and
// title, and rig decides which of those are agents and what they're doing.
//
// Sessions are addressed by the name SessionName derives from a rig's basedir,
// which every backend spells identically; panes and windows are addressed by
// whatever opaque id the backend handed back when it made them.
package mux

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoClientSwitch is what Attach returns when the caller is inside the
// multiplexer but the backend has no way to move the user's client to another
// session. Callers that would otherwise switch (radar's Enter, `rig switch`)
// match on this to say so rather than surface a raw failure.
var ErrNoClientSwitch = errors.New("this multiplexer cannot switch the client to another session")

// Backend is one multiplexer. Every method that creates something returns the
// stable ids later calls address; every method that lists something returns
// nil, not an error, when the server simply isn't running, because a board
// with no sessions is a normal thing to draw.
type Backend interface {
	// Name is the backend's short name, the one `rig config backend` stores.
	Name() string

	// Sessions.
	HasSession(name string) bool
	// Sessions lists every live session, rig or not, with the fields the radar
	// and switch need to sort by recency.
	Sessions() []Session
	KillSession(name string) error
	// Endpoint names the server the current process is talking to, in whatever
	// form KillSessionAt accepts back. Teardown records it before crossing into
	// a systemd service whose environment may not carry it.
	Endpoint() string
	// KillSessionAt kills a session on the recorded server. An endpoint that no
	// longer exists means the whole server is gone and is not an error; a live
	// endpoint that can't be queried is, so teardown retains its job instead
	// of mistaking a connection failure for an absent session.
	KillSessionAt(name, endpoint string) error
	// CurrentSession is the session the current process runs inside, or "".
	CurrentSession() string
	// CurrentPane is the pane the current process runs inside, in the same id
	// space Panes reports, or "". Resume compares it against the agent pane to
	// know whether it's being run from the seat it's about to fill.
	CurrentPane() string
	// Attach puts the user's client on a session, or on a Pane's Target: a
	// switch when already inside the multiplexer, an attach from a bare
	// terminal. A backend that can't switch returns ErrNoClientSwitch.
	Attach(target string) error
	// Popup runs a shell command line in a floating popup over the session's
	// active window, and returns once the popup is open. The popup closes when
	// the command exits. It's how the radar is shown.
	Popup(session, cmdline string) error

	// Layout.
	NewSession(name, windowName, cwd string) (pane, window string, err error)
	SplitCommand(target, cwd, command string) (pane string, err error)
	SplitShell(target, cwd string) (pane string, err error)
	NewCommandWindow(session, name, cwd, command string) (pane, window string, err error)
	JoinPane(src, dst string) error
	BreakPane(src, name string) (window string, err error)
	SelectPane(target string) error
	// SendKeys types text into the target pane and presses Enter.
	SendKeys(target, text string) error
	RenameWindow(window, name string) error

	// Metadata. Role and repo are rig's labels and opaque here; Panes reads
	// them back alongside what the backend knows about each pane.
	MarkPane(pane, role, repo string) error
	MarkWindow(window, role, repo string) error
	// Panes lists one session's panes in window then pane order.
	Panes(session string) ([]Pane, error)
	// AllPanes lists every pane of every session, in the same order.
	AllPanes() ([]Pane, error)
}

// Session is one live session as the radar's universal picker sees it: its
// name (what you attach to), the working directory the backend reports for it,
// and when it was last attached (0 if never), so non-rig sessions can sort MRU
// alongside the rigs.
type Session struct {
	// Backend is the Name of the backend that listed this session, so a list
	// merged across backends stays attributable and Attach knows where to go.
	Backend      string
	Name         string
	Path         string
	LastAttached int64
}

// Pane is one pane with rig's marks read back alongside what the backend knows.
type Pane struct {
	// Backend is the Name of the backend that listed this pane; see Session.
	Backend    string
	Session    string
	PaneID     string
	PaneIdx    string
	WindowID   string
	WindowIdx  string
	WindowName string
	// Target is what Attach and SelectPane accept to land on this exact pane.
	Target string

	// Role and Repo are the marks MarkPane stored; WindowRole and WindowRepo
	// are MarkWindow's. Empty when never marked.
	Role       string
	Repo       string
	WindowRole string
	WindowRepo string

	// Command is the foreground process's name, Path its working directory,
	// Title the terminal title it set. Activity is the unix time the window
	// last produced output, 0 when the backend can't say.
	Command  string
	Path     string
	Title    string
	Activity int64
}

// SessionName converts an absolute path into the session name session-wizard
// computes in full-path mode: $HOME shown as ~, then the characters tmux can't
// keep in a session name (plus spaces) replaced with dashes, lowercased.
// Matching that convention means a `t` jump into a rig directory lands in the
// rig's existing session instead of spawning a duplicate, and rig sessions
// sort alongside everything else in the list. Every backend uses it, so a
// rig's session is the same string whichever multiplexer hosts it.
func SessionName(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		// macOS commonly exposes /var through the /private/var real path. Cwd
		// and HOME can land on opposite spellings of the same directory, which
		// would otherwise make a rig's create and teardown names disagree.
		path = resolvePath(path)
		home = resolvePath(home)
		if rel, ok := strings.CutPrefix(path, home); ok {
			path = "~" + rel
		}
	}
	repl := strings.NewReplacer(" ", "-", ".", "-", ":", "-")
	return strings.ToLower(repl.Replace(path))
}

func resolvePath(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return filepath.Clean(dir)
}
