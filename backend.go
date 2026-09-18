package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/phinze/rig/internal/mux"
	"github.com/phinze/rig/internal/mux/tmux"
	"golang.org/x/term"
)

// backend is the multiplexer every command drives. It's a package global
// rather than a parameter because rig's commands are package-level functions
// that already reached for tmux by name; main picks it once from the
// preference ladder. See package mux for the seam itself.
var backend mux.Backend = tmux.Backend{}

// backendNames lists the backends rig knows, in the order `rig config` and a
// typo's suggestion print them.
var backendNames = []string{"tmux"}

// backendByName resolves a preference to a backend. Unknown names are an
// error rather than a fallback, because a stale or misspelled preference that
// silently landed you in tmux would be indistinguishable from the setting
// never having taken.
func backendByName(name string) (mux.Backend, error) {
	switch name {
	case "", "tmux":
		return tmux.Backend{}, nil
	}
	return nil, fmt.Errorf("unknown backend %q (expected one of %s)", name, strings.Join(backendNames, ", "))
}

// defaultBackend applies the same ladder defaultAgent does, narrowest scope
// first: RIG_BACKEND for this shell, `rig config backend` for this user, then
// tmux. The env var sits above the file for the reason the agent one does —
// `RIG_BACKEND=rex rig new` has to mean something. Like the agent, it decides
// only for a new rig; the manifest records the answer and rigBackend reads it
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

// rigBackend is the multiplexer a particular rig's session lives in, read
// from its manifest rather than from the preference: a preference is about
// the next rig, and changing it must not make every existing rig's session
// unfindable. Empty means tmux, as it does for every rig made before the
// field existed. Only the per-rig lifecycle reads this today (teardown records
// it); the commands that take a manifest still drive the global, and moving
// them across is the first job of a second backend, when it's testable.
func rigBackend(m manifest) mux.Backend {
	b, err := backendByName(m.Backend)
	if err != nil {
		return tmux.Backend{}
	}
	return b
}

// rigSessionName is the session a rig's basedir maps to. It's mux.SessionName
// under a name that says what rig uses it for.
func rigSessionName(path string) string {
	return mux.SessionName(path)
}

// insideSession reports whether the current process is running inside the
// named session. False when not inside the multiplexer at all.
func insideSession(name string) bool {
	return name != "" && backend.CurrentSession() == name
}

// lastAttached maps each live session name to the unix time it was last
// attached (0 if never). It's how `rig switch` sorts most-recently-touched
// first, the same signal session-wizard's `t` sorts on.
func lastAttached() map[string]int64 {
	sessions := backend.Sessions()
	m := make(map[string]int64, len(sessions))
	for _, s := range sessions {
		m[s.Name] = s.LastAttached
	}
	return m
}

// agentChild is one agent-bearing window: what the radar dangles under a rig
// or session so the board reads as a HUD of every agent in flight. Window is
// the window's name (a repo, for a rig), Target is what a jump hands to
// Attach, and Context is the task the agent named for itself (empty when it's
// still on the "Claude Code" placeholder).
type agentChild struct {
	Session string
	Window  string
	Target  string
	Context string
	Working bool // window produced output within agentActiveWindow
}

// liveAgentChildren is the radar's one sweep of every pane on the server,
// filtered down to the agents. Nil when the server isn't running.
func liveAgentChildren() map[string][]agentChild {
	panes, err := backend.AllPanes()
	if err != nil {
		return nil
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
			Session: p.Session,
			Window:  p.WindowName,
			Target:  p.Target,
			Context: ctx,
			Working: p.Activity > 0 && now-p.Activity < activeWithin,
		})
	}
	return children
}

func isAgentCommand(cmd string) bool {
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
