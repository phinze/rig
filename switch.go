package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// runSwitch is rig's answer to session-wizard's `t`: a most-recently-touched
// switcher over the rigs in flight. With no arg it fzf-picks; with an arg it
// filters by id/slug/title substring and only falls back to the picker when the
// filter is ambiguous. `cd` is a retained alias for the muscle memory this
// grew out of.
//
// Three things separate it from a plain rig listing, and they're what make it
// feel like `t`: rows are ordered most-recently-attached first (not
// created-oldest-first), the session you're already in is dropped, and each row
// carries its live status (age, working/idle) so you switch with context. The
// status is all local — no gh round-trips — so the switcher stays instant.
func runSwitch(args []string) error {
	rigs, err := listRigs()
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	statuses := withRemoteStatuses(rigStatuses(rigs, home, time.Now()))

	// Drop the rig we're currently sitting in — you never switch to where you
	// already are.
	if cur := currentKey(); cur != (sessionKey{}) {
		kept := statuses[:0]
		for _, s := range statuses {
			if s.remote != nil || rigKey(s.Path, s.Backend) != cur {
				kept = append(kept, s)
			}
		}
		statuses = kept
	}

	// Parked rigs are deliberately dormant — awaiting review, session killed —
	// so they stay out of the switcher until `rig wake` brings one back.
	{
		kept := statuses[:0]
		for _, s := range statuses {
			if !s.Parked {
				kept = append(kept, s)
			}
		}
		statuses = kept
	}
	if len(statuses) == 0 {
		return fmt.Errorf("no other rigs in flight")
	}

	// Most-recently-attached first; sessionless rigs (last-attached 0) sink to
	// the bottom, ties broken by newest-created so a fresh rig outranks a stale
	// one.
	attached := lastAttached()
	sort.SliceStable(statuses, func(i, j int) bool {
		ai := attached[rigKey(statuses[i].Path, statuses[i].Backend)]
		aj := attached[rigKey(statuses[j].Path, statuses[j].Backend)]
		if ai != aj {
			return ai > aj
		}
		return statuses[i].Created.After(statuses[j].Created)
	})

	chosen, err := pickRigStatus(statuses, args, "switch to rig: ")
	if err != nil {
		return err
	}
	if chosen == nil {
		return nil
	}
	if chosen.remote != nil {
		// Enter it the way the radar does: through the surface, whose portal
		// or /v1/show moves the screen. A live remote rig needs no repair here,
		// and one that does is its host's to repair (rig resume id@place).
		b := surfaceNamed(chosen.remote.surface)
		return attachOrReport(b, chosen.remote.session)
	}
	rs, err := resumeRigRuntime(chosen.Path, false, false)
	if err != nil {
		return err
	}
	return rs.attach()
}

// pickRigStatus resolves a single rig from statuses. An arg substring-matches
// id/slug/title: a unique match is taken directly, an ambiguous one narrows the
// fzf picker, no match is an error. With no arg (or an ambiguous one) it opens
// fzf with the given prompt. Returns nil when the user escapes the picker.
// Shared by switch and wake so both filter and select the same way.
func pickRigStatus(statuses []rigStatus, args []string, prompt string) (*rigStatus, error) {
	if len(args) >= 1 {
		q := strings.ToLower(strings.Join(args, " "))
		var matches []rigStatus
		for _, s := range statuses {
			// TrackerID is in the haystack for adopted rigs, whose id stays the
			// kickoff slug: without it `rig switch MIR-123` can't reach the rig
			// that `rig dispatch MIR-123` and `rig relay` both already resolve.
			hay := strings.ToLower(remoteHandle(s) + " " + s.Slug + " " + s.Title + " " + s.TrackerID)
			if strings.Contains(hay, q) {
				matches = append(matches, s)
			}
		}
		if exact := exactPick(matches, q); exact != nil {
			return exact, nil
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("no rig matches %q", q)
		case 1:
			return &matches[0], nil
		default:
			statuses = matches // narrow the picker to the matches
		}
	}

	rows := make([]string, len(statuses))
	for i, s := range statuses {
		// col1 id, col2 age+agent, col3 title are shown; col4 slug is the
		// hidden lookup key (unique per rig, unlike id which can repeat).
		rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s", remoteHandle(s), switchStatusCol(s), s.Title, pickKey(s))
	}
	sel, err := fzfSelect(rows, prompt, nil)
	if err != nil {
		return nil, err
	}
	if sel == "" {
		return nil, nil
	}
	slug := strings.Split(sel, "\t")
	lookup := slug[len(slug)-1]
	for i := range statuses {
		if pickKey(statuses[i]) == lookup {
			return &statuses[i], nil
		}
	}
	return nil, nil
}

// pickKey is a row's hidden lookup key: the slug, which is unique per host,
// qualified by place for a rig on another one, since every host slugs
// ~/workspaces/foo the same way.
func pickKey(s rigStatus) string {
	if s.remote != nil {
		return s.Slug + "@" + s.Location
	}
	return s.Slug
}

// exactPick settles the two ambiguities remote rows bring and no others: a
// query spelled id@place means that row, and a query that names exactly one
// rig here outright beats rigs elsewhere that merely share the name, which is
// local-wins, the rule every verb follows. Anything else ambiguous stays the
// picker's to narrow, as it always was.
func exactPick(matches []rigStatus, q string) *rigStatus {
	var local, handle []int
	remotes := 0
	for i, s := range matches {
		if s.remote != nil {
			remotes++
			if strings.EqualFold(remoteHandle(s), q) {
				handle = append(handle, i)
			}
			continue
		}
		if strings.EqualFold(s.ID, q) || strings.EqualFold(s.Slug, q) || (s.TrackerID != "" && strings.EqualFold(s.TrackerID, q)) {
			local = append(local, i)
		}
	}
	if len(handle) == 1 {
		return &matches[handle[0]]
	}
	if len(local) == 1 && len(local)+remotes == len(matches) && remotes > 0 {
		return &matches[local[0]]
	}
	return nil
}

// switchStatusCol renders the compact age/agent cell for a switch row, e.g.
// "2h  working" or just "2h" when no agent is live.
func switchStatusCol(s rigStatus) string {
	if s.Agent == "" {
		return age(s.Created)
	}
	return age(s.Created) + "  " + s.Agent
}
