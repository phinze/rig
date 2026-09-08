package main

import (
	"fmt"
	"os"
	"strings"
)

// runAdopt gives a rig the tracker identity it lacked when it was created:
// `rig new` work that grew into something worth tracking and filed its own
// ticket. It is deliberately the smallest possible promotion — three manifest
// fields and a regenerated instruction file — because the alternative is a
// rename, and a rename would burn the thing the rig is for.
//
// The local id, basedir, tmux session and jj workspace all stay exactly as they
// were. Every agent store keys on absolute cwd or workspace path (see
// agentSessionActivity and friends), never on rig, so moving the basedir to a
// ticket-shaped name would orphan the whole conversation — and a rig that filed
// its own ticket is precisely one whose conversation is the valuable part. So
// the id stays where the work lives, and the tracker says what it's about.
//
// What that unlocks is real rather than cosmetic: `rig relay` stops refusing,
// `rig project status` can see the rig at all, `rig dispatch MIR-123` reaches
// it, and the boards draw it with the ticket kind instead of as a loose rig.
func runAdopt(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: rig adopt <issue>")
	}
	id := strings.ToUpper(strings.TrimSpace(args[0]))

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	basedir, err := findBasedir(cwd)
	if err != nil {
		return err
	}
	lock, err := acquireRigMutationLock(basedir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	if m.isProject() {
		return fmt.Errorf("a project rig already tracks a Linear project; adopt promotes a task rig")
	}
	if m.TrackerID != "" {
		if strings.EqualFold(m.TrackerID, id) {
			fmt.Fprintf(os.Stderr, "rig: %s already tracks %s\n", m.ID, m.TrackerID)
			return nil
		}
		// Reassignment is refused rather than supported. The rig's whole history
		// — its branch, its PR, its conversation — accumulated under the first
		// identifier, and silently re-pointing it would leave every board
		// agreeing about a ticket none of that work belongs to.
		return fmt.Errorf("%s already tracks %s; adopt promotes an untracked rig, it does not reassign one", m.ID, m.TrackerID)
	}

	// Resolving is what makes this worth a command rather than a documented
	// hand-edit: a typo'd identifier would otherwise sit in the manifest until
	// relay or project status failed with an error about something else.
	tk, err := resolveTask(id)
	if err != nil {
		return err
	}

	m.Tracker = "linear"
	m.TrackerID = tk.Identifier
	if tk.Title != "" {
		// The ticket is the canonical name for the work now. The kickoff line
		// isn't lost: it's still the rig's id, and still the heading of
		// KICKOFF.md, which is where the original framing belongs.
		m.Title = tk.Title
	}
	if err := writeManifest(basedir, m); err != nil {
		return err
	}
	// The generated instructions carry the rig's title, so without this every
	// agent that loads them keeps being told the kickoff line is the task.
	if err := writeRigAgentInstructions(basedir, m); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "rig: %s adopted %s — %s\n", m.ID, tk.Identifier, tk.Title)
	fmt.Fprintf(os.Stderr, "rig: id, basedir and session unchanged; link the PR by branching as %s or by naming %s in its body\n",
		tk.workBranchName(), tk.Identifier)
	return nil
}
