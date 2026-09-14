package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskSourceCycleWraps(t *testing.T) {
	seen := map[taskSource]bool{}
	s := sourceLinear
	for range taskSources {
		seen[s] = true
		s = s.next()
	}
	if s != sourceLinear || len(seen) != len(taskSources) {
		t.Fatalf("cycle from linear visited %v and landed on %q", seen, s)
	}
}

func TestParseTaskSourceSpellings(t *testing.T) {
	for name, want := range map[string]taskSource{
		"linear": sourceLinear, "GitHub": sourceGitHub, "gh": sourceGitHub,
		"tasks": sourceVikunja, "vikunja\n": sourceVikunja, "personal-tasks": sourceVikunja,
	} {
		got, err := parseTaskSource(name)
		if err != nil || got != want {
			t.Errorf("parseTaskSource(%q) = (%q, %v), want %q", name, got, err, want)
		}
	}
	if _, err := parseTaskSource("jira"); err == nil {
		t.Error("parseTaskSource(jira) should fail")
	}
}

// ctrl-t's transform-prompt shells out to `rig __source cycle STATE`, which
// must advance the file (so the reload that follows searches the new source)
// and print the prompt fzf redraws.
func TestSourceCycleCommandRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte("linear\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt, err := cycleSourceState(path)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != issuePrompt(sourceGitHub) {
		t.Errorf("prompt = %q, want the github prompt", prompt)
	}
	if got, ok := readSourceState(path); !ok || got != sourceGitHub {
		t.Errorf("state after cycle = (%q, %v), want github", got, ok)
	}

	// Garbage in the file costs the keystroke a known starting point, not the
	// picker: it cycles from the default rather than failing.
	if err := os.WriteFile(path, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if prompt, err := cycleSourceState(path); err != nil || prompt != issuePrompt(sourceGitHub) {
		t.Errorf("cycle from garbage = (%q, %v), want a restart from linear", prompt, err)
	}
}

func TestSourcePickBindReloadsAfterCycling(t *testing.T) {
	p := &sourcePick{source: sourceVikunja}
	if err := p.prepare(); err != nil {
		t.Fatal(err)
	}
	defer p.cleanup()
	if got, ok := readSourceState(p.statePath); !ok || got != sourceVikunja {
		t.Fatalf("prepared state = (%q, %v), want vikunja", got, ok)
	}
	args := p.fzfArgs("/bin/rig", "'/bin/rig' __issues STATE {q}")
	if len(args) != 1 {
		t.Fatalf("fzfArgs = %q, want one bind", args)
	}
	bind := args[0]
	for _, want := range []string{"--bind=" + sourceCycleKey + ":", "transform-prompt(", "__source cycle", "+reload('/bin/rig' __issues STATE {q})"} {
		if !strings.Contains(bind, want) {
			t.Errorf("bind %q lacks %q", bind, want)
		}
	}
	if !strings.Contains(bind, p.statePath) {
		t.Errorf("bind %q doesn't name the state file", bind)
	}
}

func TestParseGitHubIssueRef(t *testing.T) {
	for in, want := range map[string]githubIssueRef{
		"phinze/rig#9": {Owner: "phinze", Repo: "rig", Number: 9},
		"https://github.com/phinze/recto/issues/10": {Owner: "phinze", Repo: "recto", Number: 10},
		"https://github.com/a/b.c/issues/3/":        {Owner: "a", Repo: "b.c", Number: 3},
	} {
		got := parseGitHubIssueRef(in)
		if got == nil || *got != want {
			t.Errorf("parseGitHubIssueRef(%q) = %+v, want %+v", in, got, want)
		}
	}
	for _, in := range []string{"#9", "MIR-75", "phinze/rig", "https://github.com/phinze/rig/pull/9", "phinze/rig#"} {
		if got := parseGitHubIssueRef(in); got != nil {
			t.Errorf("parseGitHubIssueRef(%q) = %+v, want nil", in, got)
		}
	}
}

// The local id is derivable from the ref alone, which is what lets `rig up`
// find an existing rig before it spends a lookup. A GitHub issue's id carries
// the repo: the number alone is unique only per repo, and pr-<n> is taken.
func TestTaskRefRigID(t *testing.T) {
	for in, want := range map[taskRef]string{
		{source: sourceLinear, id: "MIR-75"}:       "mir-75",
		{id: "PERS-3"}:                             "pers-3",
		{source: sourceGitHub, id: "phinze/Rig#9"}: "rig-9",
		{source: sourceGitHub, id: "https://github.com/phinze/recto/issues/10"}: "recto-10",
	} {
		if got := in.rigID(); got != want {
			t.Errorf("%+v.rigID() = %q, want %q", in, got, want)
		}
	}
}

func TestResolveIssueIDExactForms(t *testing.T) {
	got, err := resolveIssueID([]string{"phinze/rig#9"}, nil, "")
	if err != nil || got != (taskRef{source: sourceGitHub, id: "phinze/rig#9"}) {
		t.Errorf("github short form = (%+v, %v)", got, err)
	}
	got, err = resolveIssueID([]string{"https://github.com/phinze/rig/issues/9"}, nil, sourceVikunja)
	if err != nil || got != (taskRef{source: sourceGitHub, id: "phinze/rig#9"}) {
		t.Errorf("github url form should win over --source: (%+v, %v)", got, err)
	}
	// A TEAM-123 id is left unsourced so the lookup is deferred past the
	// existing-rig check; --source pins it.
	got, err = resolveIssueID([]string{"PERS-3"}, nil, "")
	if err != nil || got != (taskRef{id: "PERS-3"}) {
		t.Errorf("bare id = (%+v, %v)", got, err)
	}
	got, err = resolveIssueID([]string{"PERS-3"}, nil, sourceVikunja)
	if err != nil || got != (taskRef{source: sourceVikunja, id: "PERS-3"}) {
		t.Errorf("--source id = (%+v, %v)", got, err)
	}
}

func TestExtractSourceFlag(t *testing.T) {
	src, rest, err := extractSourceFlag([]string{"--source", "tasks", "PERS-3"})
	if err != nil || src != sourceVikunja || strings.Join(rest, " ") != "PERS-3" {
		t.Errorf("= (%q, %q, %v)", src, rest, err)
	}
	src, rest, err = extractSourceFlag([]string{"some", "words"})
	if err != nil || src != "" || strings.Join(rest, " ") != "some words" {
		t.Errorf("no flag = (%q, %q, %v)", src, rest, err)
	}
	if _, _, err := extractSourceFlag([]string{"--source=jira"}); err == nil {
		t.Error("unknown source should error")
	}
}

func TestParseGitHubIssueRows(t *testing.T) {
	raw := []byte(`[
  {"number":9,"repository":{"name":"rig","nameWithOwner":"phinze/rig"},"state":"open","title":"macOS: test suite"},
  {"number":10,"repository":{"name":"recto","nameWithOwner":"phinze/recto"},"state":"OPEN","title":"A web surface"}
]`)
	rows, err := parseGitHubIssueRows(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []issueCandidate{
		{Identifier: "phinze/rig#9", State: "open", Title: "macOS: test suite"},
		{Identifier: "phinze/recto#10", State: "open", Title: "A web surface"},
	}
	if len(rows) != len(want) || rows[0] != want[0] || rows[1] != want[1] {
		t.Errorf("rows = %+v, want %+v", rows, want)
	}
}

func TestParseVikunjaTaskRows(t *testing.T) {
	raw := []byte(`[
  {"id":6,"title":"Add backups","done":false,"identifier":"PERS-3","buckets":[{"id":12,"title":"Todo"}]},
  {"id":7,"title":"Rotate token","done":false,"identifier":"PERS-4","buckets":[]},
  {"id":8,"title":"Old","done":true,"identifier":"PERS-1"}
]`)
	rows, err := parseVikunjaTaskRows(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []issueCandidate{
		{Identifier: "PERS-3", State: "Todo", Title: "Add backups"},
		{Identifier: "PERS-4", State: "open", Title: "Rotate token"},
		{Identifier: "PERS-1", State: "done", Title: "Old"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

// Synthesized branches carry Linear's `<user>/<id>-<slug>` shape so the same
// basedir derivation serves every source, and they're pushed as-is: only
// Linear links a PR from its branch name, so only Linear gets the escape.
func TestTaskDerivationsForSynthesizedBranches(t *testing.T) {
	gh := task{Source: sourceGitHub, Identifier: "phinze/rig#9", Title: "macOS: test suite", Repo: "phinze/rig",
		BranchName: "phinze/rig-9-macos-test-suite"}
	if gh.rigID() != "rig-9" || gh.basedirName() != "rig-9-macos-test-suite" || gh.workBranchName() != gh.BranchName {
		t.Errorf("github task derived (%q, %q, %q)", gh.rigID(), gh.basedirName(), gh.workBranchName())
	}
	vk := task{Source: sourceVikunja, Identifier: "PERS-3", Title: "Add backups", Domain: "personal",
		BranchName: "phinze/pers-3-add-backups"}
	if vk.rigID() != "pers-3" || vk.basedirName() != "pers-3-add-backups" || vk.workBranchName() != vk.BranchName {
		t.Errorf("vikunja task derived (%q, %q, %q)", vk.rigID(), vk.basedirName(), vk.workBranchName())
	}
	// No branch at all (gh couldn't say who you are) still yields a basedir.
	bare := task{Source: sourceVikunja, Identifier: "CTL-2", Title: "Fix the thing"}
	if bare.basedirName() != "ctl-2-fix-the-thing" {
		t.Errorf("bare basedirName = %q", bare.basedirName())
	}
	// A task built by the PR-link lookup has no source and is Linear.
	lin := task{Identifier: "MIR-75", BranchName: "phinze/mir-75-add-zig-stack"}
	if lin.source() != sourceLinear || lin.workBranchName() != "phinze/mir_75-add-zig-stack" {
		t.Errorf("blank source: %q %q", lin.source(), lin.workBranchName())
	}
}

func TestPickupPromptNamesEachSourcesTool(t *testing.T) {
	gh := pickupPrompt(task{Source: sourceGitHub, Identifier: "phinze/rig#9", Title: "t"}, false)
	if !strings.Contains(gh, "gh issue view 9") || strings.Contains(gh, "Linear") {
		t.Errorf("github prompt = %q", gh)
	}
	vk := pickupPrompt(task{Source: sourceVikunja, Identifier: "CTL-2", Title: "t", Domain: "ctl"}, true)
	if !strings.Contains(vk, "personal-tasks ctl show CTL-2") || !strings.Contains(vk, "../"+rigKickoffName) {
		t.Errorf("vikunja prompt = %q", vk)
	}
}
