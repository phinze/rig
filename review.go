package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// runReview is the jreview sibling of runUp: it resolves a PR, makes sure the
// repo is cloned and colocated, fetches the PR head (fork-safe), drops a jj
// workspace on that branch, and spawns a recto --pr + /review-pr session.
func runReview(args []string) error {
	pr, err := resolvePR(args)
	if err != nil {
		return err
	}
	if pr == nil {
		return nil // picker cancelled
	}

	branch, title, err := prDetails(pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return err
	}

	repoPath, err := ensureGhqClone(pr.Owner, pr.Repo)
	if err != nil {
		return err
	}

	if err := ensureJJColocated(repoPath); err != nil {
		return fmt.Errorf("colocating jj on %s: %w", repoPath, err)
	}

	if err := fetchPRHead(repoPath, branch, pr.Number); err != nil {
		return err
	}

	// Task id is just pr-<n>: jj workspace names get the repo appended and are
	// registered per source repo, and the basedir gets the title slug, so the
	// repo name isn't needed for uniqueness anywhere the id travels. The slug
	// derives from the PR title (Linear-style id-plus-title shape) rather than
	// the branch, which often embeds a whole ticket slug of its own.
	rigID := fmt.Sprintf("pr-%d", pr.Number)
	basedirName := taskSlug(rigID, title)
	basedir, err := basedirPath(basedirName)
	if err != nil {
		return err
	}

	m := manifest{ID: rigID, Title: title}
	if err := createBasedir(basedir, m); err != nil {
		return err
	}

	repo := repoRef{Owner: pr.Owner, Name: pr.Repo, Path: repoPath}
	repoDest, err := addRepoWorkspace(basedir, rigID, repo, branch)
	if err != nil {
		return err
	}

	// recto --pr shows just the branch's diff (merge-base against trunk), and
	// claude jumps straight into the review skill since we're already parked
	// on the PR branch in a dedicated workspace.
	sess := sessionSpec{
		rectoCmd: "recto --pr",
		prompt: fmt.Sprintf(
			"/review-pr %d — you are already on the PR branch in a dedicated jj workspace; skip branch verification",
			pr.Number,
		),
	}
	session, err := spawnSession(basedir, repoDest, sess)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "rig: review %s/%s#%d — %s\n", pr.Owner, pr.Repo, pr.Number, basedir)
	return attachOrReport(session)
}

type prRef struct {
	Owner  string
	Repo   string
	Number int
}

var prURLRe = regexp.MustCompile(`github\.com/([^/]+)/([^/]+)/pull/([0-9]+)`)

// resolvePR turns `rig review` args into a PR reference. A GitHub PR URL is
// parsed directly; no args opens an fzf picker over PRs awaiting your review.
// Returns nil (no error) when the picker is cancelled.
func resolvePR(args []string) (*prRef, error) {
	if len(args) >= 1 {
		m := prURLRe.FindStringSubmatch(args[0])
		if m == nil {
			return nil, fmt.Errorf("usage: rig review [https://github.com/OWNER/REPO/pull/NUMBER]")
		}
		n, _ := strconv.Atoi(m[3])
		return &prRef{Owner: m[1], Repo: m[2], Number: n}, nil
	}

	out, err := exec.Command("gh", "search", "prs",
		"--review-requested=@me", "--state=open",
		"--json", "repository,number,title,url",
		"--jq", `.[] | "\(.repository.nameWithOwner)\t#\(.number)\t\(.title)\t\(.url)"`,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("gh search prs: %w", err)
	}
	rows := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(rows) == 0 || (len(rows) == 1 && rows[0] == "") {
		return nil, fmt.Errorf("no open PRs awaiting your review")
	}

	sel, err := fzfSelect(rows, "Review PR: ")
	if err != nil {
		return nil, err
	}
	if sel == "" {
		return nil, nil
	}

	cols := strings.SplitN(sel, "\t", 4)
	if len(cols) < 2 {
		return nil, fmt.Errorf("unexpected picker selection: %q", sel)
	}
	owner, repo, ok := strings.Cut(cols[0], "/")
	if !ok {
		return nil, fmt.Errorf("unexpected repo in selection: %q", cols[0])
	}
	n, err := strconv.Atoi(strings.TrimPrefix(cols[1], "#"))
	if err != nil {
		return nil, fmt.Errorf("unexpected PR number in selection: %q", cols[1])
	}
	return &prRef{Owner: owner, Repo: repo, Number: n}, nil
}

// prDetails fetches the PR's head branch and title via gh.
func prDetails(owner, repo string, number int) (branch, title string, err error) {
	out, err := exec.Command("gh", "pr", "view", strconv.Itoa(number),
		"-R", owner+"/"+repo, "--json", "headRefName,title").Output()
	if err != nil {
		return "", "", fmt.Errorf("gh pr view %s/%s#%d: %w", owner, repo, number, err)
	}
	var v struct {
		HeadRefName string `json:"headRefName"`
		Title       string `json:"title"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", "", fmt.Errorf("parsing gh pr view output: %w", err)
	}
	if v.HeadRefName == "" {
		return "", "", fmt.Errorf("no head branch for %s/%s#%d", owner, repo, number)
	}
	return v.HeadRefName, v.Title, nil
}

// ensureGhqClone makes sure owner/repo is cloned under the ghq root, cloning
// it if needed, and returns the absolute (symlink-resolved) repo path.
func ensureGhqClone(owner, repo string) (string, error) {
	rootOut, err := exec.Command("ghq", "root").Output()
	if err != nil {
		return "", fmt.Errorf("ghq root: %w", err)
	}
	root := strings.TrimSpace(string(rootOut))
	path := filepath.Join(root, "github.com", owner, repo)

	if !dirExists(path) {
		fmt.Fprintf(os.Stderr, "rig: ghq get github.com/%s/%s\n", owner, repo)
		cmd := exec.Command("ghq", "get", fmt.Sprintf("github.com/%s/%s", owner, repo))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("ghq get %s/%s: %w", owner, repo, err)
		}
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path, nil
}

// fetchPRHead fetches the PR's head commit into a local branch. Using the
// pull/N/head ref works for fork PRs too, where the head branch isn't on
// origin. Skips the fetch if we already have the branch (git errors on a
// colon-form fetch into an existing ref).
func fetchPRHead(repoPath, branch string, number int) error {
	if gitHasBranch(repoPath, branch) {
		return nil
	}
	spec := fmt.Sprintf("pull/%d/head:%s", number, branch)
	cmd := exec.Command("git", "-C", repoPath, "fetch", "origin", spec, "--quiet")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fetching pull/%d/head: %w", number, err)
	}
	return nil
}

func gitHasBranch(repoPath, branch string) bool {
	return exec.Command("git", "-C", repoPath,
		"show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}
