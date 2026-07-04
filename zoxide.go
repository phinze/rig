package main

import (
	"os/exec"
	"strings"
)

// zoxideDirs returns the directories zoxide knows about, most-frecent first —
// the same frecency-ranked list session-wizard's fzf front-end appended so you
// could stand up a session in a dir you'd visited but didn't currently have
// open. Degrades to nil when zoxide isn't installed or has no history, so the
// NEW picker just shows nothing rather than erroring.
func zoxideDirs() []string {
	out, err := exec.Command("zoxide", "query", "-l").Output()
	if err != nil {
		return nil
	}
	var dirs []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			dirs = append(dirs, line)
		}
	}
	return dirs
}

// zoxideAdd bumps a directory's frecency rank, the way session-wizard did when
// you picked a dir. Best-effort: a missing zoxide or a failed add never blocks
// standing the session up.
func zoxideAdd(dir string) {
	_ = exec.Command("zoxide", "add", dir).Run()
}
