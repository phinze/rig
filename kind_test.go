package main

import "testing"

func TestShowForLabelsByKind(t *testing.T) {
	cases := []struct {
		m     manifest
		label string
		first bool
	}{
		{manifest{ID: "purposeful-supporting-panes", Title: "purposeful supporting panes"}, "purposeful-supporting-panes", false},
		{manifest{ID: "pr-200", Kind: "review", Title: "review"}, rigKindGlyph(rigKindReview) + " pr-200", false},
		{manifest{ID: "cos-2026-10-31", Kind: "cos", Title: "chief of staff"}, rigKindGlyph(rigKindCoS) + " cos-2026-10-31", true},
	}
	for _, c := range cases {
		got := showFor(c.m)
		if got.label != c.label || got.first != c.first || got.title != c.m.Title {
			t.Errorf("showFor(%s) = %+v, want label %q first %v title %q", c.m.ID, got, c.label, c.first, c.m.Title)
		}
	}
}
