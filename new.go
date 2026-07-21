package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// runNew starts work that does not have a tracker identity yet. The kickoff is
// both the task's human title and the seed for its local identity; everything
// after that joins the ordinary authoring-rig path, except there is no branch
// hint to record. Like a repo added later with `rig add`, the workspace starts
// at trunk() and branch discovery takes over once the work grows a bookmark.
func runNew(args []string) error {
	agent, args, err := extractAgentFlag(args)
	if err != nil {
		return err
	}
	repoFlag, args := extractRepoFlag(args)

	kickoff, err := resolveKickoff(args)
	if err != nil {
		return err
	}
	if kickoff == "" {
		return nil // empty interactive prompt means never mind
	}

	rigID := kickoffID(kickoff)
	if rigID == "" {
		return fmt.Errorf("kickoff does not produce a usable local name")
	}
	basedir, err := basedirPath(rigID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(basedir); err == nil {
		if _, manifestErr := os.Stat(filepath.Join(basedir, manifestName)); manifestErr == nil {
			return fmt.Errorf("rig %q already exists; run `rig switch %s` (or `rig wake %s` if parked), or use a different kickoff", rigID, rigID, rigID)
		}
		return fmt.Errorf("basedir already exists: %s", basedir)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking basedir: %w", err)
	}

	repo, err := resolveRepo(repoFlag)
	if err != nil {
		return err
	}
	if repo.Path == "" {
		return nil // repo picker cancelled
	}
	if err := ensureJJColocated(repo.Path); err != nil {
		return fmt.Errorf("colocating jj on %s: %w", repo.Path, err)
	}

	m := manifest{ID: rigID, Title: kickoff, Agent: string(agent)}
	if err := createBasedir(basedir, m); err != nil {
		return err
	}

	repoDest, err := addRepoWorkspace(basedir, rigID, repo, "trunk()", "")
	if err != nil {
		return err
	}

	sess := sessionSpec{
		rectoCmd: "recto",
		repo:     repo.Name,
		agent:    agent,
		prompt: fmt.Sprintf(
			"Starting unticketed work with this kickoff: %s. There is no issue to read yet. Help me investigate and decide what, if anything, should be ticketed.",
			kickoff,
		),
	}
	session, err := spawnSession(basedir, repoDest, sess)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "rig: new %s — %s\n", rigID, basedir)
	return attachOrReport(session)
}

// resolveKickoff accepts an inline kickoff (`rig new investigate the flake`)
// or asks for one when run interactively with no positional arguments. The
// inline form keeps scripts and agent shells usable without pretending a pipe
// can answer a terminal prompt.
func resolveKickoff(args []string) (string, error) {
	if len(args) > 0 {
		return strings.TrimSpace(strings.Join(args, " ")), nil
	}
	if !stdinIsTTY() {
		return "", fmt.Errorf("Kickoff prompt needs an interactive terminal; pass it inline, e.g. `rig new investigate the flake`")
	}

	m := newKickoffPromptModel()
	result, err := tea.NewProgram(m, tea.WithInput(os.Stdin), tea.WithOutput(os.Stderr)).Run()
	if err != nil {
		return "", fmt.Errorf("reading kickoff: %w", err)
	}
	return result.(kickoffPromptModel).kickoff, nil
}

type kickoffPromptModel struct {
	input   textinput.Model
	kickoff string
	done    bool
}

func newKickoffPromptModel() kickoffPromptModel {
	input := textinput.New()
	input.Prompt = "Kickoff: "
	input.Placeholder = "What are we working on?"
	input.Width = 72
	input.Focus()
	return kickoffPromptModel{input: input}
}

func (m kickoffPromptModel) Init() tea.Cmd { return textinput.Blink }

func (m kickoffPromptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEnter:
			m.kickoff = strings.TrimSpace(m.input.Value())
			m.done = true
			return m, tea.Quit
		case tea.KeyEsc, tea.KeyCtrlC:
			m.done = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.input.Width = max(20, msg.Width-len(m.input.Prompt))
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m kickoffPromptModel) View() string {
	if m.done {
		return ""
	}
	return m.input.View()
}

// kickoffID turns the free-form kickoff into the stable identity used by the
// manifest, basedir, jj workspace, and tmux session. Keep the same 60-character
// ceiling as tracker-derived task slugs so local work does not grow a noisier
// filesystem shape than ticketed work.
func kickoffID(kickoff string) string {
	const maxLen = 60
	id := slugify(kickoff)
	if len(id) <= maxLen {
		return id
	}

	words := compactKickoffWords(strings.Split(id, "-"))
	if compact := strings.Join(words, "-"); len(compact) <= maxLen {
		return compact
	}
	return fitKickoffWords(words, maxLen)
}

var kickoffNoise = func() map[string]struct{} {
	words := strings.Fields(`
		a actually an and are as at based be been being better but by can could
		did do does e eg for from g had has have how i if in into is it its just
		little may maybe might occasionally of on or our please really s should so
		somehow than that the their then there these this those to up was we were
		what when where whether which why will with would you your
	`)
	set := make(map[string]struct{}, len(words))
	for _, word := range words {
		set[word] = struct{}{}
	}
	return set
}()

// compactKickoffWords removes conversational scaffolding from a long kickoff.
// It is deliberately a tiny local heuristic: naming a workspace should never
// wait on the network or require one of the supported agents to be installed.
func compactKickoffWords(words []string) []string {
	compacted := make([]string, 0, len(words))
	for _, word := range words {
		if _, noisy := kickoffNoise[word]; !noisy {
			compacted = append(compacted, word)
		}
	}
	if len(compacted) == 0 {
		return words
	}

	// These verbs usually introduce the real subject rather than distinguish
	// one workspace from another. Only discard one at the beginning.
	if len(compacted) > 1 {
		switch compacted[0] {
		case "explore", "figure", "investigate", "look", "understand":
			compacted = compacted[1:]
		}
	}
	return compacted
}

// fitKickoffWords keeps the subject at the beginning and reserves room for
// the outcome at the end. A plain prefix tends to lose the useful "so X works"
// part of conversational kickoffs.
func fitKickoffWords(words []string, maxLen int) string {
	if len(words) == 0 || maxLen <= 0 {
		return ""
	}
	if len(words) == 1 {
		return strings.TrimRight(words[0][:min(len(words[0]), maxLen)], "-")
	}

	tailCount := min(3, len(words)-1)
	for tailCount > 1 && len(strings.Join(words[len(words)-tailCount:], "-")) > maxLen/2 {
		tailCount--
	}
	tail := words[len(words)-tailCount:]
	tailLen := len(strings.Join(tail, "-"))
	headBudget := maxLen - tailLen - 1

	head := make([]string, 0, len(words)-tailCount)
	headLen := 0
	for _, word := range words[:len(words)-tailCount] {
		nextLen := len(word)
		if len(head) > 0 {
			nextLen++
		}
		if headLen+nextLen > headBudget {
			break
		}
		head = append(head, word)
		headLen += nextLen
	}
	if len(head) == 0 {
		return strings.TrimRight(strings.Join(tail, "-")[:min(tailLen, maxLen)], "-")
	}
	return strings.Join(append(head, tail...), "-")
}
