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
	case "add":
		err = runAdd(args)
	case "ls":
		err = runLs(args)
	case "cd":
		err = runCd(args)
	case "down":
		err = runDown(args)
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
  rig add <owner/repo>      add another repo to the rig you're in
  rig ls                    list rigs in flight
  rig cd [query]            jump to a rig's tmux session (fzf if ambiguous)
  rig down                  break the current rig down
`)
}
