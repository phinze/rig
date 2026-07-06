package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeGh installs a stub `gh` on PATH for the duration of the test. Its
// behavior is steered by env vars the test sets: GH_FAKE_STATE picks the PR
// state it reports, GH_FAKE_REVIEW sets its reviewDecision, GH_FAKE_NOPR makes
// it answer "no pull requests found", and GH_FAKE_ERR makes it fail like an
// offline/unauthorized gh. For the review-rig path, GH_FAKE_REVIEWS supplies the
// `reviews` array a `--json reviews` query returns (default empty), and
// GH_FAKE_LOGIN sets the login `gh api user` reports (default "me").
func fakeGh(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	// GH_FAKE_ROLLUP steers the emitted statusCheckRollup (defaults to a single
	// passing CheckRun); tests set it to "" for a bare PR or a failing item.
	script := `#!/bin/sh
if [ "$1" = "api" ] && [ "$2" = "user" ]; then
  printf '%s\n' "${GH_FAKE_LOGIN:-me}"
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  if [ -n "$GH_FAKE_ERR" ]; then
    echo "could not connect to github.com" >&2
    exit 1
  fi
  if [ -n "$GH_FAKE_NOPR" ]; then
    echo "no pull requests found for branch \"x\"" >&2
    exit 1
  fi
  case "$*" in
  *"--json reviews"*)
    printf '{"reviews":%s}\n' "${GH_FAKE_REVIEWS:-[]}"
    exit 0
    ;;
  esac
  rollup='[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}]'
  if [ -n "${GH_FAKE_ROLLUP+set}" ]; then rollup="$GH_FAKE_ROLLUP"; fi
  printf '{"number":7,"state":"%s","url":"https://github.com/o/r/pull/7","statusCheckRollup":%s,"reviewDecision":"%s"}\n' \
    "${GH_FAKE_STATE:-OPEN}" "${rollup:-[]}" "${GH_FAKE_REVIEW:-}"
  exit 0
fi
echo "fake gh: unsupported invocation $*" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPrForBranch(t *testing.T) {
	fakeGh(t)

	t.Run("merged with passing checks", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "MERGED")
		pr, err := prForBranch("o/r", "feat")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pr == nil || pr.State != "MERGED" || pr.Number != 7 || pr.Checks != "passing" {
			t.Fatalf("got %+v, want a MERGED PR #7 with passing checks", pr)
		}
	})

	t.Run("review decision comes through", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "OPEN")
		t.Setenv("GH_FAKE_REVIEW", "CHANGES_REQUESTED")
		pr, err := prForBranch("o/r", "feat")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pr == nil || pr.Review != "CHANGES_REQUESTED" {
			t.Fatalf("got %+v, want Review=CHANGES_REQUESTED", pr)
		}
	})

	t.Run("no PR for branch is not an error", func(t *testing.T) {
		t.Setenv("GH_FAKE_NOPR", "1")
		pr, err := prForBranch("o/r", "feat")
		if err != nil {
			t.Fatalf("expected nil error for missing PR, got %v", err)
		}
		if pr != nil {
			t.Fatalf("expected nil PR, got %+v", pr)
		}
	})

	t.Run("gh failure propagates", func(t *testing.T) {
		t.Setenv("GH_FAKE_ERR", "1")
		if _, err := prForBranch("o/r", "feat"); err == nil {
			t.Fatal("expected an error when gh fails")
		}
	})
}

func TestReviewSubmittedByMe(t *testing.T) {
	fakeGh(t)

	t.Run("my submitted review counts", func(t *testing.T) {
		t.Setenv("GH_FAKE_REVIEWS", `[{"author":{"login":"me"},"state":"CHANGES_REQUESTED"}]`)
		got, err := reviewSubmittedByMe("o/r", "feat", "me")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got {
			t.Error("expected my CHANGES_REQUESTED review to count")
		}
	})

	t.Run("a still-pending draft doesn't count", func(t *testing.T) {
		t.Setenv("GH_FAKE_REVIEWS", `[{"author":{"login":"me"},"state":"PENDING"}]`)
		got, err := reviewSubmittedByMe("o/r", "feat", "me")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got {
			t.Error("expected an unsent PENDING draft not to count as reviewed")
		}
	})

	t.Run("another user's review doesn't count", func(t *testing.T) {
		t.Setenv("GH_FAKE_REVIEWS", `[{"author":{"login":"someone-else"},"state":"APPROVED"}]`)
		got, err := reviewSubmittedByMe("o/r", "feat", "me")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got {
			t.Error("expected another user's review not to count as mine")
		}
	})

	t.Run("no PR for branch reads as not-yet, no error", func(t *testing.T) {
		t.Setenv("GH_FAKE_NOPR", "1")
		got, err := reviewSubmittedByMe("o/r", "feat", "me")
		if err != nil {
			t.Fatalf("expected nil error for missing PR, got %v", err)
		}
		if got {
			t.Error("expected a missing PR to read as not-yet-reviewed")
		}
	})

	t.Run("gh failure propagates", func(t *testing.T) {
		t.Setenv("GH_FAKE_ERR", "1")
		if _, err := reviewSubmittedByMe("o/r", "feat", "me"); err == nil {
			t.Fatal("expected an error when gh fails")
		}
	})
}

func TestGhCurrentLogin(t *testing.T) {
	fakeGh(t)
	t.Setenv("GH_FAKE_LOGIN", "octocat")
	login, err := ghCurrentLogin()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if login != "octocat" {
		t.Fatalf("got %q, want octocat", login)
	}
}

func TestRollupChecks(t *testing.T) {
	cases := []struct {
		name  string
		items []checkItem
		want  string
	}{
		{"no checks", nil, ""},
		{"all green", []checkItem{
			{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "SKIPPED"},
		}, "passing"},
		{"a failure dominates", []checkItem{
			{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "FAILURE"},
		}, "failing"},
		{"in-progress reads pending", []checkItem{
			{Typename: "CheckRun", Status: "IN_PROGRESS"},
			{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "SUCCESS"},
		}, "pending"},
		{"failure beats pending", []checkItem{
			{Typename: "CheckRun", Status: "IN_PROGRESS"},
			{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "FAILURE"},
		}, "failing"},
		{"legacy status context error", []checkItem{
			{Typename: "StatusContext", State: "ERROR"},
		}, "failing"},
		{"legacy status context pending", []checkItem{
			{Typename: "StatusContext", State: "PENDING"},
		}, "pending"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rollupChecks(c.items); got != c.want {
				t.Errorf("rollupChecks = %q, want %q", got, c.want)
			}
		})
	}
}
