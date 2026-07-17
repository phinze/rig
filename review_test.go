package main

import "testing"

func TestExistingReviewRigUsesTrackedRepoIdentity(t *testing.T) {
	basedir := t.TempDir()
	m := manifest{
		ID:   "pr-42",
		Kind: "review",
		Repos: map[string]string{
			"rfd":     "fakeowner/rfd",
			"runtime": "fakeowner/runtime",
		},
		Branches: map[string][]string{
			"rfd": {"rfd-branch"},
		},
	}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}
	rigs := []rigInfo{{ID: "pr-42", Path: basedir}}

	found, err := existingReviewRig(rigs, &prRef{Owner: "fakeowner", Repo: "rfd", Number: 42})
	if err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatal("expected the tracked primary repo to identify the review rig")
	}

	found, err = existingReviewRig(rigs, &prRef{Owner: "fakeowner", Repo: "runtime", Number: 42})
	if err != nil {
		t.Fatal(err)
	}
	if found != nil {
		t.Fatal("an untracked research repo must not identify the review rig")
	}
}

func TestExistingReviewRigAcceptsLegacySingleRepoManifest(t *testing.T) {
	basedir := t.TempDir()
	m := manifest{
		ID:    "pr-42",
		Kind:  "review",
		Repos: map[string]string{"rfd": "fakeowner/rfd"},
	}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}
	rigs := []rigInfo{{ID: "pr-42", Path: basedir}}

	found, err := existingReviewRig(rigs, &prRef{Owner: "fakeowner", Repo: "rfd", Number: 42})
	if err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatal("expected a legacy single-repo review manifest to match")
	}
}
