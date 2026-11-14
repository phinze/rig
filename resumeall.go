package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// resumeAllWait is how long resume --all waits for the agents it launched to
// show up in their panes before calling the ones still missing failures.
var resumeAllWait = 30 * time.Second

// resumeAllPoll is the gap between pane listings while it waits. Each listing
// is a batch of backend calls, so this stays coarse.
var resumeAllPoll = 2 * time.Second

// runResumeAll brings back every local rig that should have an agent and
// doesn't: not parked, not half-built, and either its session is gone or its
// agent seat holds a bare shell. That is the state a multiplexer restart or a
// reboot leaves, and nothing else needs a person to look at each rig first.
// It never attaches, so it works without a tty, and it reports each rig
// only once its agent is seen running.
func runResumeAll(dryRun bool, out io.Writer) error {
	rigs, err := listRigs()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	statuses := boardStatuses(rigs, home, time.Now())
	targets := resumeAllTargets(statuses, localCurrentSession())
	if len(targets) == 0 {
		fmt.Fprintln(out, "rig: every rig that should have an agent has one")
		return nil
	}
	for _, s := range targets {
		fmt.Fprintf(out, "%s\t%s\n", s.ID, resumeAllReason(s))
	}
	if dryRun {
		return nil
	}

	var launched []rigStatus
	var failed []string
	for _, s := range targets {
		// Nonblocking: a rig someone else is mutating right now is theirs to
		// finish, and waiting on its lock would stall every rig after it.
		if _, err := resumeRigRuntime(s.Path, true, true); err != nil {
			fmt.Fprintf(out, "%s\tfailed: %v\n", s.ID, err)
			failed = append(failed, s.ID)
			continue
		}
		launched = append(launched, s)
	}

	missing := awaitAgents(launched, resumeAllWait)
	for _, s := range launched {
		if missing[s.Path] {
			fmt.Fprintf(out, "%s\tfailed: no agent running after %s\n", s.ID, resumeAllWait)
			failed = append(failed, s.ID)
			continue
		}
		fmt.Fprintf(out, "%s\tresumed\n", s.ID)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d rigs did not come back: %s", len(failed), len(targets), strings.Join(failed, ", "))
	}
	return nil
}

// resumeAllTargets picks the rigs resume --all repairs, in id order. The rig
// whose session holds the caller goes last: resuming it from its own agent
// seat replaces this process, which would end the loop for everything after.
func resumeAllTargets(statuses []rigStatus, currentSession string) []rigStatus {
	var targets []rigStatus
	for _, s := range statuses {
		if s.remote != nil || s.Parked || s.Building != "" {
			continue
		}
		if s.SessionLive && s.Agent != agentNone {
			continue
		}
		targets = append(targets, s)
	}
	sort.SliceStable(targets, func(i, j int) bool {
		iSelf := rigSessionName(targets[i].Path) == currentSession
		jSelf := rigSessionName(targets[j].Path) == currentSession
		if iSelf != jSelf {
			return jSelf
		}
		return targets[i].ID < targets[j].ID
	})
	return targets
}

func resumeAllReason(s rigStatus) string {
	if !s.SessionLive {
		return "no session"
	}
	return "session up, no agent"
}

// localCurrentSession is the session this process runs inside, on whichever
// local backend knows it, or "".
func localCurrentSession() string {
	for _, b := range localBackends() {
		if name := b.CurrentSession(); name != "" {
			return name
		}
	}
	return ""
}

// awaitAgents lists panes until every launched rig shows an agent or the wait
// runs out, and returns the basedirs still missing one. It lists every local
// pane per round rather than probing rig by rig, since each probe is a
// backend call.
func awaitAgents(launched []rigStatus, wait time.Duration) map[string]bool {
	missing := map[string]bool{}
	for _, s := range launched {
		missing[s.Path] = true
	}
	deadline := time.Now().Add(wait)
	for len(missing) > 0 {
		_, panes := boardListing(localBackends())
		agents := agentChildren(panes, time.Now().Unix())
		for _, s := range launched {
			if len(agents[rigKey(s.Path, s.Backend)]) > 0 {
				delete(missing, s.Path)
			}
		}
		if len(missing) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(resumeAllPoll)
	}
	return missing
}
