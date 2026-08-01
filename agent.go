package main

import (
	"fmt"
	"os"
	"strings"
)

type agentKind string

const (
	agentClaude      agentKind = "claude"
	agentCodex       agentKind = "codex"
	agentAntigravity agentKind = "antigravity"
)

// agentKinds is the cycle order the picker walks, and the order every list of
// agents renders in. Claude leads because it's the compatibility default.
var agentKinds = []agentKind{agentClaude, agentCodex, agentAntigravity}

// short is the three-letter name an agent goes by in the picker bar and on the
// command line. Three of them line up in the width of one word, which is what
// makes the bar cheap enough to hang off a prompt you're already looking at.
func (a agentKind) short() string {
	switch a {
	case agentCodex:
		return "cdx"
	case agentAntigravity:
		return "agy"
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

func parseAgent(name string) (agentKind, error) {
	if name == "" {
		name = os.Getenv("RIG_AGENT")
	}
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
	default:
		return "", fmt.Errorf("unknown agent %q (want claude/cld, codex/cdx, or antigravity/agy)", name)
	}
}

// extractAgentFlag removes --agent NAME (or --agent=NAME) from a command's
// arguments and resolves it against RIG_AGENT. The flag wins over the env var,
// and naming one explicitly is also what suppresses the interactive pick: you
// don't get asked what you just said. RIG_AGENT only moves the starting
// position, since a rig exports it to everything running inside it.
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
	agent, err := parseAgent(name)
	if err != nil {
		return nil, nil, err
	}
	return newAgentPick(agent, name != ""), rest, nil
}

func (a agentKind) launchCommand(prompt string) string {
	if a != agentClaude {
		prompt = "Read the rig instructions in ../AGENTS.md first. " + prompt
	}
	quoted := shellQuote(prompt)
	switch a {
	case agentCodex:
		return "codex --dangerously-bypass-approvals-and-sandbox " + quoted
	case agentAntigravity:
		return "agy --dangerously-skip-permissions --prompt-interactive " + quoted
	default:
		return "claude --dangerously-skip-permissions " + quoted
	}
}
