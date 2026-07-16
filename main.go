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
	case "ls":
		err = runLs(args)
	case "switch", "cd": // cd is a retained alias
		err = runSwitch(args)
	case "radar":
		err = runRadar(args)
	case "park":
		err = runPark(args)
	case "wake":
		err = runWake(args)
	case "waiting":
		err = runWaiting(args)
	case "down":
		err = runDown(args)
	case "reap":
		err = runReap(args)
	case "env":
		err = runEnv(args)
	case "__gh":
		// Hidden: each rig prepends a tiny `gh` shim that delegates here. Resolve
		// repository context from cwd on every invocation, including agent tool
		// calls that change cwd without running a shell/direnv hook.
		err = runGHShim(args)
	case "__issues":
		// Hidden: fzf's live issue picker shells out to this on each keystroke to
		// get fresh Linear-search rows. Not in usage; not meant to be typed.
		err = runIssueRows(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "rig: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		if cmd == "__gh" {
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
                            Agent is claude (default), codex, or antigravity;
                            RIG_AGENT sets the default
  rig new [kickoff] [--repo owner/repo] [--agent AGENT]
                            start unticketed work in a normal authoring rig
                            (prompts for the kickoff when omitted, then uses the
                            same repo picker as up; starts at trunk with no
                            branch recorded yet)
  rig review [pr-url] [--agent AGENT]
                            pitch a review rig for someone else's PR
                            (url, or fzf picker over review-requested PRs;
                            a URL that turns out to be yours routes to up)
  rig pr                    open the rig's PR in the browser
                            (gh pr view -w, but it knows the jj branch)
  rig track [branch]        record a secondary PR branch for the repo you're in
                            (defaults to the current work's branch) so down and
                            reap gate on it alongside the rig's primary PR
  rig add <owner/repo>      add another repo to the rig you're in
  rig ls [--full]           list rigs in flight
                            (--full adds PR/CI, one gh call per repo)
  rig switch [query]        jump to a rig's tmux session, most-recently-used
                            first (fzf if ambiguous; aliased as rig cd)
  rig radar                 live TUI board over every rig, meant for a tmux
                            popup: in-flight rigs to switch to, parked rigs
                            ranked by review status; enter switches or wakes
  rig park                  park the current rig: mark it awaiting-review,
                            kill its session, drop it from switch (dir kept)
  rig wake [query]          wake a parked rig back into a session at its old cwd
  rig waiting               review status of parked rigs, most-actionable first
                            (which came back with changes, which are mergeable)
  rig down [--force]        break the current rig down
                            (refuses if it has WIP or an unmerged PR; --force
                            overrides)
  rig reap [-n] [--max-idle SECONDS]
                            break down every rig that is merged, WIP-free,
                            and idle (default 24h) — fails closed on doubt
  rig env                   print shell setup describing the current dir
                            (eval'd by the direnv stdlib; silent outside a rig)
`)
}
