package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Sweep covers every host by asking each one for its own plan. A rig's next
// step depends on facts only its host has (jj state, the teardown gate's
// fetch, whether its agent is mid-turn), so each host plans its own rigs with
// `sweep --format=json` and this board lays the remote rows beside the local
// ones. Running a checked remote row forwards the verb to its host, where the
// gate runs again under that host's lock exactly as it would locally. Merges
// are the exception, since gh doesn't care which machine asks.

// sweepPlanJSON is one plan on the wire, and the shape `sweep --format=json`
// prints for a program to read.
type sweepPlanJSON struct {
	Rig        rigRowJSON `json:"rig"`
	Location   string     `json:"location"`
	Action     string     `json:"action"`
	Detail     string     `json:"detail"`
	Merges     []rigPR    `json:"merges,omitempty"`
	Held       []string   `json:"held,omitempty"`
	Repos      int        `json:"repos"`
	Dirty      []string   `json:"dirty,omitempty"`
	AgentTitle string     `json:"agentTitle,omitempty"`
	Collect    bool       `json:"collect"`
}

func plansForJSON(plans []sweepPlan) []sweepPlanJSON {
	out := make([]sweepPlanJSON, 0, len(plans))
	for _, p := range plans {
		loc := "local"
		if p.status.remote != nil {
			loc = p.status.Location
		}
		out = append(out, sweepPlanJSON{
			Rig: rigsForJSON([]rigStatus{p.status}, true)[0], Location: loc,
			Action: p.action, Detail: p.detail, Merges: p.merges, Held: p.held,
			Repos: p.repos, Dirty: p.dirty, AgentTitle: p.agentTitle, Collect: p.collect,
		})
	}
	return out
}

func printSweepJSON(plans []sweepPlan) error {
	blob, err := json.MarshalIndent(plansForJSON(plans), "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(blob))
	return nil
}

// remoteSweepPlans is every other host's plan, lifted into rows this board
// draws and runs. A host that didn't answer is said on stderr, which the board
// hasn't taken over yet when this runs.
func remoteSweepPlans() []sweepPlan {
	hosts := remoteHosts()
	if len(hosts) == 0 {
		return nil
	}
	answers := eachHost(hosts, func(b servedBackend) []sweepPlan {
		place := surfacePlace(b.Surface())
		if servedBreaker.Down(b.URL) {
			return nil
		}
		out, err := b.execCapture("sweep", "--format=json")
		if err != nil {
			fmt.Fprintf(os.Stderr, "rig: %s didn't answer: %v\n", place, err)
			return nil
		}
		var wire []sweepPlanJSON
		if err := json.Unmarshal(out, &wire); err != nil {
			fmt.Fprintf(os.Stderr, "rig: %s: unreadable plan: %v\n", place, err)
			return nil
		}
		var plans []sweepPlan
		for _, w := range wire {
			s := statusFromRow(w.Rig)
			s.Location = place
			s.remote = &remoteRig{surface: b.Surface(), prsLooked: true}
			plans = append(plans, sweepPlan{
				rig: rigInfo{ID: s.ID, Slug: s.Slug, Path: s.Path}, status: s,
				action: w.Action, detail: w.Detail, merges: w.Merges, held: w.Held,
				repos: w.Repos, dirty: w.Dirty, agentTitle: w.AgentTitle, collect: w.Collect,
			})
		}
		return plans
	})
	var all []sweepPlan
	for _, a := range answers {
		all = append(all, a...)
	}
	return all
}

// executeRemotePlan runs one checked remote row. Its host re-runs every gate,
// so a refusal there (work turned up, the lock is held) is that rig's skip and
// not the pass's failure: its reason already streamed above. Only failing to
// reach the host stops the pass, the same "a fact about your setup" rule the
// local rows follow.
func executeRemotePlan(p sweepPlan, dryRun bool, mergeFlag string) error {
	handle := remoteHandle(p.status)
	t, err := remoteOf(p.status)
	if err != nil {
		return err
	}
	run := func(argv ...string) error {
		err := t.host.exec(execRequest{Argv: argv, From: currentSender().addr}, os.Stderr, os.Stderr)
		if _, ok := errors.AsType[remoteExit](err); ok {
			fmt.Fprintf(os.Stderr, "  %s: skipped — %s said no (above)\n", handle, t.place)
			return nil
		}
		return err
	}
	switch p.action {
	case actionMerge:
		for _, pr := range p.merges {
			if dryRun {
				fmt.Fprintf(os.Stderr, "  would merge %s#%d\n", pr.Repo, pr.Number)
				continue
			}
			fmt.Fprintf(os.Stderr, "  merging %s#%d…\n", pr.Repo, pr.Number)
			if err := ghMergePR(pr.Repo, pr.Number, mergeFlag); err != nil {
				return fmt.Errorf("merging %s#%d: %w", pr.Repo, pr.Number, err)
			}
		}
		if dryRun {
			fmt.Fprintf(os.Stderr, "  then down %s if it comes up clean\n", handle)
			return nil
		}
		return run("down", t.row.ID)
	case actionDown:
		if dryRun {
			fmt.Fprintf(os.Stderr, "  would down %s\n", handle)
			return nil
		}
		return run("down", t.row.ID)
	case actionWake, actionPark:
		if dryRun {
			fmt.Fprintf(os.Stderr, "  would %s %s\n", p.action, handle)
			return nil
		}
		// exec rather than forward: a sweep wake unparks and rebuilds, it
		// doesn't take you there, so the session the far wake reports is
		// dropped on the floor here.
		return run(p.action, t.row.ID)
	}
	return nil
}
