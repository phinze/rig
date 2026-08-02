package main

import (
	"os/exec"
	"strings"
)

// zoxideDirs returns the directories zoxide knows about, most-frecent first —
// the same frecency-ranked list used to order repo choices. Degrades to nil
// when zoxide isn't installed or has no history.
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
