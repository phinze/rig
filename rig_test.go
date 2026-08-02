package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestTaskSlug(t *testing.T) {
	cases := []struct {
		name, id, title, want string
	}{
		{"basic", "pr-845", "Cleanup redundant logs over time", "pr-845-cleanup-redundant-logs-over-time"},
		{"empty title", "pr-845", "", "pr-845"},
		{"symbols-only title", "pr-845", "!!!", "pr-845"},
		{
			"hard cap at 60 with trailing dash trimmed",
			"mir-1184",
			"add cloud API to return the groups for a user in a sandbox",
			// raw join is 68 chars; cut lands mid-word, Linear-style
			"mir-1184-add-cloud-api-to-return-the-groups-for-a-user-in-a",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := taskSlug(c.id, c.title); got != c.want {
				t.Errorf("taskSlug(%q, %q) = %q, want %q", c.id, c.title, got, c.want)
			}
			if got := taskSlug(c.id, c.title); len(got) > 60 {
				t.Errorf("slug exceeds cap: %d chars", len(got))
			}
		})
	}
}

func TestKickoffID(t *testing.T) {
	cases := []struct {
		name, kickoff, want string
	}{
		{"basic", "Investigate flaky radar refresh", "investigate-flaky-radar-refresh"},
		{"symbols", "  What's up with auth?!  ", "what-s-up-with-auth"},
		{"symbols only", "?!", ""},
		{
			"long kickoff keeps the meaningful subject and outcome",
			"Investigate why the background reconciliation worker occasionally drops queued updates",
			"background-reconciliation-worker-drops-queued-updates",
		},
		{
			"very long kickoff keeps its tail",
			"Rig new's text input is a little janky, can we enhance that to a better readline or bubbles based textinput so e.g. ctrl-u works",
			"rig-new-text-input-janky-enhance-readline-ctrl-u-works",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := kickoffID(c.kickoff); got != c.want {
				t.Errorf("kickoffID(%q) = %q, want %q", c.kickoff, got, c.want)
			}
			if got := kickoffID(c.kickoff); len(got) > 60 {
				t.Errorf("id exceeds cap: %d chars", len(got))
			}
		})
	}
}

// The shared new-rig model is both `rig new`'s whole interactive UI and a mode
// embedded by radar. Exercise its transitions directly so the two hosts inherit
// the same editing, paste, agent, and repo-selection behavior.
func TestNewRigWizardFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, err := newRigWizardModel("", "", agentClaude)
	if err != nil {
		t.Fatal(err)
	}

	m, _ = m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("discard this")})
	m, _ = m.update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m, _ = m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("keep this")})
	m, _ = m.update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.phase != newRigContext || m.target.Kickoff != "keep this" {
		t.Fatalf("kickoff transition = %#v", m)
	}

	// Bracketed CRLF paste is normalized before textarea sees it, and ctrl-o
	// changes only the agent choice rather than leaking into the context.
	m, _ = m.update(tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune("one\r\ntwo")})
	m, _ = m.update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m, cmd := m.update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if m.phase != newRigRepo || m.context != "one\ntwo" || m.agent != agentCodex || cmd == nil {
		t.Fatalf("context transition = {phase:%d context:%q agent:%q cmd:%v}", m.phase, m.context, m.agent, cmd != nil)
	}
	if !strings.Contains(m.View(), "[cdx]") {
		t.Errorf("wizard view does not show cycled agent:\n%s", m.View())
	}

	m, _ = m.update(newRigReposMsg{repos: []repoRef{
		{Owner: "acme", Name: "alpha", Path: "/src/alpha"},
		{Owner: "acme", Name: "beta", Path: "/src/beta"},
	}})
	m, _ = m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("beta")})
	rows := m.repoRows()
	if len(rows) != 1 || rows[0].Name != "beta" {
		t.Fatalf("filtered repos = %+v, want beta", rows)
	}
	m, cmd = m.update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.phase != newRigCreating || cmd == nil {
		t.Fatalf("repo selection = {phase:%d cmd:%v}, want creating command", m.phase, cmd != nil)
	}
}

func TestNewRigWizardContextSkipVersusCancel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, err := newRigWizardModel("investigate the flake", "", agentClaude)
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.phase != newRigRepo || m.cancelled || m.context != "" || cmd == nil {
		t.Fatalf("context escape did not skip into repo picker: %#v", m)
	}

	m, err = newRigWizardModel("", "", agentClaude)
	if err != nil {
		t.Fatal(err)
	}
	m, _ = m.update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m.cancelled || !m.done {
		t.Fatalf("ctrl-c did not cancel wizard: %#v", m)
	}
}

func TestNewRigWizardFitsRadarPopup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, height := range []int{16, 24, 40} {
		m, err := newRigWizardModel("investigate the flake", "", agentClaude)
		if err != nil {
			t.Fatal(err)
		}
		m, _ = m.update(tea.WindowSizeMsg{Width: 100, Height: height})
		if lines := strings.Count(m.View(), "\n") + 1; lines > height {
			t.Errorf("context height=%d: view is %d lines", height, lines)
		}

		m, _ = m.update(tea.KeyMsg{Type: tea.KeyEsc})
		var repos []repoRef
		for i := range 100 {
			repos = append(repos, repoRef{Owner: "acme", Name: fmt.Sprintf("repo-%03d", i), Path: fmt.Sprintf("/src/repo-%03d", i)})
		}
		m, _ = m.update(newRigReposMsg{repos: repos})
		m.cursor = 75
		if lines := strings.Count(m.View(), "\n") + 1; lines > height {
			t.Errorf("repo height=%d: view is %d lines", height, lines)
		}
	}
}

func TestKickoffPromptMentionsPastedContext(t *testing.T) {
	plain := kickoffPrompt("investigate the flake", false)
	if !strings.Contains(plain, "investigate the flake") || !strings.Contains(plain, "no ticket to read") {
		t.Errorf("bare kickoff prompt = %q", plain)
	}
	if strings.Contains(plain, rigKickoffName) {
		t.Errorf("bare kickoff prompt points at a file that wasn't written: %q", plain)
	}

	withContext := kickoffPrompt("investigate the flake", true)
	if !strings.Contains(withContext, "../"+rigKickoffName) {
		t.Errorf("context kickoff prompt = %q, want a pointer to ../%s", withContext, rigKickoffName)
	}
}

func TestResolveKickoffInline(t *testing.T) {
	got, err := resolveKickoff([]string{" investigate", "the flake "})
	if err != nil {
		t.Fatal(err)
	}
	if got != "investigate the flake" {
		t.Errorf("resolveKickoff = %q, want %q", got, "investigate the flake")
	}
}
