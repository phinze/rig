package main

import (
	"path/filepath"
	"testing"
)

func TestRigContextExposesCurrentReviewRepo(t *testing.T) {
	base := filepath.Join("", "workspaces", "pr-42-fix")
	m := manifest{
		ID: "pr-42", Kind: "review", MainRepo: "rfd",
		ReviewPRs: map[string]string{
			"cloud": "https://github.com/fakeowner/cloud/pull/42",
			"rfd":   "https://github.com/fakeowner/rfd/pull/7",
		},
		Repos: map[string]string{
			"cloud": "fakeowner/cloud",
			"rfd":   "fakeowner/rfd",
		},
	}
	got := rigContextFor(base, filepath.Join(base, "cloud", "internal"), m)
	if got.SchemaVersion != 1 || got.ID != "pr-42" || got.Kind != "review" {
		t.Fatalf("identity = %+v", got)
	}
	if got.Repo != "cloud" || got.Repository != "fakeowner/cloud" {
		t.Errorf("repository context = %+v", got)
	}
	if got.ReviewPR != m.ReviewPRs["cloud"] {
		t.Errorf("review_pr = %q, want %q", got.ReviewPR, m.ReviewPRs["cloud"])
	}
}

func TestRigContextKeepsManifestInternalsBehindCompatibilityAPI(t *testing.T) {
	base := filepath.Join("", "workspaces", "pr-225-fix")
	m := manifest{
		ID: "pr-225", Kind: "review", MainRepo: "cloud",
		Repos: map[string]string{"cloud": "mirendev/cloud"},
	}
	got := rigContextFor(base, filepath.Join(base, "cloud"), m)
	if got.ReviewPR != "https://github.com/mirendev/cloud/pull/225" {
		t.Errorf("legacy review_pr = %q", got.ReviewPR)
	}

	authoring := m
	authoring.Kind = ""
	if got := rigContextFor(base, filepath.Join(base, "cloud"), authoring); got.ReviewPR != "" {
		t.Errorf("authoring rig exposed review_pr %q", got.ReviewPR)
	}
}

func TestRigContextDoesNotAttachReviewToAnotherRepo(t *testing.T) {
	base := filepath.Join("", "workspaces", "pr-42-fix")
	m := manifest{
		ID: "pr-42", Kind: "review", MainRepo: "cloud",
		ReviewPRs: map[string]string{
			"cloud": "https://github.com/fakeowner/cloud/pull/42",
		},
		Repos: map[string]string{
			"cloud": "fakeowner/cloud",
			"rfd":   "fakeowner/rfd",
		},
	}
	got := rigContextFor(base, filepath.Join(base, "rfd"), m)
	if got.ReviewPR != "" {
		t.Errorf("secondary repo exposed main review PR %q", got.ReviewPR)
	}
}

func TestRigContextCanExposeOneReviewPerRepo(t *testing.T) {
	base := filepath.Join("", "workspaces", "pr-42-fix")
	m := manifest{
		ID: "pr-42", Kind: "review",
		ReviewPRs: map[string]string{
			"cloud": "https://github.com/fakeowner/cloud/pull/42",
			"rfd":   "https://github.com/fakeowner/rfd/pull/7",
		},
		Repos: map[string]string{
			"cloud": "fakeowner/cloud",
			"rfd":   "fakeowner/rfd",
		},
	}
	got := rigContextFor(base, filepath.Join(base, "rfd"), m)
	if got.ReviewPR != m.ReviewPRs["rfd"] {
		t.Errorf("second review_pr = %q, want %q", got.ReviewPR, m.ReviewPRs["rfd"])
	}
}

func TestLegacyMultiRepoReviewNeedsExplicitLocator(t *testing.T) {
	base := filepath.Join("", "workspaces", "pr-42-fix")
	m := manifest{
		ID: "pr-42", Kind: "review",
		Repos: map[string]string{
			"cloud": "fakeowner/cloud",
			"rfd":   "fakeowner/rfd",
		},
	}
	if got := rigContextFor(base, filepath.Join(base, "cloud"), m); got.ReviewPR != "" {
		t.Errorf("ambiguous legacy rig exposed review_pr %q", got.ReviewPR)
	}
}
