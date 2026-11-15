package main

import (
	"reflect"
	"testing"
)

// fakeStack answers the lister from a fixed set of open PRs.
func fakeStack(prs ...stackPR) stackLister {
	return func(_, _, by, branch string) ([]stackPR, error) {
		var out []stackPR
		for _, pr := range prs {
			if (by == "head" && pr.HeadRefName == branch) || (by == "base" && pr.BaseRefName == branch) {
				out = append(out, pr)
			}
		}
		return out, nil
	}
}

func chainNumbers(chain []*prRef) []int {
	var out []int
	for _, pr := range chain {
		out = append(out, pr.Number)
	}
	return out
}

// From any PR in a stack the walk finds the whole of it, bottom first:
// down through the PR whose head is the base, up through the one based on
// the head, stopping at trunk and at the top.
func TestReviewChainWalksAStackBothWays(t *testing.T) {
	stack := []stackPR{
		{Number: 41, HeadRefName: "one", BaseRefName: "main"},
		{Number: 42, HeadRefName: "two", BaseRefName: "one"},
		{Number: 43, HeadRefName: "three", BaseRefName: "two"},
		{Number: 99, HeadRefName: "elsewhere", BaseRefName: "main"},
	}
	list := fakeStack(stack...)
	for _, start := range stack[:3] {
		pr := &prRef{Owner: "o", Repo: "r", Number: start.Number}
		chain, err := reviewChain(pr, start, list)
		if err != nil {
			t.Fatal(err)
		}
		if got := chainNumbers(chain); !reflect.DeepEqual(got, []int{41, 42, 43}) {
			t.Errorf("from #%d: got %v", start.Number, got)
		}
	}

	lone := stack[3]
	chain, _ := reviewChain(&prRef{Owner: "o", Repo: "r", Number: 99}, lone, list)
	if got := chainNumbers(chain); !reflect.DeepEqual(got, []int{99}) {
		t.Errorf("a PR in no stack is a chain of one: got %v", got)
	}
}

// Two PRs based on the same head is a fork, not a stack: the walk stops
// below it rather than guess which one is the reader's.
func TestReviewChainStopsAtAFork(t *testing.T) {
	list := fakeStack(
		stackPR{Number: 41, HeadRefName: "one", BaseRefName: "main"},
		stackPR{Number: 42, HeadRefName: "two", BaseRefName: "one"},
		stackPR{Number: 43, HeadRefName: "three", BaseRefName: "one"},
	)
	chain, _ := reviewChain(&prRef{Owner: "o", Repo: "r", Number: 41},
		stackPR{Number: 41, HeadRefName: "one", BaseRefName: "main"}, list)
	if got := chainNumbers(chain); !reflect.DeepEqual(got, []int{41}) {
		t.Errorf("got %v", got)
	}
}

// A fork PR whose branch shares a name with this repository's branch is
// somebody else's work, not a layer of the stack.
func TestReviewChainIgnoresForkBranchesOfTheSameName(t *testing.T) {
	list := fakeStack(
		stackPR{Number: 7, HeadRefName: "main", BaseRefName: "main", IsCrossRepository: true},
		stackPR{Number: 41, HeadRefName: "one", BaseRefName: "main"},
		stackPR{Number: 8, HeadRefName: "x", BaseRefName: "one", IsCrossRepository: true},
	)
	chain, _ := reviewChain(&prRef{Owner: "o", Repo: "r", Number: 41},
		stackPR{Number: 41, HeadRefName: "one", BaseRefName: "main"}, list)
	if got := chainNumbers(chain); !reflect.DeepEqual(got, []int{41}) {
		t.Errorf("got %v", got)
	}
}

// Explicit stacks stay in one repository, in the order given.
func TestParseStackArgs(t *testing.T) {
	chain, err := parseStackArgs([]string{
		"https://github.com/o/r/pull/41",
		"https://github.com/o/r/pull/42",
	})
	if err != nil || !reflect.DeepEqual(chainNumbers(chain), []int{41, 42}) {
		t.Fatalf("got %v, %v", chainNumbers(chain), err)
	}
	if _, err := parseStackArgs([]string{
		"https://github.com/o/r/pull/41",
		"https://github.com/o/other/pull/42",
	}); err == nil {
		t.Fatal("a stack across repositories is refused")
	}
}

// A stack review is named for its top PR, but any PR of the stack finds it,
// the manifest keeps the stack, and rig info hands it to Recto bottom first.
func TestAStackReviewIsFoundByAnyOfItsPRs(t *testing.T) {
	basedir := t.TempDir()
	stack := []string{
		"https://github.com/o/r/pull/41",
		"https://github.com/o/r/pull/42",
	}
	m := manifest{
		ID:          "pr-42",
		Kind:        "review",
		Repos:       map[string]string{"r": "o/r"},
		Branches:    map[string][]string{"r": {"two"}},
		ReviewPRs:   map[string]string{"r": stack[1]},
		ReviewStack: map[string][]string{"r": stack},
	}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}
	reread, err := readManifest(basedir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reread.ReviewStack["r"], stack) {
		t.Fatalf("the manifest keeps the stack: got %v", reread.ReviewStack)
	}

	rigs := []rigInfo{{ID: "pr-42", Path: basedir}}
	for _, n := range []int{41, 42} {
		found, err := existingReviewRig(rigs, &prRef{Owner: "o", Repo: "r", Number: n})
		if err != nil || found == nil {
			t.Fatalf("#%d should find the stack review: %v, %v", n, found, err)
		}
	}
	if found, _ := existingReviewRig(rigs, &prRef{Owner: "o", Repo: "r", Number: 43}); found != nil {
		t.Fatal("a PR outside the stack doesn't")
	}

	ctx := rigContextFor(basedir, basedir+"/r", reread)
	if !reflect.DeepEqual(ctx.ReviewPRs, stack) || ctx.ReviewPR != stack[1] {
		t.Fatalf("rig info: review_pr %q, review_prs %v", ctx.ReviewPR, ctx.ReviewPRs)
	}
}

// Each lower head is pinned beside the workspace's own bookmark, never under
// it, since a Git ref can't be both a branch and a directory.
func TestStackBookmarksAreSiblingsOfTheReviewBookmark(t *testing.T) {
	top := reviewBookmarkName("pr-42", "r")
	lower := stackBookmarkName("pr-42", "r", 41)
	if lower == top || len(lower) <= len(top) || lower[:len(top)] != top || lower[len(top)] == '/' {
		t.Fatalf("top %q, lower %q", top, lower)
	}
}
