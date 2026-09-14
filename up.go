package main

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"strings"
)

func runUp(args []string) error {
	pick, args, err := extractAgentFlag(args)
	if err != nil {
		return err
	}
	defer pick.cleanup()
	repoFlag, args := extractRepoFlag(args)
	contextFlag, args := extractContextFlag(args)
	source, args, err := extractSourceFlag(args)
	if err != nil {
		return err
	}

	// A PR URL means "pick up my own work on this PR" — authoring, not the issue
	// flow. pickupPR sorts authoring vs review by who owns the PR (and reroutes
	// to review if it's not yours). Idempotency lives inside the authoring pickup,
	// keyed off the linked Linear issue or its branch fallback (a PR born from an
	// issue shares that issue's rig), which is why there's no cheap pr-<n>
	// pre-check here: pr-<n> is unique only per repo, and wouldn't match an
	// issue-keyed rig anyway.
	if len(args) >= 1 {
		if pr := parsePRURL(args[0]); pr != nil {
			if contextFlag != "" {
				fmt.Fprintln(os.Stderr, "rig: warning: --context is for issue pickups; a PR pickup resumes existing work and ignores it")
			}
			return pickupPR(pr, "up", pick)
		}
	}

	ref, err := resolveIssueID(args, pick, source)
	if err != nil {
		return err
	}
	if ref.id == "" {
		return nil // picker cancelled
	}
	context, err := resolveUpContext(contextFlag)
	if err != nil {
		return err
	}

	// Idempotency: `rig up X` means "put me in my rig for X, making it if it
	// isn't there." If a rig for this task already exists, go to it rather than
	// try to build a second one and error on the existing basedir. The check is
	// local (listRigs, no tracker call), so re-upping into work you already have
	// stays as instant as `rig switch`; the network only gets touched below, on
	// a genuine create.
	done, unfinished, err := attachExistingRig(ref.rigID())
	if err != nil {
		return err
	}
	if done {
		// Idempotency wins over the context: the rig exists and we went there.
		// But dropping the color silently is how a project agent ends up
		// believing it briefed a task agent that never heard a word, so say so
		// and name the command that does reach a running rig.
		if context != "" {
			fmt.Fprintf(os.Stderr, "rig: warning: %s already exists, so the context was not delivered; `rig dispatch %s <prompt>` hands a prompt to its agent\n", ref.id, ref.id)
		}
		return nil
	}

	tk, err := resolveTask(ref)
	if err != nil {
		return err
	}
	// A GitHub issue already says which repo it's for, so that question isn't
	// asked again; --repo still overrides, for the issue whose fix lands
	// somewhere else.
	repoFlag = cmp.Or(repoFlag, tk.Repo)

	// Finishing an interrupted create means walking the create path again, but
	// you already answered the repo question the first time and the wreck
	// recorded the answer. Reuse it rather than re-asking, and say which one, so
	// a resume never silently picks a repo on your behalf. An explicit --repo
	// still wins: naming one is a deliberate override, and it's also the way to
	// change your mind without tearing the rig down.
	if unfinished != nil {
		repoFlag = cmp.Or(repoFlag, unfinished.Building)
		fmt.Fprintf(os.Stderr, "rig: %s was left half-built — finishing it with %s\n",
			unfinished.ID, repoFlag)
	}

	repo, err := resolveRepo(repoFlag, pick)
	if err != nil {
		return err
	}
	if repo.Path == "" {
		return nil // repo picker cancelled
	}

	// Nothing prompted (an exact id plus --repo) means the agent bar hasn't been
	// on screen yet, so it gets its own turn before we commit the choice.
	if ok, err := pick.ensurePicked(); err != nil || !ok {
		return err
	}

	basedir, err := basedirPath(tk.basedirName())
	if err != nil {
		return err
	}

	if err := ensureJJColocated(repo.Path); err != nil {
		return fmt.Errorf("colocating jj on %s: %w", repo.Path, err)
	}

	m := manifest{
		ID: tk.rigID(), Title: tk.Title, Agent: string(pick.kind), MainRepo: repo.Name,
		Tracker: string(tk.source()), TrackerID: tk.Identifier, TrackerURL: tk.URL,
		BuildingRepo: repo.nameWithOwner(),
	}
	if err := createBasedir(basedir, m); err != nil {
		return err
	}
	if context != "" {
		if err := writeRigKickoff(basedir, tk.Identifier+": "+tk.Title, context); err != nil {
			return fmt.Errorf("writing %s: %w", rigKickoffName, err)
		}
	}

	branchName := tk.workBranchName()
	startRev := "trunk()"
	if tk.source() == sourceLinear {
		// Existing work may predate keyword-controlled Linear linking and still
		// ride the generated, issue-bearing branch. Resume it when found; only
		// use the new branch shape when there is no old work to recover.
		if startRev = resolveStartRev(repo.Path, tk.BranchName); startRev != "trunk()" {
			branchName = tk.BranchName
		}
	}
	if startRev == "trunk()" {
		startRev = resolveStartRev(repo.Path, branchName)
	}
	// Record the intended branch even when startRev fell back to trunk() because
	// it isn't pushed yet, so pr/ls/reap resolve the right PR once it exists.
	repoDest, err := addRepoWorkspace(basedir, tk.rigID(), repo, startRev, branchName)
	if err != nil {
		return err
	}

	// Layout: recto on the right, the selected agent on the left with an issue-pickup
	// prompt phrased for wherever the issue lives.
	sess := sessionSpec{
		rectoCmd: rectoCommand(),
		repo:     repo.Name,
		agent:    pick.kind,
		prompt:   pickupPrompt(tk, context != ""),
	}
	session, err := spawnSession(basedir, repoDest, sess)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "rig: up %s — %s\n", tk.Identifier, basedir)
	return attachOrReport(session)
}

// pickupPrompt is the opening message for an issue pickup. Each source names
// the tool the agent should read the issue with, since none of them is
// discoverable from the rig alone: Linear by its MCP, a GitHub issue by the
// rig's own gh shim (which already points at the repo), a personal task by the
// `personal-tasks` helper and skill. Extra context is handed over as a path
// rather than inlined, for the same reasons kickoffPrompt gives: the prompt
// reaches the agent as one shell argument, which is a poor courier for a
// multi-line blob, and a file survives a resume and whatever other agent the
// rig grows later.
func pickupPrompt(tk task, hasContext bool) string {
	var prompt string
	switch tk.source() {
	case sourceGitHub:
		number := "<n>"
		if ref := parseGitHubIssueRef(tk.Identifier); ref != nil {
			number = fmt.Sprint(ref.Number)
		}
		prompt = fmt.Sprintf(
			"Picking up GitHub issue %s (%s). Read it and its comments with `gh issue view %s --comments`, then help me plan.",
			tk.Identifier, tk.Title, number,
		)
	case sourceVikunja:
		prompt = fmt.Sprintf(
			"Picking up personal task %s (%s). Use the personal-tasks skill to read it (`personal-tasks %s show %s`) and claim it, then help me plan.",
			tk.Identifier, tk.Title, tk.Domain, tk.Identifier,
		)
	default:
		prompt = fmt.Sprintf(
			"Picking up %s (%s). Use the Linear MCP (it may take a few seconds to connect) to read the issue, mark it In Progress and assigned to me, then help me plan.",
			tk.Identifier, tk.Title,
		)
	}
	if hasContext {
		prompt += fmt.Sprintf(" There's extra context for this pickup in ../%s; read it alongside the issue, it may narrow or redirect what the ticket says.", rigKickoffName)
	}
	return prompt
}

// resolveUpContext gathers the color a pickup can carry beyond the ticket: a
// --context flag, piped stdin, or both (flag first). It exists for a project
// rig's agent starting a task rig on your behalf, which knows things the ticket
// doesn't and has no other channel to the task agent until it's running. A
// terminal on stdin means nothing was piped, so only the flag counts.
func resolveUpContext(flag string) (string, error) {
	parts := []string{strings.TrimSpace(flag)}
	if !stdinIsTTY() {
		blob, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading pickup context from stdin: %w", err)
		}
		parts = append(parts, strings.TrimSpace(string(blob)))
	}
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n"), nil
}

// extractRepoFlag pulls a --repo owner/repo (or --repo=owner/repo) out of args,
// returning its value and the remaining args. `rig up` mixes the flag in with
// its issue-id / query / PR-url positional, so we strip it before dispatching on
// what's left. A repeated flag keeps the last; a trailing bare --repo with no
// value is left in rest, where resolveRepo's empty-override path treats it as
// "no override" and falls through to the picker.
func extractRepoFlag(args []string) (repo string, rest []string) {
	return extractValueFlag(args, "--repo")
}

// extractContextFlag pulls --context <text> (or --context=text) out of args.
func extractContextFlag(args []string) (context string, rest []string) {
	return extractValueFlag(args, "--context")
}

// extractSourceFlag pulls --source <name> out of args. It names the tracker an
// exact id belongs to and seeds the picker's starting source; absent, an exact
// `TEAM-123` id is routed by its prefix and the picker opens on Linear.
func extractSourceFlag(args []string) (taskSource, []string, error) {
	name, rest := extractValueFlag(args, "--source")
	if name == "" {
		return "", rest, nil
	}
	source, err := parseTaskSource(name)
	if err != nil {
		return "", nil, err
	}
	return source, rest, nil
}

// extractValueFlag strips one valued flag from a positional-heavy arg list. A
// repeated flag keeps the last; a trailing bare flag with no value is left in
// rest, where the caller's empty-value path treats it as "not given".
func extractValueFlag(args []string, name string) (value string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == name && i+1 < len(args) {
			value = args[i+1]
			i++
			continue
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			value = v
			continue
		}
		rest = append(rest, a)
	}
	return value, rest
}

// attachExistingRig makes `rig up` idempotent. If a rig whose id matches rigID
// already exists, it goes there instead of creating a duplicate: a live rig is
// switched to, a parked one is woken first (park stamp cleared, session restood
// at the same basedir so earlier agent sessions are a resume away).
//
// It returns done when it handled the id, so runUp only falls through to the
// create path when nothing matched — or when what matched is a rig whose own
// create was interrupted, which it hands back as unfinished rather than
// entering. Attaching to one of those cannot work (there is no workspace for a
// session to start in) and it is the retry that would have fixed it, so
// claiming the id here is what used to make the failure permanent: every
// `rig up MIR-1564` reported "already up" and then died in ensureRigRuntime.
// Session-stand-up-and-attach mirrors wake/switch.
func attachExistingRig(rigID string) (done bool, unfinished *rigInfo, err error) {
	rigs, err := listRigs()
	if err != nil {
		return false, nil, err
	}
	var found *rigInfo
	for i := range rigs {
		// TrackerID as well as ID, because `rig adopt` deliberately leaves the
		// local id alone: an adopted rig's id is still its kickoff slug, so an
		// ID-only match would miss it and build a second, empty rig for the same
		// ticket — the exact duplication this check exists to prevent.
		if rigs[i].ID == rigID ||
			(rigs[i].TrackerID != "" && strings.EqualFold(rigs[i].TrackerID, rigID)) {
			found = &rigs[i]
			break
		}
	}
	if found == nil {
		return false, nil, nil
	}
	if found.Building != "" {
		return false, found, nil
	}
	return true, nil, activateRig(*found)
}

// activateRig is the shared "put me in this rig" path. It is deliberately
// idempotent: parked rigs are woken, active rigs are switched to, and either
// kind gets its session rebuilt if it disappeared. `up`, `review`, and an
// explicit `wake` all mean this once they have resolved a concrete rig.
func activateRig(r rigInfo) error {
	wasParked := !r.Parked.IsZero()
	if err := setRigParked(r.Path, false, false, func(_ manifest) {
		if wasParked {
			fmt.Fprintf(os.Stderr, "rig: woke %s — %s\n", r.ID, r.Path)
		} else {
			fmt.Fprintf(os.Stderr, "rig: %s already up — switching\n", r.ID)
		}
	}); err != nil {
		return err
	}
	return attachOrReport(tmuxSessionName(r.Path))
}
