package main

import (
	"os"
	"strings"
	"testing"
)

func TestClaudeLaunchLine(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name  string
		kind  agentKind
		line  string
		check func(got string) bool
	}{
		{"fresh launch wraps", agentClaude,
			"claude --dangerously-skip-permissions 'pick up MIR-75'",
			func(got string) bool {
				return strings.HasPrefix(got, "claude --settings '"+dir+"/.rig/claude-settings.json' --name 'mir-75' ") &&
					strings.Contains(got, "--dangerously-skip-permissions 'pick up MIR-75'")
			}},
		{"resume wraps", agentClaude,
			"claude --dangerously-skip-permissions --resume abc-123",
			func(got string) bool {
				return strings.HasPrefix(got, "claude --settings '") && strings.HasSuffix(got, "--resume abc-123")
			}},
		{"codex untouched", agentCodex,
			"codex --dangerously-bypass-approvals-and-sandbox 'go'",
			func(got string) bool { return got == "codex --dangerously-bypass-approvals-and-sandbox 'go'" }},
		{"unexpected prefix untouched", agentClaude,
			"bash -c claude",
			func(got string) bool { return got == "bash -c claude" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := claudeLaunchLine(dir, "mir-75", c.kind, c.line)
			if !c.check(got) {
				t.Errorf("claudeLaunchLine = %q", got)
			}
		})
	}

	// The settings file landed and is not rewritten when identical.
	path := claudeRigSettingsPath(dir)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("settings not written: %v", err)
	}
	if string(body) != claudeRigSettings {
		t.Errorf("settings = %q, want %q", body, claudeRigSettings)
	}
	info1, _ := os.Stat(path)
	claudeLaunchLine(dir, "mir-75", agentClaude, "claude 'go'")
	info2, _ := os.Stat(path)
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("identical settings were rewritten")
	}
}

func TestClaudeLaunchLineNameQuoting(t *testing.T) {
	dir := t.TempDir()
	// A rig id with a space would be two words; the name must arrive quoted.
	got := claudeLaunchLine(dir, "my rig", agentClaude, "claude 'go'")
	if !strings.Contains(got, `--name 'my rig' `) {
		t.Errorf("name quoting = %q", got)
	}
}
