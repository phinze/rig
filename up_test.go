package main

import (
	"reflect"
	"testing"
)

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
