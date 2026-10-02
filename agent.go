package main

import (
	"fmt"
	"strings"
)

type agentKind string

const (
	agentClaude      agentKind = "claude"
	agentCodex       agentKind = "codex"
	agentAntigravity agentKind = "antigravity"
	agentPi          agentKind = "pi"
)

// agentKinds is the cycle order the picker walks, and the order every list of
// agents renders in. Fixed, and independent of which agent starts selected:
// agentBar moves brackets rather than a cursor, so the columns hold still while
// the choice travels. Claude leads because it was the original default.
var agentKinds = []agentKind{agentClaude, agentCodex, agentAntigravity, agentPi}

// short is the name an agent goes by in the picker bar and on the command
// line: three letters, except Pi, whose own name is already shorter than any
// abbreviation of it. They line up in the width of a word or two, which is what
// makes the bar cheap enough to hang off a prompt you're already looking at.
func (a agentKind) short() string {
	switch a {
	case agentCodex:
		return "cdx"
	case agentAntigravity:
		return "agy"
	case agentPi:
		return "pi"
	default:
		return "cld"
	}
}

func (a agentKind) next() agentKind { return agentStep(a, 1) }
func (a agentKind) prev() agentKind { return agentStep(a, -1) }

func agentStep(a agentKind, delta int) agentKind {
	for i, k := range agentKinds {
		if k == a {
			return agentKinds[(i+delta+len(agentKinds))%len(agentKinds)]
		}
	}
	return agentClaude
}

// parseAgent maps a spelling to an agent. It is a pure parser and deliberately
// consults nothing: an empty name means Claude, because that is what an empty
// `agent` field in a manifest or tombstone has always meant. The standing
// preference lives in defaultAgent instead, which is what keeps changing your
// default from retroactively relabelling every rig recorded before the field
// existed.
func parseAgent(name string) (agentKind, error) {
	if name == "" {
		return agentClaude, nil
	}
	switch agentKind(strings.ToLower(name)) {
	case agentClaude, "cld":
		return agentClaude, nil
	case agentCodex, "cdx":
		return agentCodex, nil
	case agentAntigravity, "agy":
		return agentAntigravity, nil
	case agentPi:
		return agentPi, nil
	default:
		return "", fmt.Errorf("unknown agent %q (want claude/cld, codex/cdx, antigravity/agy, or pi)", name)
	}
}

// extractAgentFlag removes --agent NAME (or --agent=NAME) from a command's
// arguments and resolves it against the standing default. The flag wins, and
// naming one explicitly is also what suppresses the interactive pick: you don't
// get asked what you just said. The default only moves the starting position:
// it's a preference about new rigs in general, which is a different statement
// from "this rig, this one time" — so it seeds the bar and still lets you see
// it.
func extractAgentFlag(args []string) (*agentPick, []string, error) {
	name := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--agent" {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("--agent needs a value")
			}
			name = args[i+1]
			i++
			continue
		}
		if v, ok := strings.CutPrefix(a, "--agent="); ok {
			name = v
			continue
		}
		rest = append(rest, a)
	}
	if name == "" {
		return newAgentPick(defaultAgent(), false), rest, nil
	}
	agent, err := parseAgent(name)
	if err != nil {
		return nil, nil, err
	}
	return newAgentPick(agent, true), rest, nil
}

// resumeCommand reopens an existing conversation by id, which is how a
// resurrected rig gets its context back rather than starting cold. It carries
// the same permission flags launchCommand does, because a resumed session is
// still working inside a rig and shouldn't suddenly start prompting. Each agent
// spells this differently and the spellings were verified against the installed
// CLIs: codex takes a subcommand (its bypass flag is global, so it precedes
// it), claude, antigravity, and pi take flags. Pi has no permission prompts
// to bypass, and its prompt follows `--` so a message that happens to start
// with a dash can't be read as an option.
func (a agentKind) resumeCommand(sessionID string) string {
	return a.resumeCommandWithPrompt(sessionID, "")
}

// resumeCommandWithPrompt reopens a conversation and optionally gives it its
// next assignment in the same invocation. This is the safe handoff path for a
// parked rig: the prompt arrives as part of agent startup rather than being
// typed into a process whose input state Rig cannot know.
func (a agentKind) resumeCommandWithPrompt(sessionID, prompt string) string {
	quoted := shellQuote(sessionID)
	promptArg := ""
	if prompt != "" {
		promptArg = " " + shellQuote(prompt)
	}
	switch a {
	case agentCodex:
		return "codex --dangerously-bypass-approvals-and-sandbox resume " + quoted + promptArg
	case agentAntigravity:
		line := "agy --dangerously-skip-permissions --conversation " + quoted
		if prompt != "" {
			line += " --prompt-interactive" + promptArg
		}
		return line
	case agentPi:
		line := "pi --session " + quoted
		if prompt != "" {
			line += " --" + promptArg
		}
		return line
	default:
		return "claude --dangerously-skip-permissions --resume " + quoted + promptArg
	}
}

// findsRigInstructions reports whether the agent discovers the rig's
// generated instructions on its own. Claude and Pi both walk parent
// directories for CLAUDE.md/AGENTS.md, so starting one level below the rig
// root is enough; the others get an explicit breadcrumb in their first prompt.
func (a agentKind) findsRigInstructions() bool {
	return a == agentClaude || a == agentPi
}

func (a agentKind) launchCommand(prompt string) string {
	if !a.findsRigInstructions() {
		prompt = "Read the rig instructions in ../AGENTS.md first. " + prompt
	}
	return a.launchPromptCommand(prompt)
}

// launchCoordinatorCommand is the repositoryless sibling of launchCommand. Task
// agents start one directory below their rig instructions; coordinator agents
// (project and cos) start at the rig root, so their explicit breadcrumb is ./AGENTS.md.
func (a agentKind) launchCoordinatorCommand(prompt string) string {
	if !a.findsRigInstructions() {
		prompt = "Read the rig instructions in ./AGENTS.md first. " + prompt
	}
	return a.launchPromptCommand(prompt)
}

func (a agentKind) launchPromptCommand(prompt string) string {
	quoted := shellQuote(prompt)
	switch a {
	case agentCodex:
		return "codex --dangerously-bypass-approvals-and-sandbox " + quoted
	case agentAntigravity:
		return "agy --dangerously-skip-permissions --prompt-interactive " + quoted
	case agentPi:
		return "pi -- " + quoted
	default:
		return "claude --dangerously-skip-permissions " + quoted
	}
}
