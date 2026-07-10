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

func parseAgent(name string) (agentKind, error) {
	if name == "" {
		name = os.Getenv("RIG_AGENT")
	}
	if name == "" {
		return agentClaude, nil
	}
	switch agentKind(strings.ToLower(name)) {
	case agentClaude:
		return agentClaude, nil
	case agentCodex:
		return agentCodex, nil
	case agentAntigravity, "agy":
		return agentAntigravity, nil
	default:
		return "", fmt.Errorf("unknown agent %q (want claude, codex, or antigravity)", name)
	}
}

// extractAgentFlag removes --agent NAME (or --agent=NAME) from a command's
// arguments and resolves it against RIG_AGENT. The flag wins over the env var.
func extractAgentFlag(args []string) (agentKind, []string, error) {
	name := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--agent" {
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--agent needs a value")
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
	return agent, rest, err
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
