package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// newRigModel is the one interactive `rig new` flow. The standalone command
// wraps it in a Bubble Tea program; the radar delegates to the same model while
// it is in new-rig mode. Keeping the model ignorant of its host is what lets Esc
// mean "cancel this wizard" without deciding whether that should close a CLI
// program or return to the radar board.
type newRigModel struct {
	phase newRigPhase

	kickoffInput textinput.Model
	contextArea  textarea.Model
	repoInput    textinput.Model

	target      newRigTarget
	context     string
	repoFlag    string
	repos       []repoRef
	reposLoaded bool
	cursor      int
	agent       agentKind

	width  int
	height int
	err    error

	result    newRigResult
	cancelled bool
	done      bool
}

type newRigPhase int

const (
	newRigKickoff newRigPhase = iota
	newRigContext
	newRigRepo
	newRigCreating
	newRigFailed
)

type newRigReposMsg struct {
	repos []repoRef
	err   error
}

type newRigCreatedMsg struct {
	result newRigResult
	err    error
}

func newRigWizardModel(kickoff, repoFlag string, agent agentKind) (newRigModel, error) {
	kickoffInput := textinput.New()
	kickoffInput.Prompt = "Kickoff: "
	kickoffInput.Placeholder = "What are we working on?"
	kickoffInput.Width = 72
	kickoffInput.Focus()

	contextArea := textarea.New()
	contextArea.Placeholder = "Paste anything else worth knowing…"
	contextArea.ShowLineNumbers = false
	contextArea.MaxHeight = 0
	contextArea.SetWidth(76)
	contextArea.SetHeight(10)

	repoInput := textinput.New()
	repoInput.Prompt = "/ "
	repoInput.Placeholder = "Find a repo"
	repoInput.Width = 72

	m := newRigModel{
		phase:        newRigKickoff,
		kickoffInput: kickoffInput,
		contextArea:  contextArea,
		repoInput:    repoInput,
		repoFlag:     repoFlag,
		agent:        agent,
	}
	if kickoff = strings.TrimSpace(kickoff); kickoff != "" {
		target, err := prepareNewRig(kickoff)
		if err != nil {
			return newRigModel{}, err
		}
		m.target = target
		m.phase = newRigContext
		m.kickoffInput.Blur()
		m.contextArea.Focus()
	}
	return m, nil
}

func (m newRigModel) Init() tea.Cmd {
	switch m.phase {
	case newRigContext:
		return textarea.Blink
	default:
		return textinput.Blink
	}
}

// update advances the host-neutral wizard. It never returns tea.Quit: the CLI
// wrapper and radar decide independently what a finished or cancelled wizard
// means for their containing UI.
func (m newRigModel) update(msg tea.Msg) (newRigModel, tea.Cmd) {
	switch msg := msg.(type) {
	case newRigReposMsg:
		m.err = msg.err
		m.repos = msg.repos
		m.reposLoaded = true
		m.cursor = 0
		return m, nil
	case newRigCreatedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.phase = newRigFailed
			return m, nil
		}
		m.result = msg.result
		m.done = true
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m.updateComponent(msg)
	}
	if m.phase != newRigCreating && key.Type == tea.KeyCtrlC {
		m.cancelled = true
		m.done = true
		return m, nil
	}
	if m.phase != newRigCreating && m.phase != newRigFailed && isAgentCycleKey(key) {
		m.agent = m.agent.next()
		return m, nil
	}

	switch m.phase {
	case newRigKickoff:
		switch key.Type {
		case tea.KeyEnter:
			target, err := prepareNewRig(m.kickoffInput.Value())
			if err != nil {
				m.err = err
				return m, nil
			}
			m.err = nil
			m.target = target
			m.phase = newRigContext
			m.kickoffInput.Blur()
			m.contextArea.Focus()
			return m, textarea.Blink
		case tea.KeyEsc:
			m.cancelled = true
			m.done = true
			return m, nil
		}

	case newRigContext:
		if key.Paste {
			m.contextArea.InsertString(strings.ReplaceAll(string(key.Runes), "\r\n", "\n"))
			return m, nil
		}
		switch key.Type {
		case tea.KeyCtrlD:
			m.context = strings.TrimSpace(m.contextArea.Value())
			return m.beginRepo()
		case tea.KeyEsc:
			m.context = ""
			return m.beginRepo()
		}

	case newRigRepo:
		switch key.String() {
		case "esc":
			m.cancelled = true
			m.done = true
			return m, nil
		case "down":
			if rows := m.repoRows(); m.cursor < len(rows)-1 {
				m.cursor++
			}
			return m, nil
		case "up":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "enter":
			rows := m.repoRows()
			if len(rows) == 0 {
				return m, nil
			}
			return m.beginCreate(rows[m.cursor])
		case "ctrl+u":
			m.repoInput.SetValue("")
			m.cursor = 0
			return m, nil
		case "backspace":
			m.cursor = 0
		case "ctrl+r":
			m.err = nil
			m.repos = nil
			m.reposLoaded = false
			m.cursor = 0
			return m, discoverNewReposCmd()
		default:
			if key.Type == tea.KeyRunes {
				m.cursor = 0
			}
		}

	case newRigFailed:
		if key.Type == tea.KeyEsc || key.Type == tea.KeyEnter {
			m.cancelled = true
			m.done = true
		}
		return m, nil

	case newRigCreating:
		return m, nil
	}

	return m.updateComponent(msg)
}

func (m newRigModel) updateComponent(msg tea.Msg) (newRigModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.phase {
	case newRigKickoff:
		m.kickoffInput, cmd = m.kickoffInput.Update(msg)
	case newRigContext:
		m.contextArea, cmd = m.contextArea.Update(msg)
	case newRigRepo:
		m.repoInput, cmd = m.repoInput.Update(msg)
	}
	return m, cmd
}

func (m newRigModel) beginRepo() (newRigModel, tea.Cmd) {
	m.contextArea.Blur()
	m.err = nil
	if m.repoFlag != "" {
		m.phase = newRigCreating
		return m, createNewRigCmd(m.target, m.context, repoRef{}, m.repoFlag, m.agent)
	}
	m.phase = newRigRepo
	m.repoInput.Focus()
	m.reposLoaded = false
	return m, tea.Batch(textinput.Blink, discoverNewReposCmd())
}

func (m newRigModel) beginCreate(repo repoRef) (newRigModel, tea.Cmd) {
	m.phase = newRigCreating
	m.repoInput.Blur()
	m.err = nil
	return m, createNewRigCmd(m.target, m.context, repo, "", m.agent)
}

func discoverNewReposCmd() tea.Cmd {
	return func() tea.Msg {
		cwd, cwdErr := detectPrimaryRepo()
		var cwdRepo *repoRef
		if cwdErr == nil {
			cwdRepo = &cwd
		}
		ghq, err := ghqRepos()
		if err != nil && cwdRepo == nil {
			return newRigReposMsg{err: err}
		}
		repos := repoCandidates(cwdRepo, ghq)
		if len(repos) == 0 {
			return newRigReposMsg{err: fmt.Errorf("no repos found via ghq")}
		}
		return newRigReposMsg{repos: repos}
	}
}

func createNewRigCmd(target newRigTarget, context string, repo repoRef, repoFlag string, agent agentKind) tea.Cmd {
	return func() tea.Msg {
		if repoFlag != "" {
			resolved, err := resolveRepo(repoFlag, nil)
			if err != nil {
				return newRigCreatedMsg{err: err}
			}
			repo = resolved
		}
		result, err := createNewRig(target, context, repo, agent)
		return newRigCreatedMsg{result: result, err: err}
	}
}

func (m newRigModel) repoRows() []repoRef {
	query := strings.TrimSpace(m.repoInput.Value())
	if query == "" {
		return m.repos
	}
	type scored struct {
		repo  repoRef
		score float64
	}
	var matches []scored
	for _, repo := range m.repos {
		hay := repo.nameWithOwner() + " " + repo.Path
		if score, ok := fuzzyScore(query, hay); ok {
			matches = append(matches, scored{repo, score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	rows := make([]repoRef, len(matches))
	for i, match := range matches {
		rows[i] = match.repo
	}
	return rows
}

func (m *newRigModel) resize() {
	width := max(20, m.width-4)
	m.kickoffInput.Width = max(20, width-len(m.kickoffInput.Prompt))
	m.repoInput.Width = max(20, width-len(m.repoInput.Prompt))
	m.contextArea.SetWidth(width)
	if m.height > 0 {
		m.contextArea.SetHeight(min(max(m.height-9, 6), 20))
	}
}

func (m newRigModel) View() string {
	if m.done {
		return ""
	}
	var body string
	var footer string
	switch m.phase {
	case newRigKickoff:
		body = m.kickoffInput.View()
		footer = "enter continue · esc cancel"
	case newRigContext:
		body = fmt.Sprintf("Kickoff: %s\n\nContext (optional):\n%s", m.clippedKickoff(len("Kickoff: ")), m.contextArea.View())
		footer = "ctrl-d continue · esc skip · ctrl-c cancel"
	case newRigRepo:
		body = m.repoView()
		footer = "type to find · ↑/↓ move · enter choose · esc cancel · ctrl-r reload"
	case newRigCreating:
		body = fmt.Sprintf("Creating %s\n\n  kickoff  %s\n  agent    %s", m.target.ID, m.clippedKickoff(len("  kickoff  ")), m.agent.short())
		footer = "setting up workspace and session…"
	case newRigFailed:
		body = "Could not create the rig."
		footer = "enter or esc close"
	}
	if m.err != nil {
		body += "\n\n" + radarErrStyle.Render(m.err.Error())
	}
	if m.phase != newRigCreating && m.phase != newRigFailed {
		body += "\n\n" + agentBar(m.agent)
	}
	return "\n" + body + "\n\n" + radarFaintStyle.Render(footer) + "\n"
}

func (m newRigModel) repoView() string {
	rows := m.repoRows()
	var lines []string
	if len(rows) == 0 {
		switch {
		case !m.reposLoaded:
			lines = append(lines, radarFaintStyle.Render("  loading repos…"))
		case m.err != nil:
			lines = append(lines, radarFaintStyle.Render("  no repos"))
		default:
			lines = append(lines, radarFaintStyle.Render("  no matches"))
		}
	} else {
		start, end := 0, len(rows)
		if m.height > 0 {
			budget := max(1, m.height-10)
			start, end = windowBody(len(rows), m.cursor, budget)
		}
		for i, repo := range rows[start:end] {
			name := repo.nameWithOwner()
			path := tildePath(repo.Path, homeDir())
			nameWidth := 32
			if m.width > 0 {
				available := max(20, m.width-4)
				nameWidth = min(nameWidth, max(10, available/2))
				name = radarTruncate(name, nameWidth)
				path = collapsePath(path, max(8, available-nameWidth-1))
			}
			line := fmt.Sprintf("  %-*s %s", nameWidth, name, path)
			if start+i == m.cursor {
				line = radarCursorStyle.Render(line)
			}
			lines = append(lines, line)
		}
	}
	return "Pick repo\n\n" + m.repoInput.View() + "\n\n" + strings.Join(lines, "\n")
}

func (m newRigModel) clippedKickoff(prefix int) string {
	if m.width <= 0 {
		return m.target.Kickoff
	}
	return radarTruncate(m.target.Kickoff, max(8, m.width-prefix-2))
}

func homeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

// newRigProgram supplies the one host-specific behavior the standalone CLI
// needs: once the shared model finishes, release Bubble Tea's alternate screen.
type newRigProgram struct{ newRigModel }

func (p newRigProgram) Init() tea.Cmd { return p.newRigModel.Init() }

func (p newRigProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := p.newRigModel.update(msg)
	p.newRigModel = m
	if m.done {
		return p, tea.Quit
	}
	return p, cmd
}

func (p newRigProgram) View() string { return p.newRigModel.View() }

func runNewRigTUI(m newRigModel) (newRigModel, error) {
	result, err := tea.NewProgram(
		newRigProgram{m},
		tea.WithInput(os.Stdin),
		tea.WithOutput(os.Stderr),
		tea.WithAltScreen(),
	).Run()
	if err != nil {
		return newRigModel{}, fmt.Errorf("running new-rig wizard: %w", err)
	}
	return result.(newRigProgram).newRigModel, nil
}
