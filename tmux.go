package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// tmuxSessionName converts an absolute path into the session name
// session-wizard computes in full-path mode: $HOME shown as ~, then the
// characters tmux can't keep in a session name (plus spaces) replaced with
// dashes, lowercased. Matching that convention means a `t` jump into a rig
// directory lands in the rig's existing session instead of spawning a
// duplicate, and rig sessions sort alongside everything else in the list.
func tmuxSessionName(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, ok := strings.CutPrefix(path, home); ok {
			path = "~" + rel
		}
	}
	repl := strings.NewReplacer(" ", "-", ".", "-", ":", "-")
	return strings.ToLower(repl.Replace(path))
}

func tmuxHasSession(name string) bool {
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

// tmuxLastAttached maps each live tmux session name to the unix time it was
// last attached (0 if never). It's how `rig switch` sorts most-recently-touched
// first, the same session_last_attached signal session-wizard's `t` sorts on.
// Returns an empty map when tmux isn't running.
func tmuxLastAttached() map[string]int64 {
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_last_attached} #{session_name}").Output()
	if err != nil {
		return map[string]int64{}
	}
	m := make(map[string]int64)
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		ts, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		secs, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			continue
		}
		m[name] = secs
	}
	return m
}

// tmuxSession is one live tmux session as the radar's universal picker sees it:
// its name (what you attach to), the working directory tmux reports for it, and
// when it was last attached (0 if never), so non-rig sessions can sort MRU
// alongside the rigs.
type tmuxSession struct {
	Name         string
	Path         string
	LastAttached int64
}

// tmuxSessions lists every live session with the fields the radar needs. It's
// the one-call superset of tmuxLastAttached: the radar builds both its
// attached-map and its bare-session rows from a single list-sessions. Returns
// nil when tmux isn't running. A tab delimiter keeps paths with spaces intact
// (session names are dash-normalized, so they never carry a tab).
func tmuxSessions() []tmuxSession {
	out, err := exec.Command("tmux", "list-sessions", "-F",
		"#{session_last_attached}\t#{session_path}\t#{session_name}").Output()
	if err != nil {
		return nil
	}
	var sessions []tmuxSession
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		secs, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		sessions = append(sessions, tmuxSession{
			Name:         fields[2],
			Path:         fields[1],
			LastAttached: secs,
		})
	}
	return sessions
}

// agentChild is one claude-bearing tmux window: what the radar dangles under a
// rig or session so the board reads as a HUD of every agent in flight. Window
// is the window's name (a repo, for a rig), Target is the session:index a jump
// lands on, and Context is the task the agent named for itself (empty when it's
// still on the "Claude Code" placeholder).
type agentChild struct {
	Session string
	Window  string
	Target  string
	Context string
}

// tmuxAgentChildren maps each session to its claude windows, in window order.
// One list-panes -a sweeps the whole tree; a window counts once (its first
// claude pane) even when it holds several. A pane is the agent when its command
// is claude or its title still wears Claude Code's state glyph, so a claude
// running under a wrapper process is still caught by the title. Returns nil when
// tmux isn't running.
func tmuxAgentChildren() map[string][]agentChild {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{session_name}\t#{window_index}\t#{window_name}\t#{pane_current_command}\t#{pane_title}").Output()
	if err != nil {
		return nil
	}
	children := map[string][]agentChild{}
	seen := map[string]bool{} // session\twindow — one child per window
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 5)
		if len(f) != 5 {
			continue
		}
		session, windex, wname, cmd, title := f[0], f[1], f[2], f[3], f[4]
		if cmd != "claude" && stripAgentGlyph(title) == title {
			continue // not an agent pane: no claude command, no state glyph
		}
		key := session + "\t" + windex
		if seen[key] {
			continue
		}
		seen[key] = true
		ctx := stripAgentGlyph(title)
		if ctx == agentPlaceholder {
			ctx = "" // "Claude Code" default: agent open, no task named
		}
		children[session] = append(children[session], agentChild{
			Session: session,
			Window:  wname,
			Target:  session + ":" + windex,
			Context: ctx,
		})
	}
	return children
}

// currentTmuxSession returns the name of the session the current process is
// running inside, or "" if not inside tmux. `rig switch` drops this from the
// list — you never switch to where you already are.
func currentTmuxSession() string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	out, err := exec.Command("tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func tmuxNewSession(name, cwd string) error {
	cmd := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", cwd)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// tmuxSplitH splits the given target horizontally, running command in the
// new (right) pane with cwd as its working directory.
func tmuxSplitH(target, cwd, command string) error {
	cmd := exec.Command("tmux", "split-window", "-h", "-t", target, "-c", cwd, command)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// tmuxNewWindow opens a new window named name in the session, with cwd as its
// working directory, and returns the new window's id (e.g. "@5"). The window is
// created detached (-d) so it doesn't steal focus: rig add is normally run from
// a main session, and the new repo's window should wait in the background until
// the caller chooses to visit it. The returned id is a stable target for a
// follow-up split, even after other windows come and go.
func tmuxNewWindow(session, name, cwd string) (string, error) {
	cmd := exec.Command("tmux", "new-window", "-d",
		"-t", session, "-n", name, "-c", cwd,
		"-P", "-F", "#{window_id}",
	)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func tmuxSelectPane(target string) error {
	cmd := exec.Command("tmux", "select-pane", "-t", target)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// tmuxSendKeys types text into the target pane, then presses Enter.
func tmuxSendKeys(target, text string) error {
	cmd := exec.Command("tmux", "send-keys", "-t", target, text, "Enter")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// shellQuote wraps s for safe inclusion as a single shell argument when
// typed via send-keys (where the receiving shell will reparse the line).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func tmuxKillSession(name string) error {
	if !tmuxHasSession(name) {
		return nil
	}
	cmd := exec.Command("tmux", "kill-session", "-t", name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// insideTmuxSession reports whether the current process is running inside
// the named tmux session. Returns false if not inside tmux at all.
func insideTmuxSession(name string) bool {
	if os.Getenv("TMUX") == "" {
		return false
	}
	out, err := exec.Command("tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == name
}

// tmuxAttach switches to the session if already inside tmux, otherwise attaches.
func tmuxAttach(name string) error {
	bin := "attach"
	if os.Getenv("TMUX") != "" {
		bin = "switch-client"
	}
	cmd := exec.Command("tmux", bin, "-t", name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// stdinIsTTY reports whether stdin is connected to a real terminal.
// Note: a char-device check alone returns true for /dev/null, so use the
// proper termios probe via x/term.
func stdinIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
