package main

import (
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"
)

func tmuxSessionName(rigID string) string {
	return "rig-" + rigID
}

func tmuxHasSession(name string) bool {
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
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
// working directory.
func tmuxNewWindow(session, name, cwd string) error {
	cmd := exec.Command("tmux", "new-window", "-t", session, "-n", name, "-c", cwd)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
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
