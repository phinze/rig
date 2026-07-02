package main

import (
	"fmt"
	"os"
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
	case "down":
		err = runDown(args)
	case "reap":
		err = runReap(args)
	case "env":
		err = runEnv(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "rig: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "rig: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `rig: workspace tool for task-shaped work

usage:
  rig up [issue-id|query]   pitch a new rig from a Linear issue
                            (exact id, search terms, or fzf picker with no args)
  rig review [pr-url]       pitch a review rig for a PR
                            (url, or fzf picker over review-requested PRs)
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
  rig down                  break the current rig down
  rig reap [-n] [--max-idle SECONDS]
                            break down every rig that is merged, WIP-free,
                            and idle (default 24h) — fails closed on doubt
  rig env                   print shell exports describing the current dir
                            (eval'd by the direnv stdlib; silent outside a rig)
`)
}
