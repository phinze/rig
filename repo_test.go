package main

import "testing"

func TestRankReposByFrecency(t *testing.T) {
	repos := []repoRef{
		{Name: "alpha", Path: "/src/github.com/me/alpha"},
		{Name: "bravo", Path: "/src/github.com/me/bravo"},
		{Name: "charlie", Path: "/src/github.com/me/charlie"},
	}
	// zoxide says charlie is most frecent, then alpha; bravo is unseen.
	dirs := []string{
		"/src/github.com/me/charlie",
		"/src/github.com/me/alpha",
	}
	rankReposByFrecency(repos, dirs)

	got := []string{repos[0].Name, repos[1].Name, repos[2].Name}
	want := []string{"charlie", "alpha", "bravo"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rank order = %v, want %v", got, want)
		}
	}
}

func TestRepoCandidates(t *testing.T) {
	ghq := []repoRef{
		{Name: "alpha", Path: "/p/alpha"},
		{Name: "bravo", Path: "/p/bravo"},
	}

	// No cwd repo: candidates are just the ghq list, untouched.
	if got := repoCandidates(nil, ghq); len(got) != 2 || got[0].Name != "alpha" {
		t.Fatalf("nil cwd = %v, want the ghq list unchanged", got)
	}

	// cwd repo already in the ghq list: it moves to the front, no duplicate.
	cwd := repoRef{Name: "bravo", Path: "/p/bravo"}
	got := repoCandidates(&cwd, ghq)
	if len(got) != 2 {
		t.Fatalf("deduped candidates = %d entries, want 2: %v", len(got), got)
	}
	if got[0].Path != "/p/bravo" {
		t.Errorf("cwd repo not first: %v", got)
	}

	// cwd repo outside the ghq list: prepended, list otherwise intact.
	outside := repoRef{Name: "charlie", Path: "/elsewhere/charlie"}
	got = repoCandidates(&outside, ghq)
	want := []string{"charlie", "alpha", "bravo"}
	if len(got) != 3 {
		t.Fatalf("candidates = %d entries, want 3: %v", len(got), got)
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("candidate order = %v, want %v", got, want)
		}
	}
}

// Repos zoxide has never seen keep their original relative order behind the
// ranked ones, rather than being shuffled among themselves.
func TestRankReposByFrecencyStableTail(t *testing.T) {
	repos := []repoRef{
		{Name: "one", Path: "/p/one"},
		{Name: "two", Path: "/p/two"},
		{Name: "three", Path: "/p/three"},
	}
	rankReposByFrecency(repos, nil) // nothing ranked

	want := []string{"one", "two", "three"}
	for i, r := range repos {
		if r.Name != want[i] {
			t.Fatalf("unranked order = %v at %d, want %v", r.Name, i, want)
		}
	}
}
