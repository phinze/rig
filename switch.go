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
	statuses := rigStatuses(rigs, home, time.Now())

	// Drop the rig we're currently sitting in — you never switch to where you
	// already are.
	if cur := currentTmuxSession(); cur != "" {
		kept := statuses[:0]
		for _, s := range statuses {
			if tmuxSessionName(s.Path) != cur {
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
	attached := tmuxLastAttached()
	sort.SliceStable(statuses, func(i, j int) bool {
		ai := attached[tmuxSessionName(statuses[i].Path)]
		aj := attached[tmuxSessionName(statuses[j].Path)]
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
	if err := touchRig(chosen.Path); err != nil {
		return err
	}

	session := tmuxSessionName(chosen.Path)
	if !tmuxHasSession(session) {
		// Rig dir is present but its session was killed; stand up a bare one.
		if err := tmuxNewSession(session, chosen.Path); err != nil {
			return fmt.Errorf("tmux new-session: %w", err)
		}
	}
	return attachOrReport(session)
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
			hay := strings.ToLower(s.ID + " " + s.Slug + " " + s.Title)
			if strings.Contains(hay, q) {
				matches = append(matches, s)
			}
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
		rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s", s.ID, switchStatusCol(s), s.Title, s.Slug)
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
		if statuses[i].Slug == lookup {
			return &statuses[i], nil
		}
	}
	return nil, nil
}

// switchStatusCol renders the compact age/agent cell for a switch row, e.g.
// "2h  working" or just "2h" when no agent is live.
func switchStatusCol(s rigStatus) string {
	if s.Agent == "" {
		return age(s.Created)
	}
	return age(s.Created) + "  " + s.Agent
}
