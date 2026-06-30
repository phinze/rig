package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeGh installs a stub `gh` on PATH for the duration of the test. Its
// behavior is steered by env vars the test sets: GH_FAKE_STATE picks the PR
// state it reports, GH_FAKE_NOPR makes it answer "no pull requests found", and
// GH_FAKE_ERR makes it fail like an offline/unauthorized gh.
func fakeGh(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  if [ -n "$GH_FAKE_ERR" ]; then
    echo "could not connect to github.com" >&2
    exit 1
  fi
  if [ -n "$GH_FAKE_NOPR" ]; then
    echo "no pull requests found for branch \"x\"" >&2
    exit 1
  fi
  printf '{"number":7,"state":"%s","url":"https://github.com/o/r/pull/7"}\n' "${GH_FAKE_STATE:-OPEN}"
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

	t.Run("merged", func(t *testing.T) {
		t.Setenv("GH_FAKE_STATE", "MERGED")
		pr, err := prForBranch("o/r", "feat")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pr == nil || pr.State != "MERGED" || pr.Number != 7 {
			t.Fatalf("got %+v, want a MERGED PR #7", pr)
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
