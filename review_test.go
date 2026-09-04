package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewBookmarkPinsWorkspaceAcrossRewrittenFetch(t *testing.T) {
	for _, bin := range []string{"git", "jj"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	setHermeticGit(t)
	env := append(os.Environ(), hermeticGitVars()...)

	origin := filepath.Join(home, "origin.git")
	seed := filepath.Join(home, "seed")
	source := filepath.Join(home, "source")
	review := filepath.Join(home, "review")
	mustMkdir(t, origin)
	mustRun(t, origin, env, "git", "init", "-q", "--bare", "-b", "main")
	mustRun(t, home, env, "git", "clone", "-q", origin, seed)
	if err := os.WriteFile(filepath.Join(seed, "message.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, seed, env, "git", "add", "message.txt")
	mustRun(t, seed, env, "git", "commit", "-q", "-m", "main")
	mustRun(t, seed, env, "git", "push", "-q", "origin", "main")
	mustRun(t, seed, env, "git", "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(seed, "message.txt"), []byte("original review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, seed, env, "git", "commit", "-q", "-am", "original head")
	mustRun(t, seed, env, "git", "push", "-q", "origin", "feature")
	mustRun(t, seed, env, "git", "push", "-q", "origin", "HEAD:refs/pull/7/head")

	mustRun(t, home, env, "git", "clone", "-q", origin, source)
	mustRun(t, source, env, "jj", "git", "init", "--colocate")
	bookmark := reviewBookmarkName("pr-7", "repo")
	if err := fetchReviewHead(source, bookmark, 7); err != nil {
		t.Fatalf("fetchReviewHead: %v", err)
	}
	if err := jjWorkspaceAdd(source, "pr-7-repo", bookmark, review); err != nil {
		t.Fatalf("jjWorkspaceAdd: %v", err)
	}
	originalID := strings.TrimSpace(mustOutput(t, review, env,
		"jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id"))
	originalDiff := mustOutput(t, review, env, "jj", "diff", "--from", "main@origin", "--git")
	if !strings.Contains(originalDiff, "original review") {
		t.Fatalf("initial review diff does not contain original head:\n%s", originalDiff)
	}

	// The author replaces the PR stack, then an unrelated workspace fetches.
	// Without Rig's bookmark jj abandons the old head and rebases review @ onto
	// main; the workspace directory alone does not retain the review snapshot.
	mustRun(t, seed, env, "git", "checkout", "-q", "main")
	mustRun(t, seed, env, "git", "checkout", "-q", "-B", "feature")
	if err := os.WriteFile(filepath.Join(seed, "message.txt"), []byte("rewritten review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, seed, env, "git", "commit", "-q", "-am", "rewritten head")
	mustRun(t, seed, env, "git", "push", "-q", "--force", "origin", "feature")
	mustRun(t, seed, env, "git", "push", "-q", "--force", "origin", "HEAD:refs/pull/7/head")
	mustRun(t, source, env, "jj", "git", "fetch")

	gotID := strings.TrimSpace(mustOutput(t, review, env,
		"jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id"))
	gotDiff := mustOutput(t, review, env, "jj", "diff", "--from", "main@origin", "--git")
	if gotID != originalID {
		t.Errorf("review head moved after fetch: got %s, want %s", gotID, originalID)
	}
	if gotDiff != originalDiff {
		t.Errorf("review diff moved after fetch:\n%s", gotDiff)
	}
	tracked := mustOutput(t, source, env, "jj", "bookmark", "list",
		"--tracked", "--remote", "origin", "-T", `name ++ "\n"`, bookmark)
	if strings.TrimSpace(tracked) != "" {
		t.Errorf("review bookmark joined origin's tracked push set: %s", tracked)
	}

	// Moving the pin and rebasing the empty working-copy commit are separate,
	// explicit steps. Together they are the refresh operation; an ordinary
	// fetch above performs neither.
	if err := fetchReviewHead(source, bookmark, 7); err != nil {
		t.Fatalf("refresh fetchReviewHead: %v", err)
	}
	refreshedID, err := jjCommitID(source, bookmark)
	if err != nil {
		t.Fatalf("refreshed bookmark: %v", err)
	}
	if refreshedID == originalID {
		t.Fatal("explicit refresh left the bookmark on the original head")
	}
	if err := jjRebaseWorkspace(review, bookmark); err != nil {
		t.Fatalf("jjRebaseWorkspace: %v", err)
	}
	gotID = strings.TrimSpace(mustOutput(t, review, env,
		"jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id"))
	gotDiff = mustOutput(t, review, env, "jj", "diff", "--from", "main@origin", "--git")
	if gotID != refreshedID {
		t.Errorf("explicit refresh head = %s, want %s", gotID, refreshedID)
	}
	if !strings.Contains(gotDiff, "rewritten review") {
		t.Errorf("explicit refresh did not reveal rewritten diff:\n%s", gotDiff)
	}
}

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

func TestReviewPickerMarksRigsInsteadOfHidingThem(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "active")
	parked := filepath.Join(root, "parked")
	for path, m := range map[string]manifest{
		active: {ID: "pr-42", Kind: "review", Repos: map[string]string{"rfd": "fakeowner/rfd"}},
		parked: {ID: "pr-43", Kind: "review", Repos: map[string]string{"rfd": "fakeowner/rfd"}},
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeManifest(path, m); err != nil {
			t.Fatal(err)
		}
	}
	rigs := []rigInfo{
		{ID: "pr-42", Kind: "review", Path: active, Title: "Active review"},
		{ID: "pr-43", Kind: "review", Path: parked, Title: "Parked review", Parked: time.Now()},
	}
	requested := parseReviewSearchRows(strings.Join([]string{
		"fakeowner/rfd\t#42\tActive review",
		"fakeowner/rfd\t#43\tParked review",
		"fakeowner/other\t#42\tSame number, other repo",
	}, "\n"))

	got := mergeReviewCandidates(requested, reviewRigCandidates(rigs))

	// Fresh work leads; a PR whose number collides with a rig's in a different
	// repo is fresh work, not that rig.
	if len(got) != 3 {
		t.Fatalf("got %d candidates, want 3: %+v", len(got), got)
	}
	if got[0].pr.Repo != "other" || got[0].rig != "" {
		t.Errorf("first row = %+v, want the unrigged fakeowner/other#42", got[0])
	}
	marks := map[int]string{}
	for _, c := range got {
		if c.pr.Repo == "rfd" {
			marks[c.pr.Number] = c.rig
		}
	}
	if marks[42] != "live rig" || marks[43] != "parked rig" {
		t.Errorf("rig marks = %v, want 42 live and 43 parked", marks)
	}
	// Every PR that had a rig is still reachable: nothing was subtracted.
	for _, row := range reviewPickerRows(got) {
		if pr, err := reviewPRFromPickerRow(row); err != nil {
			t.Errorf("rendered row %q does not parse back: %v", row, err)
		} else if pr.Number == 0 {
			t.Errorf("row %q lost its PR number", row)
		}
	}
}

// A review rig survives in the picker after GitHub drops the review request,
// which it does the moment you submit a review. That rig is otherwise
// unreachable from the command that created it.
func TestReviewPickerKeepsRigsGitHubNoLongerLists(t *testing.T) {
	basedir := t.TempDir()
	m := manifest{
		ID: "pr-99", Kind: "review",
		Repos:     map[string]string{"rfd": "fakeowner/rfd"},
		ReviewPRs: map[string]string{"rfd": "https://github.com/fakeowner/rfd/pull/99"},
	}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}
	rigs := []rigInfo{{ID: "pr-99", Kind: "review", Path: basedir, Title: "Reviewed already"}}

	got := mergeReviewCandidates(nil, reviewRigCandidates(rigs))
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want the rig's PR: %+v", len(got), got)
	}
	if got[0].pr.Owner != "fakeowner" || got[0].pr.Repo != "rfd" || got[0].pr.Number != 99 {
		t.Errorf("recovered %+v, want fakeowner/rfd#99", got[0].pr)
	}
	if got[0].rig != "live rig" {
		t.Errorf("mark = %q, want live rig", got[0].rig)
	}
}

// Older review rigs predate the recorded locator, so the pr-<n> id plus the
// reviewed repository has to reconstruct it.
func TestReviewRigPRFallsBackToTheID(t *testing.T) {
	basedir := t.TempDir()
	m := manifest{ID: "pr-42", Kind: "review", Repos: map[string]string{"rfd": "fakeowner/rfd"}}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}
	pr := reviewRigPR(rigInfo{ID: "pr-42", Kind: "review", Path: basedir})
	if pr == nil {
		t.Fatal("legacy review rig could not name its PR")
	}
	if pr.Owner != "fakeowner" || pr.Repo != "rfd" || pr.Number != 42 {
		t.Errorf("recovered %+v, want fakeowner/rfd#42", *pr)
	}

	// A rig that can't name a PR is dropped, never guessed at.
	bare := t.TempDir()
	if err := writeManifest(bare, manifest{ID: "some-kickoff", Kind: "review"}); err != nil {
		t.Fatal(err)
	}
	if got := reviewRigPR(rigInfo{ID: "some-kickoff", Kind: "review", Path: bare}); got != nil {
		t.Errorf("unidentifiable review rig produced %+v", *got)
	}
}

// A multi-repo review rig records a locator per repo, and map order is random.
// The main repo is the review the rig is about; the others were added for
// research and must never win the coin toss.
func TestReviewRigPRPrefersTheMainRepoDeterministically(t *testing.T) {
	basedir := t.TempDir()
	m := manifest{
		ID: "pr-42", Kind: "review", MainRepo: "rfd",
		Repos: map[string]string{"rfd": "fakeowner/rfd", "cloud": "fakeowner/cloud"},
		ReviewPRs: map[string]string{
			"rfd":   "https://github.com/fakeowner/rfd/pull/42",
			"cloud": "https://github.com/fakeowner/cloud/pull/7",
		},
	}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}
	r := rigInfo{ID: "pr-42", Kind: "review", Path: basedir}
	for i := 0; i < 20; i++ {
		pr := reviewRigPR(r)
		if pr == nil {
			t.Fatal("multi-repo review rig could not name its PR")
		}
		if pr.Repo != "rfd" || pr.Number != 42 {
			t.Fatalf("resolved %+v on run %d, want fakeowner/rfd#42", *pr, i)
		}
	}
}
