package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// fzfSelect pipes tab-delimited rows into fzf and returns the chosen row.
// Only the first three columns are shown (with-nth=1,2,3); callers can stash
// extra data in later columns. Returns "" if the user cancels (fzf exit 130).
func fzfSelect(rows []string, prompt string) (string, error) {
	if len(rows) == 0 {
		return "", fmt.Errorf("nothing to pick from")
	}
	// fzf needs a controlling terminal to draw its UI and read keys. Run from a
	// pipe, CI, or an agent's shell there's no tty and fzf dies with a cryptic
	// "inappropriate ioctl for device" / exit 2. Catch that up front and say
	// something the caller can act on instead.
	if !stdinIsTTY() {
		return "", noTTYError(prompt)
	}
	cmd := exec.Command("fzf",
		"--height=40%", "--reverse",
		"--with-nth=1,2,3", "--delimiter=\t",
		"--prompt="+prompt,
	)
	cmd.Stdin = strings.NewReader(strings.Join(rows, "\n") + "\n")
	cmd.Stderr = os.Stderr // fzf draws its UI on the controlling tty
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 130 {
			return "", nil // user pressed Esc / Ctrl-C
		}
		return "", fmt.Errorf("fzf: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// noTTYError explains why an interactive picker can't run here and points at
// the escape hatch every picker-backed command has: pass an explicit argument
// to skip the picker entirely. The prompt ("Pick issue: ", "cd to rig: ",
// "Review PR: ") names which picker was trying to open.
func noTTYError(prompt string) error {
	what := strings.TrimRight(prompt, ": ")
	return fmt.Errorf(
		"%s picker needs an interactive terminal, but stdin isn't a tty.\n"+
			"Run rig from a terminal, or pass an explicit argument to skip the picker "+
			"(an exact id like MIR-75, a search query, or a PR url).",
		what,
	)
}
