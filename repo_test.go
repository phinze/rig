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
