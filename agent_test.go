package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestExtractAgentFlag(t *testing.T) {
	t.Setenv("RIG_AGENT", "codex")
	pick, rest, err := extractAgentFlag([]string{"FAKE-1", "--agent", "agy", "--repo", "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	if pick.kind != agentAntigravity {
		t.Errorf("agent = %q, want antigravity", pick.kind)
	}
	if !pick.explicit {
		t.Error("--agent should mark the choice explicit so nothing prompts for it")
	}
	if got := strings.Join(rest, " "); got != "FAKE-1 --repo o/r" {
		t.Errorf("rest = %q", got)
	}

	// The env var only moves the starting position: it's a standing shell
	// preference, and having one must not count as having picked this time.
	pick, _, err = extractAgentFlag(nil)
	if err != nil || pick.kind != agentCodex {
		t.Errorf("env default = %q, %v; want codex", pick.kind, err)
	}
	if pick.explicit {
		t.Error("RIG_AGENT should not suppress the pick")
	}
}

func TestParseAgentShortNames(t *testing.T) {
	cases := map[string]agentKind{
		"cld": agentClaude, "claude": agentClaude,
		"cdx": agentCodex, "codex": agentCodex,
		"agy": agentAntigravity, "antigravity": agentAntigravity,
		"CDX": agentCodex,
	}
	for name, want := range cases {
		got, err := parseAgent(name)
		if err != nil || got != want {
			t.Errorf("parseAgent(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := parseAgent("gemini"); err == nil {
		t.Error("unknown agent should error")
	}
}

func TestAgentCycleWraps(t *testing.T) {
	seen := []agentKind{}
	k := agentClaude
	for range agentKinds {
		seen = append(seen, k)
		k = k.next()
	}
	if k != agentClaude {
		t.Errorf("cycle did not wrap: ended at %q", k)
	}
	if len(seen) != len(agentKinds) {
		t.Errorf("cycle visited %d agents, want %d", len(seen), len(agentKinds))
	}
	if agentClaude.prev() != agentAntigravity {
		t.Errorf("prev from claude = %q, want antigravity", agentClaude.prev())
	}
}

func TestAgentBarMarksSelection(t *testing.T) {
	bar := agentBar(agentCodex)
	if !strings.Contains(bar, "[cdx]") {
		t.Errorf("bar = %q, want cdx selected", bar)
	}
	for _, other := range []string{"[cld]", "[agy]"} {
		if strings.Contains(bar, other) {
			t.Errorf("bar = %q, want only one selection", bar)
		}
	}
	if !strings.Contains(bar, agentCycleKey) {
		t.Errorf("bar = %q, want it to name the cycle key", bar)
	}
}

// The fzf pickers hand the choice back through a file, so a cycle has to
// survive the round trip and reprint the picker's own hint above the bar.
func TestAgentCycleCommandRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte("claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := cycleAgentState(path, "tab: fresh Linear search")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "tab: fresh Linear search\n") {
		t.Errorf("header = %q, want the picker hint on top", out)
	}
	if !strings.Contains(out, "[cdx]") {
		t.Errorf("header = %q, want the cycled agent selected", out)
	}

	pick := &agentPick{kind: agentClaude, statePath: path}
	pick.sync()
	if pick.kind != agentCodex {
		t.Errorf("after sync kind = %q, want codex", pick.kind)
	}
}

// A temp file that vanished or filled up with junk costs you the keystroke,
// not the rig: the choice stays where it was.
func TestAgentSyncSurvivesGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte("not-an-agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	pick := &agentPick{kind: agentCodex, statePath: path}
	pick.sync()
	if pick.kind != agentCodex {
		t.Errorf("garbage state changed the choice to %q", pick.kind)
	}

	pick = &agentPick{kind: agentCodex, statePath: filepath.Join(t.TempDir(), "missing")}
	pick.sync()
	if pick.kind != agentCodex {
		t.Errorf("missing state changed the choice to %q", pick.kind)
	}
}

func TestAgentPromptModelKeys(t *testing.T) {
	m := agentPromptModel{kind: agentClaude}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = updated.(agentPromptModel)
	if m.kind != agentCodex {
		t.Errorf("%s left kind at %q", agentCycleKey, m.kind)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = updated.(agentPromptModel)
	if m.kind != agentClaude {
		t.Errorf("left arrow left kind at %q", m.kind)
	}

	accepted, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if final := accepted.(agentPromptModel); !final.done || final.cancelled || cmd == nil {
		t.Errorf("enter left prompt in state %#v", final)
	}
	cancelled, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if final := cancelled.(agentPromptModel); !final.cancelled {
		t.Error("esc should abandon the command, not accept the default")
	}
}

// The standalone bar exists for invocations that prompt for nothing else, so
// anything that already asked — or already told us — must not trigger it.
func TestEnsurePickedSkipsWhenAlreadySettled(t *testing.T) {
	for name, pick := range map[string]*agentPick{
		"explicit flag": {kind: agentCodex, explicit: true},
		"already shown": {kind: agentCodex, shown: true},
	} {
		ok, err := pick.ensurePicked()
		if err != nil || !ok {
			t.Errorf("%s: ensurePicked = %v, %v; want a silent pass", name, ok, err)
		}
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

func TestProjectAgentLaunchCommandUsesRootInstructions(t *testing.T) {
	got := agentCodex.launchProjectCommand("assess the project")
	want := "codex --dangerously-bypass-approvals-and-sandbox 'Read the project rig instructions in ./AGENTS.md first. assess the project'"
	if got != want {
		t.Errorf("project launch = %q, want %q", got, want)
	}
}

func TestAgentResumeCommandsCarryDispatchPrompt(t *testing.T) {
	prompt := "Run address-pr-review and handle the latest feedback."
	cases := []struct {
		agent agentKind
		want  string
	}{
		{agentClaude, "claude --dangerously-skip-permissions --resume 'session-1' 'Run address-pr-review and handle the latest feedback.'"},
		{agentCodex, "codex --dangerously-bypass-approvals-and-sandbox resume 'session-1' 'Run address-pr-review and handle the latest feedback.'"},
		{agentAntigravity, "agy --dangerously-skip-permissions --conversation 'session-1' --prompt-interactive 'Run address-pr-review and handle the latest feedback.'"},
	}
	for _, tc := range cases {
		if got := tc.agent.resumeCommandWithPrompt("session-1", prompt); got != tc.want {
			t.Errorf("%s dispatch resume = %q, want %q", tc.agent, got, tc.want)
		}
	}
}
