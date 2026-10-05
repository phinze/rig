package main

import (
	"fmt"
	"os"
	"time"
)

// cosAddress is the name every rig can send to without knowing which day it
// is. A chief-of-staff rig lives for one workday, so its id changes daily, and
// a report-back line naming yesterday's id breaks the moment yesterday's rig is
// torn down. The alias is what makes "tell the chief of staff" a stable
// instruction rather than one that expires overnight.
const cosAddress = "cos"

// cosRigID names the chief-of-staff rig for a given workday. The date is the
// whole identity: one per day, started in the morning and torn down at its
// end, so `rig cos` on the same day always lands in the same rig.
func cosRigID(day time.Time) string { return "cos-" + day.Format(time.DateOnly) }

// runCoS creates or enters today's chief-of-staff rig. Like a project rig it
// is repositoryless, its agent running from the rig root, but it coordinates
// across every rig rather than one project's.
func runCoS(args []string) error {
	pick, args, err := extractAgentFlag(args)
	if err != nil {
		return err
	}
	defer pick.cleanup()
	if len(args) > 0 {
		return fmt.Errorf("usage: rig cos [--agent AGENT]")
	}

	now := time.Now()
	rigID := cosRigID(now)
	rigs, err := listRigs()
	if err != nil {
		return err
	}
	for _, r := range rigs {
		if r.ID == rigID {
			return activateRig(r)
		}
	}
	// Yesterday's rig still standing is the normal case: the day ends with the
	// next morning's handover, not a teardown at night. Sends to `cos` now
	// reach the newest rig, so the old one hears only from its successor, which
	// takes the handover and then tears it down. That's why the newest
	// predecessor is named in today's kickoff and not just printed here.
	var predecessor string
	for _, r := range rigs {
		if r.Kind == "cos" && r.ID < rigID {
			fmt.Fprintf(os.Stderr, "rig: %s is still up; sends to %s now reach %s\n", r.ID, cosAddress, rigID)
			if r.ID > predecessor {
				predecessor = r.ID
			}
		}
	}
	if ok, err := pick.ensurePicked(); err != nil || !ok {
		return err
	}

	basedir, err := basedirPath(rigID)
	if err != nil {
		return err
	}
	day := now.Format(time.DateOnly)
	m := manifest{
		ID: rigID, Title: "chief of staff " + day, Kind: "cos",
		Agent: string(pick.kind), Backend: preferredBackend.Name(),
	}
	if err := createBasedir(basedir, m); err != nil {
		return err
	}
	if err := writeRigAgentInstructions(basedir, m); err != nil {
		return fmt.Errorf("writing cos rig instructions: %w", err)
	}
	rs := sessionFor(basedir, m)
	err = spawnCoordinatorSession(rs, basedir, sessionSpec{
		agent:   pick.kind,
		prompt:  cosKickoff(day, predecessor),
		rigID:   m.ID,
		basedir: basedir,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rig: chief of staff for %s — %s\n", day, basedir)
	return rs.attach()
}

// cosKickoff starts the day's agent. When an earlier cos rig is still up, the
// handover comes first: that rig's conversation may hold more than its plan
// file does, and it's reachable only by its dated id, since `cos` now means
// today. Its `rig reply` routes back here on its own.
func cosKickoff(day, predecessor string) string {
	prompt := fmt.Sprintf("You are the chief of staff for %s. Use the chief-of-staff skill.", day)
	if predecessor != "" {
		prompt += fmt.Sprintf(" The previous chief of staff, %s, is still up: before anything else, ask it for a handover with `rig send %s <message>` and wait for its reply. Once its plan-file section is in, tear it down with `rig down` from its basedir. If it can't be reached, fall back to the plan file and leave the teardown to me.", predecessor, predecessor)
	}
	return prompt + " Pick up any threads carried over from the last workday, read the board, and brief me. Propose before acting on another rig, and draft anything with my name on it for approval."
}

// resolveCoS answers the `cos` address: the newest chief-of-staff rig. Newest
// rather than live, because a send to a rig that isn't running already fails
// loudly with the reason, which says more than "no rig matches" would.
func resolveCoS(rigs []rigInfo) (rigInfo, bool) {
	var best rigInfo
	for _, r := range rigs {
		if r.Kind == "cos" && r.ID > best.ID {
			best = r
		}
	}
	return best, best.ID != ""
}
