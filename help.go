package main

import (
	"fmt"
	"io"
)

// command is one typeable rig command. This table is the single source for
// resolution (commandNames), the `rig help` index, and per-command help, so a
// command can't exist without help and help can't name a command that doesn't.
//
// Help text is written for an agent reading it cold: what the command does, its
// exact arguments, and the facts needed to call it correctly. Rationale lives in
// CLAUDE.md and DESIGN.md, not here.
type command struct {
	name    string
	group   string
	usage   []string // synopsis lines, without the leading "rig "
	summary string   // one line for the index
	details string   // optional; printed after usage
	// forwards marks a command whose arguments after the first belong to
	// another program, so only the first is checked for -h/--help.
	forwards bool
}

// commandGroups orders the index.
var commandGroups = []string{"start", "work", "navigate", "finish", "plumbing"}

var commands = []command{
	{
		name:    "up",
		group:   "start",
		usage:   []string{"up [ID|QUERY|PR-URL] [--source linear|github|tasks] [--repo OWNER/REPO] [--agent AGENT] [--context TEXT]"},
		summary: "go to a task's rig, creating it if new",
		details: `Argument: a task id (MIR-75, owner/repo#9, a GitHub issue URL, PERS-3), search
terms, or a URL of your own PR. No argument opens an fzf picker (needs a TTY).
Idempotent: an existing rig is switched to, not rebuilt.

  --source   tracker for an id or the picker's start: linear, github (your own
             repos), tasks (Vikunja). Default: routed by id shape, else linear.
  --repo     skip the repo picker. A GitHub issue already names its repo.
  --agent    see "rig help new".
  --context  text written to the new rig's KICKOFF.md. Piped stdin does the
             same. Ignored with a warning if the rig exists; use dispatch.`,
	},
	{
		name:    "new",
		group:   "start",
		usage:   []string{"new [KICKOFF] [--repo OWNER/REPO] [--agent AGENT]"},
		summary: "start a rig for work with no ticket",
		details: `Prompts for the kickoff, then optional pasted context, then the repo. Piped
stdin supplies the context without prompting. The rig starts at trunk with no
branch.

  --agent  claude|cld, codex|cdx, antigravity|agy. Default: $RIG_AGENT, then
           "rig config agent", then claude.`,
	},
	{
		name:    "review",
		group:   "start",
		usage:   []string{"review [PR-URL] [--agent AGENT]", "review --refresh PR-URL"},
		summary: "start or enter a review rig for someone else's PR",
		details: `No argument: fzf picker over PRs awaiting your review plus existing review
rigs. A URL for your own PR routes to "rig up".

  --refresh  advance an existing review rig to the PR's current head.`,
	},
	{
		name:    "project",
		group:   "start",
		usage:   []string{"project [QUERY|URL|UUID] [--agent AGENT]", "project status [--format=json|table]"},
		summary: "enter a Linear project overview rig (no repos)",
		details: `status: the current project's issues joined to local rig, agent, PR, review,
and CI state. Run from inside a project rig.`,
	},
	{
		name:    "adopt",
		group:   "start",
		usage:   []string{"adopt ISSUE"},
		summary: "attach a Linear issue to the current rig",
		details: `For a "rig new" rig that now has a ticket. Sets the tracker and title; id,
paths, session, and branches are unchanged. Enables relay, project status,
and dispatch by issue id. Refuses a rig that already tracks an issue.`,
	},
	{
		name:    "add",
		group:   "work",
		usage:   []string{"add OWNER/REPO"},
		summary: "add another repo to the current rig",
		details: `Clones if needed and creates a jj workspace at trunk() plus a Recto window.`,
	},
	{
		name:     "recto",
		group:    "work",
		usage:    []string{"recto REPO [RECTO-ARGS...]"},
		summary:  "show a repo's Recto beside the agent",
		details:  `Extra arguments go to Recto, e.g. "rig recto cloud focus src/app.go:42".`,
		forwards: true,
	},
	{
		name:    "track",
		group:   "work",
		usage:   []string{"track [BRANCH]"},
		summary: "record an extra PR branch for the current repo",
		details: `Default: the current work's branch. down and sweep then check that branch's
PR alongside the rig's primary one.`,
	},
	{
		name:    "pr",
		group:   "work",
		usage:   []string{"pr"},
		summary: "open one of the current rig's PRs in the browser",
		details: `Picks with fzf when several match.`,
	},
	{
		name:    "dispatch",
		group:   "work",
		usage:   []string{"dispatch RIG-OR-ISSUE [--] PROMPT..."},
		summary: "wake a stopped or parked rig and give its agent a prompt",
		details: `Runs in the background; does not switch to the rig. Refuses a rig whose agent
is already running. Use -- before a prompt that starts with a dash.`,
	},
	{
		name:    "relay",
		group:   "work",
		usage:   []string{"relay MESSAGE..."},
		summary: "send a note from a Linear issue rig to its project rig",
		details: `Local only; nothing is posted to Linear. The current rig must track a Linear
issue that belongs to a project.`,
	},
	{
		name:    "switch",
		group:   "navigate",
		usage:   []string{"switch [QUERY]"},
		summary: "jump to a rig's session (alias: cd)",
		details: `Most recently used first; fzf when ambiguous.`,
	},
	{
		name:    "radar",
		group:   "navigate",
		usage:   []string{"radar [--popup]"},
		summary: "interactive board of every rig and session",
		details: `--popup opens it over the current session.

Keys: type to filter, enter switch/wake/resurrect, ctrl-n new rig, ctrl-p toggle
parked, ctrl-x park or tear down the current rig, ctrl-t show torn-down rigs,
ctrl-r refresh PRs, ctrl-u clear filter, esc/ctrl-c quit.`,
	},
	{
		name:    "ls",
		group:   "navigate",
		usage:   []string{"ls [--format=json|table] [--full]"},
		summary: "list rigs in flight",
		details: `  --full         add PR and CI state (one gh call per repo).
  --format=json  stable machine-readable rows.`,
	},
	{
		name:    "wake",
		group:   "navigate",
		usage:   []string{"wake [QUERY|PR-URL]"},
		summary: "enter a rig, unparking it if parked",
	},
	{
		name:    "resume",
		group:   "navigate",
		usage:   []string{"resume [QUERY]"},
		summary: "rebuild a live rig's agent and Recto panes",
		details: `Default: the rig containing cwd. Does not change parked state.`,
	},
	{
		name:    "park",
		group:   "finish",
		usage:   []string{"park"},
		summary: "mark the current rig awaiting review and close its session",
		details: `Keeps the directory. Inside the rig's session, opens radar to pick where to go
next first. Undo with wake.`,
	},
	{
		name:    "waiting",
		group:   "finish",
		usage:   []string{"waiting"},
		summary: "review status of parked rigs, most actionable first",
	},
	{
		name:    "sweep",
		group:   "finish",
		usage:   []string{"sweep [-n|--dry-run] [--merge-method merge|squash|rebase]"},
		summary: "propose and run each rig's next step (merge, park, wake, tear down)",
		details: `Shows a checkable board, then runs the checked actions; stops at the first
failure. Merges start unchecked. Without a TTY, prints the plan and exits.

  -n              plan only.
  --merge-method  default merge.`,
	},
	{
		name:    "down",
		group:   "finish",
		usage:   []string{"down [-f|--force]"},
		summary: "tear down the current rig",
		details: `Refuses with uncommitted work or an unmerged PR unless --force. Recoverable for
a week with resurrect.`,
	},
	{
		name:    "history",
		group:   "finish",
		usage:   []string{"history"},
		summary: "list recently torn-down rigs that can be resurrected",
	},
	{
		name:    "resurrect",
		group:   "finish",
		usage:   []string{"resurrect RIG-ID"},
		summary: "rebuild a torn-down rig and resume its agent",
		details: `Workspaces return at their recorded branches; uncommitted work does not.`,
	},
	{
		name:  "notify",
		group: "plumbing",
		usage: []string{
			"notify post --source S --key K --title T [--body B] [--level info|warn|error] [--rig ID]",
			"notify list [--format=json|table]",
			"notify dismiss SOURCE/KEY... | --all",
		},
		summary: "post to the inbox shown in ls, sweep, and radar",
		details: `Posting an existing source/key updates that entry and counts repeats.
--rig attaches the entry to that rig's row.`,
	},
	{
		name:    "env",
		group:   "plumbing",
		usage:   []string{"env"},
		summary: "print shell exports for the current dir (used by direnv)",
		details: `Prints nothing outside a rig.`,
	},
	{
		name:    "info",
		group:   "plumbing",
		usage:   []string{"info --format=json"},
		summary: "print the current rig and repo as JSON",
		details: `The supported interface for external tools; don't parse .rig/manifest.toml.`,
	},
	{
		name:    "config",
		group:   "plumbing",
		usage:   []string{"config [SETTING [VALUE]]", "config SETTING --unset"},
		summary: "read or write settings in ~/.config/rig/config.toml",
		details: `No argument lists every setting, its value, and where it came from.

  agent    default agent for new rigs: claude, codex, antigravity.
  backend  multiplexer for new rigs: tmux, rex.`,
	},
	{
		name:    "reap",
		group:   "plumbing",
		usage:   []string{"reap [-n|--dry-run]"},
		summary: "retry stranded teardowns and stop orphaned scopes",
		details: `Never decides which rigs to tear down; that's sweep.`,
	},
}

func findCommand(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// wantsHelp reports whether args ask for help instead of running the command.
// Only -h and --help count, and a bare -- ends the scan, so a literal "--help"
// can still reach a command as data.
func wantsHelp(c command, args []string) bool {
	if c.forwards && len(args) > 1 {
		args = args[:1]
	}
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// runHelp is `rig help [COMMAND]`. The command may be any spelling
// resolveCommand accepts.
func runHelp(args []string, w io.Writer) error {
	if len(args) == 0 {
		printIndex(w)
		return nil
	}
	if len(args) > 1 {
		return fmt.Errorf("usage: rig help [COMMAND]")
	}
	name, err := resolveCommand(args[0])
	if err != nil {
		return err
	}
	c, ok := findCommand(name)
	if !ok {
		printIndex(w)
		return nil
	}
	printCommandHelp(w, c)
	return nil
}

func printIndex(w io.Writer) {
	fmt.Fprintln(w, "rig: one workspace per task (tmux/rex session, jj workspaces, agent)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "usage: rig COMMAND [ARGS]")
	width := 0
	for _, c := range commands {
		width = max(width, len(c.name))
	}
	for _, g := range commandGroups {
		fmt.Fprintf(w, "\n%s:\n", g)
		for _, c := range commands {
			if c.group == g {
				fmt.Fprintf(w, "  %-*s  %s\n", width, c.name, c.summary)
			}
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "rig help COMMAND or rig COMMAND --help for arguments.")
	fmt.Fprintln(w, "Unique prefixes work (rig swe = sweep).")
}

func printCommandHelp(w io.Writer, c command) {
	fmt.Fprintf(w, "rig %s: %s\n\nusage:\n", c.name, c.summary)
	for _, u := range c.usage {
		fmt.Fprintf(w, "  rig %s\n", u)
	}
	if c.details != "" {
		fmt.Fprintf(w, "\n%s\n", c.details)
	}
}
