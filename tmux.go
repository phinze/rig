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
