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

func TestReviewPickerHidesInFlightRigsAndKeepsParkedRigs(t *testing.T) {
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
		{ID: "pr-42", Path: active},
		{ID: "pr-43", Path: parked, Parked: time.Now()},
	}
	rows := []string{
		"fakeowner/rfd\t#42\tActive review\thttps://github.com/fakeowner/rfd/pull/42",
		"fakeowner/rfd\t#43\tParked review\thttps://github.com/fakeowner/rfd/pull/43",
		"fakeowner/other\t#42\tSame number, other repo\thttps://github.com/fakeowner/other/pull/42",
	}

	got, err := withoutInFlightReviewRigs(rows, rigs)
	if err != nil {
		t.Fatal(err)
	}
	want := rows[1:]
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("filtered rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
