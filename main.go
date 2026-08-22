package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "up":
		err = runUp(args)
	case "new":
		err = runNew(args)
	case "review":
		err = runReview(args)
	case "pr":
		err = runPR(args)
	case "track":
		err = runTrack(args)
	case "add":
		err = runAdd(args)
	case "recto":
		err = runRecto(args)
	case "ls":
		err = runLs(args)
	case "notify":
		err = runNotify(args)
	case "switch", "cd": // cd is a retained alias
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
		// Hidden: fzf's live issue picker shells out to this on each keystroke to
		// get fresh Linear-search rows. Not in usage; not meant to be typed.
		err = runIssueRows(args)
	case "__teardown":
		// Hidden: durable teardown workers run outside tmux's pane cgroup so
		// they can stop every process scope owned by the rig, including the
		// caller's, without killing cleanup halfway through.
		if len(args) != 1 {
			err = fmt.Errorf("usage: rig __teardown JOB")
		} else {
			err = executeTeardownJobFile(args[0], false)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "rig: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		if cmd == "__gh" || cmd == "recto" {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				os.Exit(exitErr.ExitCode())
			}
		}
		fmt.Fprintf(os.Stderr, "rig: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `rig: workspace tool for task-shaped work

usage:
  rig up [issue|query|pr] [--repo owner/repo] [--agent AGENT]
                            go to your rig for a task, creating it if it's new
                            (Linear id, search terms, no-arg fzf picker, or a PR
                            of yours to resume; idempotent — re-up just switches.
                            Repo is chosen by an fzf picker over ghq repos, cwd
                            pre-selected on top, unless --repo names one)
                            Agent is cld/claude, cdx/codex, or agy/antigravity,
                            defaulting to $RIG_AGENT and then to claude. Every
                            prompt a creation command already shows carries an
                            agent bar that ctrl-o cycles; an invocation that
                            prompts for nothing gets the bar on its own.
                            --agent picks without asking
  rig new [kickoff] [--repo owner/repo] [--agent AGENT]
                            start unticketed work in a normal authoring rig
                            (prompts for the kickoff when omitted, then for a
                            blob of context to paste (esc skips it, and piped
                            stdin supplies it without a prompt), then picks the
                            repo; the same TUI opens inside radar on ctrl-n;
                            starts at trunk with no branch recorded yet)
  rig review [pr-url] [--agent AGENT]
                            pitch a review rig for someone else's PR
                            (url, or fzf picker over review-requested PRs;
                            a URL that turns out to be yours routes to up)
  rig pr                    open one of the rig's PRs in the browser
                            (across added repos and tracked/current branches;
                            when several match, pick one with fzf)
  rig track [branch]        record a secondary PR branch for the repo you're in
                            (defaults to the current work's branch) so down and
                            reap gate on it alongside the rig's primary PR
  rig add <owner/repo>      add another repo to the rig you're in
  rig recto <repo> [args]   pull that repo's persistent Recto beside the main
                            agent; optional args are forwarded to Recto there
                            (for example: rig recto cloud focus src/app.go:42)
  rig ls [--full]           list rigs in flight
                            (--full adds PR/CI, one gh call per repo)
  rig notify post --source S --key K --title T [--body B] [--level info|warn|error] [--rig ID]
  rig notify list [--format=json|table]
  rig notify dismiss <source/key>... | --all
                            the ambient inbox: anything that isn't a rig (crons,
                            watchers, nix-config-sync) posts here and it shows up
                            in ls, sweep and radar. Re-posting a key updates one
                            entry and counts the repeats instead of piling up
  rig switch [query]        jump to a rig's tmux session, most-recently-used
                            first (fzf if ambiguous; aliased as rig cd)
  rig radar                 live TUI board over every rig, meant for a tmux
                            popup: in-flight rigs to switch to, parked rigs
                            ranked by review status; enter switches or wakes,
                            ctrl-p toggles the selected rig's parked state
  rig park                  park the current rig: mark it awaiting-review,
                            kill its session, drop it from switch (dir kept)
  rig wake [query|PR-URL]   resume a rig, waking it first when parked
  rig resume [query]        repair and enter an active rig's agent + Recto
                            session (cwd rig by default; does not unpark)
  rig waiting               review status of parked rigs, most-actionable first
                            (which came back with changes, which are mergeable)
  rig sweep [-n] [--merge-method merge|squash|rebase]
                            the Monday pass: a board of every rig's proposed
                            next step, checkable, then it streams the work.
                            Merged rigs are pre-checked to tear down; approved
                            and green PRs are offered to merge but start
                            unchecked (merge commits by default). Stops at the
                            first failure. -n plans without touching anything;
                            outside a terminal it prints the plan and stops
  rig down [--force]        break the current rig down
                            (refuses if it has WIP or an unmerged PR; --force
                            overrides)
  rig reap [-n]             retry stranded teardown jobs and kill escaped
                            orphan scopes. Deciding which rigs should stop
                            existing is rig sweep's job, never reap's
  rig history               rigs torn down recently and still recoverable
  rig resurrect <id>        rebuild a torn-down rig and resume its agent
                            session (workspaces come back at their recorded
                            branches; uncommitted work does not)
  rig env                   print shell setup describing the current dir
                            (eval'd by the direnv stdlib; silent outside a rig)
`)
}
