package main

import (
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

func TestKickoffPromptEditing(t *testing.T) {
	m := newKickoffPromptModel()

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("discard this")})
	m = updated.(kickoffPromptModel)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m = updated.(kickoffPromptModel)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("keep this")})
	m = updated.(kickoffPromptModel)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(kickoffPromptModel)

	if m.kickoff != "keep this" {
		t.Errorf("kickoff = %q, want %q", m.kickoff, "keep this")
	}
	if cmd == nil || !m.done {
		t.Error("enter did not finish the prompt")
	}
}

func TestKickoffPromptEscapeCancels(t *testing.T) {
	m := newKickoffPromptModel()
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(kickoffPromptModel)

	if m.kickoff != "" || !m.done || cmd == nil {
		t.Errorf("escape left prompt in state %#v", m)
	}
}

// The context step is a textarea, so enter has to stay a newline and ctrl-d
// has to mean "done" — including the empty case, which is the common one.
func TestContextPromptAcceptsMultiline(t *testing.T) {
	m := newContextPromptModel("investigate the flake")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("first line")})
	m = updated.(contextPromptModel)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(contextPromptModel)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("second line")})
	m = updated.(contextPromptModel)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = updated.(contextPromptModel)

	if m.context != "first line\nsecond line" {
		t.Errorf("context = %q, want two lines", m.context)
	}
	if cmd == nil || !m.done || m.cancelled {
		t.Errorf("ctrl-d left prompt in state %#v", m)
	}
}

// A paste arrives as one key message whose runes carry the line breaks; CRLF
// content must not come out double-spaced.
func TestContextPromptPasteNormalizesCRLF(t *testing.T) {
	m := newContextPromptModel("investigate the flake")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune("one\r\ntwo\r\n\r\nthree")})
	m = updated.(contextPromptModel)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = updated.(contextPromptModel)

	if want := "one\ntwo\n\nthree"; m.context != want {
		t.Errorf("pasted context = %q, want %q", m.context, want)
	}
}

// Escape skips the step (an empty blob), ctrl-c abandons `rig new` outright.
// The difference matters: one goes on to build the rig, the other doesn't.
func TestContextPromptSkipVersusCancel(t *testing.T) {
	skipped, _ := newContextPromptModel("k").Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m := skipped.(contextPromptModel); m.context != "" || m.cancelled || !m.done {
		t.Errorf("escape left prompt in state %#v", m)
	}

	cancelled, _ := newContextPromptModel("k").Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if m := cancelled.(contextPromptModel); !m.cancelled || !m.done {
		t.Errorf("ctrl-c left prompt in state %#v", m)
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
