package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

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

// agentChild is one agent-bearing tmux window: what the radar dangles under a
// rig or session so the board reads as a HUD of every agent in flight. Window
// is the window's name (a repo, for a rig), Target is the session:index a jump
// lands on, and Context is the task the agent named for itself (empty when it's
// still on the "Claude Code" placeholder).
type agentChild struct {
	Session string
	Window  string
	Target  string
	Context string
	Working bool // window produced output within agentActiveWindow
}

// tmuxAgentChildren maps each session to its agent panes, in pane order. One
// list-panes -a sweeps the whole tree. Each agent pane becomes a child, except
// that panes sharing a window and the exact same context collapse to one — that
// kills the artifact of the same agent mirrored across a split without hiding
// two genuinely different agents side by side. A pane is an agent when its
// command is recognized or its title still wears Claude Code's state glyph (so
// a wrapped agent is still caught). Returns nil when tmux isn't
// running.
func tmuxAgentChildren() map[string][]agentChild {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{session_name}\t#{window_index}\t#{pane_index}\t#{window_name}\t#{pane_current_command}\t#{window_activity}\t#{pane_title}").Output()
	if err != nil {
		return nil
	}
	return parseAgentPanes(string(out), time.Now().Unix())
}

// parseAgentPanes is tmuxAgentChildren's pure core: it turns list-panes output
// (tab-separated session, window index, pane index, window name, command, window
// activity, title per line) into the per-session child list, applying the agent
// filter and the same-window-same-context dedup. window_activity is when the
// window last produced output — a stable working signal that idles off after a
// few quiet minutes, unlike the animating title glyph, which is why the glyph
// carries only the task text and never the state. now is the current unix time.
func parseAgentPanes(out string, now int64) map[string][]agentChild {
	activeWithin := int64(agentActiveWindow / time.Second)
	children := map[string][]agentChild{}
	seen := map[string]bool{} // session\twindow\tcontext — collapse exact dups
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 7)
		if len(f) != 7 {
			continue
		}
		session, windex, pindex, wname, cmd, activity, title := f[0], f[1], f[2], f[3], f[4], f[5], f[6]
		if !isAgentCommand(cmd) && stripAgentGlyph(title) == title {
			continue // not an agent pane: no known command or state glyph
		}
		ctx := stripAgentGlyph(title)
		if isAgentPlaceholder(ctx) {
			ctx = ""
		}
		key := session + "\t" + windex + "\t" + ctx
		if seen[key] {
			continue
		}
		seen[key] = true
		act, _ := strconv.ParseInt(activity, 10, 64)
		children[session] = append(children[session], agentChild{
			Session: session,
			Window:  wname,
			Target:  session + ":" + windex + "." + pindex,
			Context: ctx,
			Working: act > 0 && now-act < activeWithin,
		})
	}
	return children
}

func isAgentCommand(cmd string) bool {
	return cmd == "claude" || strings.HasPrefix(cmd, "codex") || cmd == "agy" || strings.HasPrefix(cmd, "antigravity")
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

// tmuxNewRigSession creates the first, task-level window for a rig and returns
// stable pane/window ids for the metadata and split operations that follow.
// Unlike bare sessions stood up by switch/wake, a rig session has an explicit
// window name so tmux never replaces its identity with "claude" or "recto".
func tmuxNewRigSession(name, windowName, cwd string) (string, string, error) {
	cmd := exec.Command("tmux", "new-session", "-d",
		"-s", name, "-n", windowName, "-c", cwd,
		"-P", "-F", "#{pane_id}\t#{window_id}",
	)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	fields := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(fields) != 2 {
		return "", "", fmt.Errorf("unexpected tmux new-session output %q", out)
	}
	return fields[0], fields[1], nil
}

func tmuxSplitHID(target, cwd, command string) (string, error) {
	cmd := exec.Command("tmux", "split-window", "-d", "-h", "-l", "50%",
		"-t", target, "-c", cwd, "-P", "-F", "#{pane_id}", command)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// tmuxNewCommandWindow creates a detached, explicitly named window whose only
// pane runs command. Rig uses it to park one persistent Recto per repository.
func tmuxNewCommandWindow(session, name, cwd, command string) (string, string, error) {
	cmd := exec.Command("tmux", "new-window", "-d",
		"-t", session, "-n", name, "-c", cwd,
		"-P", "-F", "#{pane_id}\t#{window_id}", command,
	)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	fields := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(fields) != 2 {
		return "", "", fmt.Errorf("unexpected tmux new-window output %q", out)
	}
	return fields[0], fields[1], nil
}

func tmuxSetPaneOption(pane, name, value string) error {
	cmd := exec.Command("tmux", "set-option", "-p", "-t", pane, name, value)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func tmuxSetWindowOption(window, name, value string) error {
	cmd := exec.Command("tmux", "set-option", "-w", "-t", window, name, value)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func tmuxRenameWindow(window, name string) error {
	cmd := exec.Command("tmux", "rename-window", "-t", window, name)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func tmuxJoinPane(src, dst string) error {
	cmd := exec.Command("tmux", "join-pane", "-d", "-f", "-h", "-l", "50%", "-s", src, "-t", dst)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func tmuxBreakPane(src, name string) (string, error) {
	cmd := exec.Command("tmux", "break-pane", "-d", "-s", src, "-n", name,
		"-P", "-F", "#{window_id}")
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
