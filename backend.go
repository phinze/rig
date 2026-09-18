package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/phinze/rig/internal/mux"
	"github.com/phinze/rig/internal/mux/rex"
	"github.com/phinze/rig/internal/mux/tmux"
	"golang.org/x/term"
)

// preferredBackend is the multiplexer a *new* rig is built on, picked once in
// main from the preference ladder. It decides nothing about existing rigs:
// each records the backend that hosts it, and sessionFor reads that back. The
// commands that list across the whole machine (radar, switch, ls) ask every
// known backend rather than this one, because a machine hosting two
// multiplexers has rigs on each and the board has to show both.
var preferredBackend mux.Backend = tmux.Backend{}

// knownBackends is every backend this binary can drive, whether or not its
// server is running, in the order `rig config` and a typo's suggestion print
// them. tmux is first because it's the default an empty name resolves to. A
// backend whose server is down lists nothing, which is the cheap and correct
// answer for a board. It's computed per call rather than at init so the Rex
// marks dir follows XDG_STATE_HOME, which tests pin after init.
func knownBackends() []mux.Backend {
	list := []mux.Backend{tmux.Backend{}, rex.Backend{MarksDir: rexMarksDir()}}
	return append(list, extraBackends...)
}

// extraBackends is where a test registers a fake, since one real multiplexer
// can't exercise the routing between two.
var extraBackends []mux.Backend

// rexMarksDir is where the Rex backend keeps its per-session mark sidecars:
// derived state, so under the state dir beside everything else rig writes
// down.
func rexMarksDir() string {
	dir, err := rigStateDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "rex-marks")
}

func backendNames() []string {
	backends := knownBackends()
	names := make([]string, 0, len(backends))
	for _, b := range backends {
		names = append(names, b.Name())
	}
	return names
}

// backendByName resolves a preference to a backend. Unknown names are an
// error rather than a fallback, because a stale or misspelled preference that
// silently landed you in tmux would be indistinguishable from the setting
// never having taken.
func backendByName(name string) (mux.Backend, error) {
	if name == "" {
		return tmux.Backend{}, nil
	}
	for _, b := range knownBackends() {
		if b.Name() == name {
			return b, nil
		}
	}
	return nil, fmt.Errorf("unknown backend %q (expected one of %s)", name, strings.Join(backendNames(), ", "))
}

// backendNamed is backendByName for a name rig itself recorded (a manifest, a
// tombstone, a teardown job, a listed session) rather than one the user typed.
// A name this binary no longer knows falls back to tmux, since a rig with no
// multiplexer at all is worse than one looked for in the wrong place: that's
// the downgraded-binary case, not the typo case, which the config command
// refuses at write time.
func backendNamed(name string) mux.Backend {
	b, err := backendByName(name)
	if err != nil {
		return tmux.Backend{}
	}
	return b
}

// defaultBackend applies the same ladder defaultAgent does, narrowest scope
// first: RIG_BACKEND for this shell, `rig config backend` for this user, then
// tmux. The env var sits above the file for the reason the agent one does —
// `RIG_BACKEND=rex rig new` has to mean something. Like the agent, it decides
// only for a new rig; the manifest records the answer and sessionFor reads it
// back.
func defaultBackend() (mux.Backend, error) {
	name, _, _ := defaultBackendWithSource()
	return backendByName(name)
}

// defaultBackendWithSource is defaultBackend plus where the answer came from,
// for `rig config` to name. The source labels are the agent's, since the
// ladder is the same and the table prints them the same way.
func defaultBackendWithSource() (name string, src agentSource, raw string) {
	if raw := os.Getenv("RIG_BACKEND"); raw != "" {
		return raw, agentFromEnv, raw
	}
	if raw := readRigConfig().Backend; raw != "" {
		return raw, agentFromConfig, raw
	}
	return "tmux", agentFromBuiltin, ""
}

// rigSession is one rig's session together with the multiplexer hosting it.
// The two travel as a pair because a session name alone is ambiguous on a
// machine with two multiplexers, and every command that used to hold the bare
// string now holds this.
type rigSession struct {
	name string
	b    mux.Backend
}

// sessionFor is the session a rig's basedir maps to, on the backend its
// manifest records. Empty means tmux, as it does for every rig made before the
// field existed.
func sessionFor(basedir string, m manifest) rigSession {
	return rigSession{name: rigSessionName(basedir), b: backendNamed(m.Backend)}
}

func (rs rigSession) live() bool { return rs.b.HasSession(rs.name) }

func (rs rigSession) attach() error { return attachOrReport(rs.b, rs.name) }

func (rs rigSession) panes() ([]mux.Pane, error) { return rs.b.Panes(rs.name) }

// rigSessionName is the session a rig's basedir maps to. It's mux.SessionName
// under a name that says what rig uses it for.
func rigSessionName(path string) string {
	return mux.SessionName(path)
}

// currentSession is the session the current process runs inside and the
// backend it belongs to, or nil and "" from a bare terminal. Each backend
// answers from its own environment, so at most one says anything.
func currentSession() (mux.Backend, string) {
	for _, b := range knownBackends() {
		if name := b.CurrentSession(); name != "" {
			return b, name
		}
	}
	return nil, ""
}

// insideSession reports whether the current process is running inside the
// given rig session. False when not inside any multiplexer.
func insideSession(rs rigSession) bool {
	b, name := currentSession()
	return b != nil && b.Name() == rs.b.Name() && name == rs.name
}

// allSessions lists every live session across every known backend, each
// tagged with the backend that listed it.
func allSessions() []mux.Session {
	var out []mux.Session
	for _, b := range knownBackends() {
		out = append(out, b.Sessions()...)
	}
	return out
}

// lastAttached maps each live session name to the unix time it was last
// attached (0 if never). It's how `rig switch` sorts most-recently-touched
// first, the same signal session-wizard's `t` sorts on.
func lastAttached() map[string]int64 {
	sessions := allSessions()
	m := make(map[string]int64, len(sessions))
	for _, s := range sessions {
		m[s.Name] = s.LastAttached
	}
	return m
}

// attachOrReport attaches to the target on the given backend when stdin is a
// tty, otherwise prints how to attach manually (e.g. when invoked from a
// script or test). A backend that can't switch the client says so and
// succeeds: the session is ready, and the last step is yours.
func attachOrReport(b mux.Backend, target string) error {
	if !stdinIsTTY() {
		fmt.Fprintf(os.Stderr, "rig: not a tty — session ready as %q, attach manually\n", target)
		return nil
	}
	if err := b.Attach(target); err != nil {
		if errors.Is(err, mux.ErrNoClientSwitch) {
			fmt.Fprintf(os.Stderr, "rig: session %q is ready, but %v — switch to it by hand\n", target, err)
			return nil
		}
		return err
	}
	return nil
}

// agentChild is one agent-bearing window: what the radar dangles under a rig
// or session so the board reads as a HUD of every agent in flight. Window is
// the window's name (a repo, for a rig), Target is what a jump hands to
// Attach, and Context is the task the agent named for itself (empty when it's
// still on the "Claude Code" placeholder).
type agentChild struct {
	Backend string
	Session string
	Window  string
	Target  string
	Context string
	Working bool // window produced output within agentActiveWindow
}

// liveAgentChildren is the radar's one sweep of every pane on every known
// backend, filtered down to the agents. A backend that can't be listed
// contributes nothing rather than failing the sweep.
func liveAgentChildren() map[string][]agentChild {
	var panes []mux.Pane
	for _, b := range knownBackends() {
		if ps, err := b.AllPanes(); err == nil {
			panes = append(panes, ps...)
		}
	}
	return agentChildren(panes, time.Now().Unix())
}

// agentChildren maps each session to its agent panes, in pane order. Each agent
// pane becomes a child, except that panes sharing a window and the exact same
// context collapse to one — that kills the artifact of the same agent mirrored
// across a split without hiding two genuinely different agents side by side. A
// pane is an agent when its command is recognized or its title still wears
// Claude Code's state glyph (so a wrapped agent is still caught). Some agents
// set the title to cwd's basename instead of naming the task; that repeats the
// repo and is treated like a placeholder so a rig can fall back to its durable
// title. Activity is when the window last produced output — a stable working
// signal that idles off after a few quiet minutes, unlike the animating title
// glyph, which is why the glyph carries only the task text and never the
// state. now is the current unix time.
func agentChildren(panes []mux.Pane, now int64) map[string][]agentChild {
	activeWithin := int64(agentActiveWindow / time.Second)
	children := map[string][]agentChild{}
	seen := map[string]bool{} // session\twindow\tcontext — collapse exact dups
	for _, p := range panes {
		if !isAgentCommand(p.Command) && stripAgentGlyph(p.Title) == p.Title {
			continue // not an agent pane: no known command or state glyph
		}
		ctx := stripAgentGlyph(p.Title)
		if isAgentPlaceholder(ctx) || (p.Path != "" && ctx == filepath.Base(p.Path)) {
			ctx = ""
		}
		key := p.Session + "\t" + p.WindowIdx + "\t" + ctx
		if seen[key] {
			continue
		}
		seen[key] = true
		children[p.Session] = append(children[p.Session], agentChild{
			Backend: p.Backend,
			Session: p.Session,
			Window:  p.WindowName,
			Target:  p.Target,
			Context: ctx,
			Working: p.Activity > 0 && now-p.Activity < activeWithin,
		})
	}
	return children
}

// isAgentCommand recognises an agent by its foreground process name. Nix
// wraps binaries as `.<name>-wrapped` around `.<name>-unwrapped`, and Rex
// reports the real process where tmux reports the wrapper, so both spellings
// are folded back to the name.
func isAgentCommand(cmd string) bool {
	cmd = strings.TrimPrefix(cmd, ".")
	cmd = strings.TrimSuffix(strings.TrimSuffix(cmd, "-unwrapped"), "-wrapped")
	return cmd == "claude" || strings.HasPrefix(cmd, "codex") || cmd == "agy" || strings.HasPrefix(cmd, "antigravity")
}

// shellQuote wraps s for safe inclusion as a single shell argument when
// typed via SendKeys (where the receiving shell will reparse the line).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// stdinIsTTY reports whether stdin is connected to a real terminal.
// Note: a char-device check alone returns true for /dev/null, so use the
// proper termios probe via x/term.
func stdinIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
