package main

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// agentCycleKey is the keystroke that advances the agent choice wherever a
// creation command is already prompting. Picking it is mostly a process of
// elimination across the three surfaces the bar rides on. fzf, bubbles'
// textinput, and its textarea all bind ctrl-a to beginning-of-line, so that one
// would cost readline muscle memory in a field you're typing into; between them
// they also claim b, c, d, e, f, g, h, j, k, l, m, n, p, q, t, u, v, w, and y.
// Of what's left, ctrl-n and ctrl-r already mean new-rig and refresh in the
// radar, and ctrl-x reads as an emacs prefix waiting for a second key. ctrl-o
// is free everywhere and means nothing else in rig.
const agentCycleKey = "ctrl-o"

// agentPick is the agent choice as it travels through a creation command. The
// choice is deliberately not a screen of its own: it rides along on whatever
// prompt is already up (kickoff line, context textarea, issue/repo/PR picker),
// costing nothing when you don't care and one keystroke when you do. Only when
// a command turns out to prompt for nothing at all — `rig review <url>`, or an
// `up` with both the id and --repo supplied — does the bar become its own
// prompt, which is the only way those invocations get to pick at all.
//
// statePath exists because the fzf pickers can't hand state back through their
// exit status: fzf cycles the choice by shelling out to `rig __agent cycle`
// (the same hidden-subcommand idiom the live issue picker uses), which
// read-modify-writes that file and prints the redrawn header.
type agentPick struct {
	kind      agentKind
	explicit  bool // --agent named one; don't prompt for what you already said
	shown     bool // some prompt has carried the bar this run
	statePath string
}

func newAgentPick(kind agentKind, explicit bool) *agentPick {
	return &agentPick{kind: kind, explicit: explicit}
}

// cycle advances to the next agent, wrapping. Bubble Tea prompts call this
// directly; fzf pickers get the same movement through `rig __agent cycle`.
func (p *agentPick) cycle() {
	if p == nil {
		return
	}
	p.kind = p.kind.next()
	p.shown = true
}

// offered marks the choice as having been on screen and hands back the current
// agent, which is the shape a Bubble Tea constructor wants: it seeds the model
// and records that this run doesn't owe you a standalone bar. A nil pick (tests,
// and any prompt not tied to a creation command) reads as plain Claude.
func (p *agentPick) offered() agentKind {
	if p == nil {
		return agentClaude
	}
	p.shown = true
	return p.kind
}

// bar renders the choice for a prompt footer or an fzf header.
func (p *agentPick) bar() string {
	if p == nil {
		return ""
	}
	return agentBar(p.kind)
}

func agentBar(kind agentKind) string {
	cells := make([]string, 0, len(agentKinds))
	for _, a := range agentKinds {
		// Brackets rather than a moving cursor so the row keeps its column
		// positions as the selection travels.
		if a == kind {
			cells = append(cells, "["+a.short()+"]")
		} else {
			cells = append(cells, " "+a.short()+" ")
		}
	}
	return "agent: " + strings.Join(cells, " ") + " · " + agentCycleKey + " cycles"
}

// agentHeader is the full fzf header block: the picker's own hint (when it has
// one) above the agent bar. `rig __agent cycle` reprints this same block, so a
// cycle can't drift from what the picker started with.
func agentHeader(kind agentKind, hint string) string {
	if hint == "" {
		return agentBar(kind)
	}
	return hint + "\n" + agentBar(kind)
}

// fzfArgs returns the header and bind that put the bar on an fzf picker, and
// marks the choice as offered. hint is the picker's own header line, if any.
// The state file is created lazily so commands that never open a picker never
// leave one behind.
func (p *agentPick) fzfArgs(hint string) []string {
	if p == nil {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil // no binary to shell back into; run the picker without the bar
	}
	if p.statePath == "" {
		f, err := os.CreateTemp("", "rig-agent-*")
		if err != nil {
			return nil
		}
		p.statePath = f.Name()
		_ = f.Close()
	}
	if err := os.WriteFile(p.statePath, []byte(string(p.kind)+"\n"), 0o600); err != nil {
		return nil
	}
	p.shown = true

	// transform-header replaces the header wholesale, so the hint travels to the
	// cycle command as an argument rather than living in a separate fzf flag it
	// would clobber. Neither argument can contain parens, which is what fzf's
	// action-argument parser balances on.
	cycle := fmt.Sprintf("%s __agent cycle %s %s", shellQuote(exe), shellQuote(p.statePath), shellQuote(hint))
	return []string{
		"--header=" + agentHeader(p.kind, hint),
		"--bind=" + agentCycleKey + ":transform-header(" + cycle + ")",
	}
}

// sync pulls back whatever the picker's cycles left in the state file. A
// missing or garbled file leaves the current choice alone: a broken temp file
// should cost you the keystroke, not the rig.
func (p *agentPick) sync() {
	if p == nil || p.statePath == "" {
		return
	}
	blob, err := os.ReadFile(p.statePath)
	if err != nil {
		return
	}
	if kind, err := parseAgent(strings.TrimSpace(string(blob))); err == nil {
		p.kind = kind
	}
}

func (p *agentPick) cleanup() {
	if p == nil || p.statePath == "" {
		return
	}
	_ = os.Remove(p.statePath)
	p.statePath = ""
}

// ensurePicked is the fallback for a command that prompted for nothing: with
// no prompt to ride along on, the bar stands alone rather than letting a fully
// specified invocation silently take the default. Returns false if you bailed.
// A --agent flag, a non-interactive stdin, or any prompt that already carried
// the bar all skip it.
func (p *agentPick) ensurePicked() (bool, error) {
	if p == nil || p.explicit || p.shown || !stdinIsTTY() {
		return true, nil
	}
	result, err := tea.NewProgram(agentPromptModel{kind: p.kind}, tea.WithInput(os.Stdin), tea.WithOutput(os.Stderr)).Run()
	if err != nil {
		return false, fmt.Errorf("reading agent choice: %w", err)
	}
	final := result.(agentPromptModel)
	if final.cancelled {
		return false, nil
	}
	p.kind = final.kind
	p.shown = true
	return true, nil
}

// agentPromptModel is the standalone bar. It answers to the same ctrl-o as the
// footers do, plus arrows and tab, because when the bar is the whole screen
// it's the thing you're aiming at rather than an aside on someone else's prompt.
type agentPromptModel struct {
	kind      agentKind
	cancelled bool
	done      bool
}

func (m agentPromptModel) Init() tea.Cmd { return nil }

func (m agentPromptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch {
	case key.Type == tea.KeyEnter:
		m.done = true
		return m, tea.Quit
	case key.Type == tea.KeyEsc, key.Type == tea.KeyCtrlC:
		m.cancelled = true
		m.done = true
		return m, tea.Quit
	case key.Type == tea.KeyLeft:
		m.kind = m.kind.prev()
	case isAgentCycleKey(key), key.Type == tea.KeyRight, key.Type == tea.KeyTab:
		m.kind = m.kind.next()
	}
	return m, nil
}

func (m agentPromptModel) View() string {
	if m.done {
		return ""
	}
	return agentBar(m.kind) + " · enter: go · esc: cancel\n"
}

// isAgentCycleKey recognizes the cycle key in a Bubble Tea prompt. Callers
// check it before handing the message to their text component, which would
// otherwise act on the key (or swallow it) first.
func isAgentCycleKey(key tea.KeyMsg) bool {
	return key.Type == tea.KeyCtrlO
}

// runAgentPickCmd backs the hidden `rig __agent` subcommand that fzf's
// transform-header shells out to. It advances the choice in the state file and
// prints the redrawn header block.
func runAgentPickCmd(args []string) error {
	if len(args) < 2 || args[0] != "cycle" {
		return fmt.Errorf("usage: rig __agent cycle STATEFILE [HINT]")
	}
	hint := ""
	if len(args) > 2 {
		hint = strings.Join(args[2:], " ")
	}
	header, err := cycleAgentState(args[1], hint)
	if err != nil {
		return err
	}
	fmt.Println(header)
	return nil
}

// cycleAgentState advances the choice recorded at path and returns the header
// block fzf should redraw.
func cycleAgentState(path, hint string) (string, error) {
	kind := agentClaude
	if blob, err := os.ReadFile(path); err == nil {
		if k, err := parseAgent(strings.TrimSpace(string(blob))); err == nil {
			kind = k
		}
	}
	kind = kind.next()
	if err := os.WriteFile(path, []byte(string(kind)+"\n"), 0o600); err != nil {
		return "", err
	}
	return agentHeader(kind, hint), nil
}
