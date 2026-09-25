package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		printIndex(os.Stderr)
		os.Exit(2)
	}

	typed, args := os.Args[1], os.Args[2:]

	// Resolve before dispatching so the switch only ever sees canonical names.
	// A shorthand or a typo dies here with a pointed message rather than the
	// whole usage block: eighty lines of help buries the one line that says
	// which command you meant.
	cmd, err := resolveCommand(typed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rig: %v\n", err)
		fmt.Fprintln(os.Stderr, "try `rig help` for the full list")
		os.Exit(2)
	}

	// Help is answered here, before any command runs, so -h/--help can never be
	// taken as a command's input: `rig relay --help` once meant relaying the
	// string "--help". Hidden internals aren't in the table and are exempt.
	if cmd == "help" {
		if err := runHelp(args, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "rig: %v\n", err)
			os.Exit(2)
		}
		return
	}
	if c, ok := findCommand(cmd); ok && wantsHelp(c, args) {
		printCommandHelp(os.Stdout, c)
		return
	}

	// The multiplexer preference is resolved once here and every command
	// drives it through the backend global. An unknown name is fatal rather
	// than a fallback to tmux, because a typo that silently built a rig in the
	// wrong multiplexer would look exactly like the setting never taking. The
	// two commands that don't touch a multiplexer are exempt, and config has
	// to be: it's the one that repairs a stored name this binary doesn't know.
	if cmd != "config" {
		if preferredBackend, err = defaultBackend(); err != nil {
			fmt.Fprintf(os.Stderr, "rig: %v\n", err)
			os.Exit(2)
		}
	}

	switch cmd {
	case "up":
		err = runUp(args)
	case "new":
		err = runNew(args)
	case "project":
		err = runProject(args)
	case "dispatch":
		err = runDispatch(args)
	case "relay":
		err = runRelay(args)
	case "review":
		err = runReview(args)
	case "pr":
		err = runPR(args)
	case "track":
		err = runTrack(args)
	case "adopt":
		err = runAdopt(args)
	case "add":
		err = runAdd(args)
	case "recto":
		err = runRecto(args)
	case "ls":
		err = runLs(args)
	case "notify":
		err = runNotify(args)
	case "switch": // reached as `cd` too, a retained alias
		err = runSwitch(args)
	case "radar":
		err = runRadar(args)
	case "park":
		err = runPark(args)
	case "wake":
		err = runWake(args)
	case "resume":
		err = runResume(args)
	case "waiting":
		err = runWaiting(args)
	case "sweep":
		err = runSweep(args)
	case "down":
		err = runDown(args)
	case "reap":
		err = runReap(args)
	case "history":
		err = runHistory(args)
	case "resurrect":
		err = runResurrect(args)
	case "env":
		err = runEnv(args)
	case "info":
		err = runInfo(args)
	case "config":
		err = runConfigCmd(args)
	case "__gh":
		// Hidden: each rig prepends a tiny `gh` shim that delegates here. Resolve
		// repository context from cwd on every invocation, including agent tool
		// calls that change cwd without running a shell/direnv hook.
		err = runGHShim(args)
	case "__agent":
		// Hidden: the fzf pickers bind ctrl-o to a transform-header that shells out
		// here, since fzf can only hand state back through a file. Not meant to be
		// typed.
		err = runAgentPickCmd(args)
	case "__issues":
		// Hidden: fzf's live issue picker shells out to this on Tab to get fresh
		// rows from the current source. Not in usage; not meant to be typed.
		err = runIssueRows(args)
	case "__source":
		// Hidden: the issue picker binds ctrl-t to a transform-prompt that shells
		// out here to advance the source, the same file round-trip as __agent.
		err = runSourcePickCmd(args)
	case "__teardown":
		// Hidden: durable teardown workers run outside tmux's pane cgroup so
		// they can stop every process scope owned by the rig, including the
		// caller's, without killing cleanup halfway through.
		if len(args) != 1 {
			err = fmt.Errorf("usage: rig __teardown JOB")
		} else {
			err = executeTeardownJobFile(args[0], false)
		}
	default:
		// resolveCommand only ever returns a name this switch handles, so
		// reaching here means the two lists drifted apart.
		fmt.Fprintf(os.Stderr, "rig: command %q has no handler\n", cmd)
		os.Exit(2)
	}

	if err != nil {
		if cmd == "__gh" || cmd == "recto" {
			if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
				os.Exit(exitErr.ExitCode())
			}
		}
		fmt.Fprintf(os.Stderr, "rig: %v\n", err)
		if strings.HasPrefix(err.Error(), "usage: ") {
			fmt.Fprintf(os.Stderr, "run `rig help %s` for details\n", cmd)
		}
		os.Exit(1)
	}
}
