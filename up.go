package main

import (
	"fmt"
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

	// A PR URL means "pick up my own work on this PR" — authoring, not the issue
	// flow. pickupPR sorts authoring vs review by who owns the PR (and reroutes
	// to review if it's not yours). Idempotency lives inside the authoring pickup,
	// keyed off the branch-derived id (a PR born from an issue shares that issue's
	// rig), which is why there's no cheap pr-<n> pre-check here: pr-<n> is unique
	// only per repo, and wouldn't match an issue-keyed rig anyway.
	if len(args) >= 1 {
		if pr := parsePRURL(args[0]); pr != nil {
			return pickupPR(pr, "up", pick)
		}
	}

	id, err := resolveIssueID(args, pick)
	if err != nil {
		return err
	}
	if id == "" {
		return nil // picker cancelled
	}

	// Idempotency: `rig up X` means "put me in my rig for X, making it if it
	// isn't there." If a rig for this task already exists, go to it rather than
	// try to build a second one and error on the existing basedir. The check is
	// local (listRigs, no tracker call), so re-upping into work you already have
	// stays as instant as `rig switch`; the network only gets touched below, on
	// a genuine create.
	if done, err := attachExistingRig(strings.ToLower(id)); err != nil {
		return err
	} else if done {
		return nil
	}

	tk, err := resolveTask(id)
	if err != nil {
		return err
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

	m := manifest{ID: tk.rigID(), Title: tk.Title, Agent: string(pick.kind)}
	if err := createBasedir(basedir, m); err != nil {
		return err
	}

	startRev := resolveStartRev(repo.Path, tk.BranchName)
	// Record the Linear branch even when startRev fell back to trunk() because
	// the branch isn't pushed yet: it's still the branch this rig's PR will ride,
	// so pr/ls/reap resolve the right PR the moment it exists.
	repoDest, err := addRepoWorkspace(basedir, tk.rigID(), repo, startRev, tk.BranchName)
	if err != nil {
		return err
	}

	// Layout: recto on the right, the selected agent on the left with an issue-pickup
	// prompt. Linear-specific phrasing for now; when a second tracker
	// arrives we'll dispatch on it.
	sess := sessionSpec{
		rectoCmd: rectoCommand(),
		repo:     repo.Name,
		agent:    pick.kind,
		prompt: fmt.Sprintf(
			"Picking up %s (%s). Use the Linear MCP (it may take a few seconds to connect) to read the issue, mark it In Progress and assigned to me, then help me plan.",
			tk.Identifier, tk.Title,
		),
	}
	session, err := spawnSession(basedir, repoDest, sess)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "rig: up %s — %s\n", tk.Identifier, basedir)
	return attachOrReport(session)
}

// extractRepoFlag pulls a --repo owner/repo (or --repo=owner/repo) out of args,
// returning its value and the remaining args. `rig up` mixes the flag in with
// its issue-id / query / PR-url positional, so we strip it before dispatching on
// what's left. A repeated flag keeps the last; a trailing bare --repo with no
// value is left in rest, where resolveRepo's empty-override path treats it as
// "no override" and falls through to the picker.
func extractRepoFlag(args []string) (repo string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--repo" && i+1 < len(args) {
			repo = args[i+1]
			i++
			continue
		}
		if v, ok := strings.CutPrefix(a, "--repo="); ok {
			repo = v
			continue
		}
		rest = append(rest, a)
	}
	return repo, rest
}

// attachExistingRig makes `rig up` idempotent. If a rig whose id matches rigID
// already exists, it goes there instead of creating a duplicate: a live rig is
// switched to, a parked one is woken first (park stamp cleared, session restood
// at the same basedir so earlier agent sessions are a resume away). It
// reports whether it handled the id, so runUp only falls through to the create
// path when nothing matched. Session-stand-up-and-attach mirrors wake/switch.
func attachExistingRig(rigID string) (bool, error) {
	rigs, err := listRigs()
	if err != nil {
		return false, err
	}
	var found *rigInfo
	for i := range rigs {
		if rigs[i].ID == rigID {
			found = &rigs[i]
			break
		}
	}
	if found == nil {
		return false, nil
	}
	return true, activateRig(*found)
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
