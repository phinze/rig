package main

import (
	"os"
	"reflect"
	"strings"
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
		linked   []linkedLinearTask
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
			// Linear's attachment lookup is canonical even when the branch is
			// manually named.
			name: "linked Linear issue", branch: "phinze/custom-branch",
			title: "PR wording differs", number: 43,
			linked: []linkedLinearTask{{
				Task:     task{Identifier: "MIR-76", Title: "Issue wording", BranchName: "phinze/mir-76-issue-wording"},
				LinkKind: "contributes",
			}},
			wantID: "mir-76", wantSlug: "mir-76-issue-wording",
		},
		{
			// New Rig branches keep a reversible id without giving Linear its
			// literal auto-linking token. This is the offline lookup fallback.
			name: "escaped Linear branch", branch: "phinze/mir_77-fix-thing",
			title: "Fix thing", number: 44,
			wantID: "mir-77", wantSlug: "mir-77-fix-thing",
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
			id, slug := prRigIdentity(pr, prMeta{Branch: tc.branch, Title: tc.title}, tc.linked)
			if id != tc.wantID || slug != tc.wantSlug {
				t.Errorf("prRigIdentity = (%q, %q), want (%q, %q)", id, slug, tc.wantID, tc.wantSlug)
			}
		})
	}
}

func TestTaskWorkBranchName(t *testing.T) {
	cases := map[string]string{
		"phinze/mir-75-add-zig-stack":  "phinze/mir_75-add-zig-stack",
		"mir-75-user/mir-75-fix-thing": "mir-75-user/mir_75-fix-thing",
		"eng2-3-fix-thing":             "eng2_3-fix-thing",
		"phinze/adhoc-cleanup":         "phinze/adhoc-cleanup",
	}
	for branch, want := range cases {
		tk := task{Identifier: strings.ToUpper(leadingIssueID(stripBranchUserPrefix(branch))), BranchName: branch}
		if tk.Identifier == "" {
			tk.Identifier = "MIR-75"
		}
		if got := tk.workBranchName(); got != want {
			t.Errorf("workBranchName(%q) = %q, want %q", branch, got, want)
		}
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

func TestLeadingEscapedIssueID(t *testing.T) {
	cases := map[string]string{
		"mir_75-add-zig-stack": "mir-75",
		"eng2_3-thing":         "eng2-3",
		"mir-75-old-shape":     "",
		"adhoc_cleanup":        "",
	}
	for slug, want := range cases {
		if got := leadingEscapedIssueID(slug); got != want {
			t.Errorf("leadingEscapedIssueID(%q) = %q, want %q", slug, got, want)
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

func TestExtractContextFlag(t *testing.T) {
	ctx, rest := extractContextFlag([]string{"MIR-75", "--context", "reuse the dial helper", "--repo", "me/api"})
	if ctx != "reuse the dial helper" || !reflect.DeepEqual(rest, []string{"MIR-75", "--repo", "me/api"}) {
		t.Fatalf("context = %q rest = %#v", ctx, rest)
	}
	ctx, rest = extractContextFlag([]string{"--context=one liner", "MIR-75"})
	if ctx != "one liner" || !reflect.DeepEqual(rest, []string{"MIR-75"}) {
		t.Fatalf("equals form: context = %q rest = %#v", ctx, rest)
	}
}

// The flag and a pipe both feed the same file, flag first, and neither is
// required: a project agent's shell tool has a non-terminal, empty stdin, which
// must read as "no context" rather than blocking or writing an empty kickoff.
func TestResolveUpContextJoinsFlagAndStdin(t *testing.T) {
	withStdin := func(t *testing.T, body string) {
		t.Helper()
		f, err := os.CreateTemp(t.TempDir(), "stdin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		old := os.Stdin
		os.Stdin = f
		t.Cleanup(func() { os.Stdin = old; _ = f.Close() })
	}

	withStdin(t, "")
	if got, err := resolveUpContext(""); err != nil || got != "" {
		t.Fatalf("nothing given = %q, %v", got, err)
	}
	if got, _ := resolveUpContext("  flag only  "); got != "flag only" {
		t.Fatalf("flag only = %q", got)
	}

	withStdin(t, "piped\nblob\n")
	if got, _ := resolveUpContext(""); got != "piped\nblob" {
		t.Fatalf("stdin only = %q", got)
	}
	withStdin(t, "piped\nblob\n")
	if got, _ := resolveUpContext("from flag"); got != "from flag\n\npiped\nblob" {
		t.Fatalf("both = %q", got)
	}
}

func TestPickupPromptPointsAtKickoffOnlyWithContext(t *testing.T) {
	tk := task{Identifier: "MIR-75", Title: "add zig stack"}
	plain := pickupPrompt(tk, false)
	if strings.Contains(plain, rigKickoffName) || !strings.Contains(plain, "MIR-75 (add zig stack)") {
		t.Fatalf("plain prompt = %q", plain)
	}
	with := pickupPrompt(tk, true)
	if !strings.Contains(with, "../"+rigKickoffName) || !strings.HasPrefix(with, plain) {
		t.Fatalf("context prompt should extend the plain one and name the file: %q", with)
	}
}
