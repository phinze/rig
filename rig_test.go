package main

import "testing"

func TestTaskSlug(t *testing.T) {
	cases := []struct {
		name, id, title, want string
	}{
		{"basic", "pr-845", "Cleanup redundant logs over time", "pr-845-cleanup-redundant-logs-over-time"},
		{"empty title", "pr-845", "", "pr-845"},
		{"symbols-only title", "pr-845", "!!!", "pr-845"},
		{
			"hard cap at 60 with trailing dash trimmed",
			"mir-1184",
			"add cloud API to return the groups for a user in a sandbox",
			// raw join is 68 chars; cut lands mid-word, Linear-style
			"mir-1184-add-cloud-api-to-return-the-groups-for-a-user-in-a",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := taskSlug(c.id, c.title); got != c.want {
				t.Errorf("taskSlug(%q, %q) = %q, want %q", c.id, c.title, got, c.want)
			}
			if got := taskSlug(c.id, c.title); len(got) > 60 {
				t.Errorf("slug exceeds cap: %d chars", len(got))
			}
		})
	}
}
