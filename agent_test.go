package main

import (
	"strings"
	"testing"
)

func TestExtractAgentFlag(t *testing.T) {
	t.Setenv("RIG_AGENT", "codex")
	agent, rest, err := extractAgentFlag([]string{"FAKE-1", "--agent", "agy", "--repo", "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	if agent != agentAntigravity {
		t.Errorf("agent = %q, want antigravity", agent)
	}
	if got := strings.Join(rest, " "); got != "FAKE-1 --repo o/r" {
		t.Errorf("rest = %q", got)
	}

	agent, _, err = extractAgentFlag(nil)
	if err != nil || agent != agentCodex {
		t.Errorf("env default = %q, %v; want codex", agent, err)
	}
}

func TestAgentLaunchCommand(t *testing.T) {
	cases := []struct {
		agent agentKind
		want  string
	}{
		{agentClaude, "claude --dangerously-skip-permissions 'do it'"},
		{agentCodex, "codex --dangerously-bypass-approvals-and-sandbox 'Read the rig instructions in ../AGENTS.md first. do it'"},
		{agentAntigravity, "agy --dangerously-skip-permissions --prompt-interactive 'Read the rig instructions in ../AGENTS.md first. do it'"},
	}
	for _, c := range cases {
		if got := c.agent.launchCommand("do it"); got != c.want {
			t.Errorf("%s command = %q, want %q", c.agent, got, c.want)
		}
	}
}
