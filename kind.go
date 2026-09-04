package main

import "strings"

// rigKind is what a rig is *about*, as distinct from what it's doing. The
// boards spend their loud cells on state (agent attention, review disposition,
// the proposed next step), so kind rides quietly beside the id on each of them.
// Without it a ticket rig, a review rig and a project rig read as three
// identical rows whose ids merely happen to be shaped differently, and you
// learn the shapes instead of seeing the kinds.
type rigKind int

const (
	rigKindLoose rigKind = iota // `rig new`: a kickoff, no tracker
	rigKindTicket
	rigKindReview
	rigKindProject
)

func rigKindOf(s rigStatus) rigKind {
	switch s.Kind {
	case "review":
		return rigKindReview
	case "project":
		return rigKindProject
	}
	if s.Tracker != "" {
		return rigKindTicket
	}
	// Rigs made before the manifest recorded a tracker have none, and several
	// are usually still in flight. Leaving those unmarked would make the marker
	// read as unreliable rather than absent, so fall back to the id, which for
	// a Linear pickup is the issue identifier and nothing else. "pr-<n>" is
	// rig's own reserved id for a PR-derived rig rather than a team prefix, so
	// it's the one shape this must not read as an issue.
	if !strings.HasPrefix(s.ID, "pr-") && leadingIssueID(s.ID) == s.ID {
		return rigKindTicket
	}
	return rigKindLoose
}

// rigKindGlyph marks the id cell with its kind. The shapes come from families
// the state glyphs don't use — a tag for a ticket, octicon's pull-request for a
// review (its git-merge sibling already means merged), a sitemap for a
// project's umbrella of issues — so kind and state never trade places at a
// glance. A loose rig draws nothing: absence is its marker, and a glyph on
// every row buys less than the two columns it would cost in a popup.
func rigKindGlyph(k rigKind) string {
	switch k {
	case rigKindTicket:
		return "\uf02b"
	case rigKindReview:
		return "\uf407"
	case rigKindProject:
		return "\uf0e8"
	}
	return ""
}

// rigKindCell is the fixed-width kind marker the flat boards draw: the glyph
// and a space, or two blanks for a loose rig. Padding rather than omitting is
// what keeps the id column beneath it aligned, which is the whole reason a
// marker in a table is worth having.
func rigKindCell(s rigStatus) string {
	if g := rigKindGlyph(rigKindOf(s)); g != "" {
		return g + " "
	}
	return "  "
}
