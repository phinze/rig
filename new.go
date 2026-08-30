package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// runNew starts work that does not have a tracker identity yet. The kickoff is
// both the task's human title and the seed for its local identity; everything
// after that joins the ordinary authoring-rig path, except there is no branch
// hint to record. Like a repo added later with `rig add`, the workspace starts
// at trunk() and branch discovery takes over once the work grows a bookmark.
func runNew(args []string) error {
	pick, args, err := extractAgentFlag(args)
	if err != nil {
		return err
	}
	defer pick.cleanup()
	repoFlag, args := extractRepoFlag(args)

	// A pipe is the scriptable shape: the positional kickoff and stdin context
	// already answer every text prompt, and an explicit repo or cwd supplies the
	// last choice. Interactive invocations all use the same Bubble Tea wizard
	// the radar embeds, so the two entry points cannot grow different controls.
	if !stdinIsTTY() {
		kickoff, err := resolveKickoff(args)
		if err != nil {
			return err
		}
		target, err := prepareNewRig(kickoff)
		if err != nil {
			return err
		}
		context, err := resolveKickoffContext()
		if err != nil {
			return err
		}
		repo, err := resolveRepo(repoFlag, pick)
		if err != nil {
			return err
		}
		result, err := createNewRig(target, context, repo, pick.kind)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "rig: new %s — %s\n", result.ID, result.Basedir)
		return attachOrReport(result.Session)
	}

	kickoff := strings.TrimSpace(strings.Join(args, " "))
	m, err := newRigWizardModel(kickoff, repoFlag, pick.kind)
	if err != nil {
		return err
	}
	final, err := runNewRigTUI(m)
	if err != nil {
		return err
	}
	if final.cancelled || final.result.Session == "" {
		if final.err != nil {
			return final.err
		}
		return nil
	}
	fmt.Fprintf(os.Stderr, "rig: new %s — %s\n", final.result.ID, final.result.Basedir)
	return attachOrReport(final.result.Session)
}

// newRigTarget is the stable identity derived from the kickoff. Preparing it
// before the context step preserves an important bit of the CLI flow: a large
// blob you just pasted is never discarded by a collision discovered afterward.
type newRigTarget struct {
	ID      string
	Kickoff string
	Basedir string
}

type newRigResult struct {
	ID      string
	Basedir string
	Session string
}

func prepareNewRig(kickoff string) (newRigTarget, error) {
	kickoff = strings.TrimSpace(kickoff)
	rigID := kickoffID(kickoff)
	if rigID == "" {
		return newRigTarget{}, fmt.Errorf("kickoff does not produce a usable local name")
	}
	basedir, err := basedirPath(rigID)
	if err != nil {
		return newRigTarget{}, err
	}
	if _, err := os.Stat(basedir); err == nil {
		if _, manifestErr := findManifestPath(basedir); manifestErr == nil {
			return newRigTarget{}, fmt.Errorf("rig %q already exists; run `rig switch %s` (or `rig wake %s` if parked), or use a different kickoff", rigID, rigID, rigID)
		}
		return newRigTarget{}, fmt.Errorf("basedir already exists: %s", basedir)
	} else if !os.IsNotExist(err) {
		return newRigTarget{}, fmt.Errorf("checking basedir: %w", err)
	}
	return newRigTarget{ID: rigID, Kickoff: kickoff, Basedir: basedir}, nil
}

// createNewRig is the non-UI transaction shared by the standalone wizard, the
// radar-embedded wizard, and piped invocations. It deliberately stops at a
// ready session; the caller owns the final attach or switch once its UI has
// released the terminal.
func createNewRig(target newRigTarget, context string, repo repoRef, agent agentKind) (newRigResult, error) {
	// Check again at commit time. The wizard may have sat open for minutes after
	// its first preflight, and a second process could have claimed the slug.
	fresh, err := prepareNewRig(target.Kickoff)
	if err != nil {
		return newRigResult{}, err
	}
	target = fresh

	if err := ensureJJColocated(repo.Path); err != nil {
		return newRigResult{}, fmt.Errorf("colocating jj on %s: %w", repo.Path, err)
	}

	m := manifest{ID: target.ID, Title: target.Kickoff, Agent: string(agent), MainRepo: repo.Name}
	if err := createBasedir(target.Basedir, m); err != nil {
		return newRigResult{}, err
	}

	// Before addRepoWorkspace, which regenerates the agent instructions: they
	// grow a pointer bullet when the kickoff file is on disk.
	context = strings.TrimSpace(context)
	if context != "" {
		if err := writeRigKickoff(target.Basedir, target.Kickoff, context); err != nil {
			return newRigResult{}, fmt.Errorf("writing %s: %w", rigKickoffName, err)
		}
	}

	repoDest, err := addRepoWorkspace(target.Basedir, target.ID, repo, "trunk()", "")
	if err != nil {
		return newRigResult{}, err
	}

	sess := sessionSpec{
		rectoCmd: rectoCommand(),
		repo:     repo.Name,
		agent:    agent,
		prompt:   kickoffPrompt(target.Kickoff, context != ""),
	}
	session, err := spawnSession(target.Basedir, repoDest, sess)
	if err != nil {
		return newRigResult{}, err
	}
	return newRigResult{ID: target.ID, Basedir: target.Basedir, Session: session}, nil
}

// kickoffPrompt is the opening message the agent wakes up to. Pasted context
// is handed over as a path rather than inlined: the prompt reaches the agent by
// being typed into its shell (spawnSession → tmux send-keys), which is a poor
// courier for a multi-line blob, and a file survives the context window, a
// resumed session, and whatever other agent the rig grows later.
func kickoffPrompt(kickoff string, hasContext bool) string {
	brief := "That's the whole brief so far — there's no ticket to read."
	if hasContext {
		brief = fmt.Sprintf("There's no ticket to read, but I pasted a blob of context into ../%s — start there.", rigKickoffName)
	}
	return fmt.Sprintf(
		"This is your kickoff for a new rig: %s. %s If it gives you enough to start some introductory analysis or sketch out next steps, go ahead and dig in, then show me what you find. If it's too thin to act on well, ask me to elaborate before diving in.",
		kickoff, brief,
	)
}

// resolveKickoffContext is the non-interactive half of context collection.
// Interactive callers use newRigModel; a pipe supplies all of stdin here.
func resolveKickoffContext() (string, error) {
	blob, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("reading kickoff context from stdin: %w", err)
	}
	return strings.TrimSpace(string(blob)), nil
}

// resolveKickoff reads the positional kickoff used by piped invocations such as
// `pbpaste | rig new investigate the flake`. Interactive callers use
// newRigModel instead.
func resolveKickoff(args []string) (string, error) {
	if len(args) > 0 {
		return strings.TrimSpace(strings.Join(args, " ")), nil
	}
	return "", fmt.Errorf("Kickoff prompt needs an interactive terminal; pass it inline, e.g. `rig new investigate the flake`")
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
