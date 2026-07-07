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

// fzfLiveSelect runs fzf as a two-layer search front-end. fzf keeps its own
// fuzzy matching on, so typing instantly reranks and filters the rows already
// loaded — fast and stable, nothing shifts under you. Layered under that, Tab
// re-runs reloadCmd (for the issue picker, a fresh Linear search) with the
// current query, widening the candidate pool beyond the initial fetch. Keeping
// the refetch on an explicit keypress rather than a keystroke debounce is
// deliberate: a pool that silently rewrites itself while you're reading it is
// disorienting, so we let local filtering handle the common case and reserve the
// network round-trip for when you ask. reloadCmd is a shell snippet (run under
// `sh -c`) printing tab-delimited rows, with the current query at fzf's {q}
// placeholder; initialQuery pre-seeds the prompt so `rig up some words` searches
// immediately while staying editable. Returns "" if the user cancels (fzf exit
// 130).
func fzfLiveSelect(reloadCmd, prompt, initialQuery string) (string, error) {
	if !stdinIsTTY() {
		return "", noTTYError(prompt)
	}
	cmd := exec.Command("fzf",
		"--height=40%", "--reverse",
		"--with-nth=1,2,3", "--delimiter=\t",
		"--prompt="+prompt,
		"--query="+initialQuery,
		"--header=tab: fresh Linear search",
		"--bind=start:reload:"+reloadCmd,
		"--bind=tab:reload:"+reloadCmd,
	)
	// The rows come from the start/change reload binds, not stdin; hand fzf an
	// empty stdin so it doesn't try to read the controlling terminal (which it
	// needs for the UI, opened separately via /dev/tty).
	cmd.Stdin = strings.NewReader("")
	cmd.Stderr = os.Stderr // fzf draws its UI on the controlling tty
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			// 130: Esc/Ctrl-C. 1: no match — with a live search the query often
			// lands on an empty result set, and hitting enter there means "never
			// mind" just as much as an explicit abort does. Both are "no pick."
			if code := ee.ExitCode(); code == 130 || code == 1 {
				return "", nil
			}
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
