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
