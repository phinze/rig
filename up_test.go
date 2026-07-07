package main

import (
	"reflect"
	"testing"
)

func TestPRRigIdentity(t *testing.T) {
	cases := []struct {
		name     string
		branch   string
		title    string
		number   int
		wantID   string
		wantSlug string
	}{
		{
			// PR born from a Linear issue: identity matches what `rig up MIR-75`
			// built, so a resumed rig lands at the same path.
			name: "linear branch", branch: "phinze/mir-75-add-zig-stack",
			title: "Add zig stack", number: 42,
			wantID: "mir-75", wantSlug: "mir-75-add-zig-stack",
		},
		{
			// No user prefix, still a Linear branch.
			name: "no user prefix", branch: "eng-3-fix-thing",
			title: "Fix thing", number: 7,
			wantID: "eng-3", wantSlug: "eng-3-fix-thing",
		},
		{
			// A PR not born from an issue: fall back to pr-<n> + title.
			name: "non-issue branch", branch: "phinze/adhoc-cleanup",
			title: "Ad-hoc cleanup", number: 99,
			wantID: "pr-99", wantSlug: "pr-99-ad-hoc-cleanup",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := &prRef{Owner: "me", Repo: "api", Number: tc.number}
			id, slug := prRigIdentity(pr, prMeta{Branch: tc.branch, Title: tc.title})
			if id != tc.wantID || slug != tc.wantSlug {
				t.Errorf("prRigIdentity = (%q, %q), want (%q, %q)", id, slug, tc.wantID, tc.wantSlug)
			}
		})
	}
}

func TestLeadingIssueID(t *testing.T) {
	cases := map[string]string{
		"mir-75-add-zig-stack": "mir-75",
		"mir-75":               "mir-75",
		"eng2-3-thing":         "eng2-3", // Linear keys may carry digits
		"adhoc-cleanup":        "",       // no numeric id
		"fix-auth":             "",
		"add-zig-stack":        "",
	}
	for slug, want := range cases {
		if got := leadingIssueID(slug); got != want {
			t.Errorf("leadingIssueID(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestExtractRepoFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantRepo string
		wantRest []string
	}{
		{"no flag", []string{"MIR-75"}, "", []string{"MIR-75"}},
		{"spaced form", []string{"MIR-75", "--repo", "me/api"}, "me/api", []string{"MIR-75"}},
		{"equals form", []string{"--repo=me/api", "MIR-75"}, "me/api", []string{"MIR-75"}},
		{"flag among query words", []string{"fix", "--repo", "me/api", "auth"}, "me/api", []string{"fix", "auth"}},
		{"repeated keeps last", []string{"--repo", "me/a", "--repo=me/b"}, "me/b", nil},
		// A trailing bare --repo has no value to consume, so it stays in rest;
		// resolveRepo's empty-override path then falls through to the picker.
		{"trailing bare flag", []string{"MIR-75", "--repo"}, "", []string{"MIR-75", "--repo"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, rest := extractRepoFlag(tc.args)
			if repo != tc.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tc.wantRepo)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %#v, want %#v", rest, tc.wantRest)
			}
		})
	}
}
