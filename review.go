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

// runReview is the review half of the pickup pair: point it at someone else's
// PR and it drops a read-only rig around it. A URL may turn out to be your own
// PR, in which case pickupPR reroutes you to authoring (up); the no-arg picker
// only ever offers review-requested PRs, which are others' by construction, so
// it skips straight to the review pickup.
func runReview(args []string) error {
	if len(args) >= 1 {
		pr := parsePRURL(args[0])
		if pr == nil {
			return fmt.Errorf("usage: rig review [https://github.com/OWNER/REPO/pull/NUMBER]")
		}
		return pickupPR(pr, "review")
	}

	pr, err := pickReviewPR()
	if err != nil {
		return err
	}
	if pr == nil {
		return nil // picker cancelled
	}
	meta, err := prDetails(pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	return reviewPickupPR(pr, meta)
}

// pickupPR materializes a rig around an existing PR, choosing authoring vs
// review by who owns it. verb is the command the user reached for; when
// authorship disagrees with it we say so and do the right thing anyway, because
// "my work" has exactly one home (up) and "other work" exactly one (review).
func pickupPR(pr *prRef, verb string) error {
	meta, err := prDetails(pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	login, err := ghCurrentLogin()
	if err != nil {
		return err
	}
	mine := meta.Author != "" && strings.EqualFold(meta.Author, login)

	if mine {
		if verb == "review" {
			fmt.Fprintf(os.Stderr, "rig: PR #%d is yours — picking it up to author, not review\n", pr.Number)
		}
		return authorPickupPR(pr, meta)
	}
	if verb == "up" {
		fmt.Fprintf(os.Stderr, "rig: PR #%d isn't yours — setting up a review, not authoring\n", pr.Number)
	}
	return reviewPickupPR(pr, meta)
}

// reviewPickupPR builds a read-only review rig: the PR head fetched fork-safe
// via pull/N/head, kind=review (done when you've posted a review, never gated
// on merge), recto --pr showing just the branch's diff, and claude dropped
// straight into /review-pr.
func reviewPickupPR(pr *prRef, meta prMeta) error {
	repoPath, err := ensureGhqClone(pr.Owner, pr.Repo)
	if err != nil {
		return err
	}
	if err := ensureJJColocated(repoPath); err != nil {
		return fmt.Errorf("colocating jj on %s: %w", repoPath, err)
	}
	if err := fetchPRHead(repoPath, meta.Branch, pr.Number); err != nil {
		return err
	}

	// Task id is just pr-<n>: jj workspace names get the repo appended and are
	// registered per source repo, and the basedir gets the title slug, so the
	// repo name isn't needed for uniqueness anywhere the id travels. The slug
	// derives from the PR title (Linear-style id-plus-title shape) rather than
	// the branch, which often embeds a whole ticket slug of its own.
	rigID := fmt.Sprintf("pr-%d", pr.Number)
	basedir, err := basedirPath(taskSlug(rigID, meta.Title))
	if err != nil {
		return err
	}

	m := manifest{ID: rigID, Title: meta.Title, Kind: "review"}
	if err := createBasedir(basedir, m); err != nil {
		return err
	}

	repo := repoRef{Owner: pr.Owner, Name: pr.Repo, Path: repoPath}
	repoDest, err := addRepoWorkspace(basedir, rigID, repo, meta.Branch, meta.Branch)
	if err != nil {
		return err
	}

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

// prRigIdentity derives an authoring pickup's rig id and basedir slug from the
// PR's branch, so a PR that was born from an issue rebuilds under the SAME id
// and path its `rig up <issue>` used. Linear stamps its id into the branch
// (phinze/mir-75-add-zig-stack), which the issue flow turned into id "mir-75"
// and basedir "mir-75-add-zig-stack"; recovering both here means a resumed rig
// lands at the exact cwd where you built it, so claude --resume finds those
// sessions (claude keys history by cwd, and cwd is <basedir>/<repo>). A branch
// with no issue id (a PR not born from a tracker) has no prior rig to match, so
// it falls back to pr-<n> + the PR title.
func prRigIdentity(pr *prRef, meta prMeta) (rigID, basedirName string) {
	slug := stripBranchUserPrefix(meta.Branch)
	if id := leadingIssueID(slug); id != "" {
		return id, slug
	}
	rigID = fmt.Sprintf("pr-%d", pr.Number)
	return rigID, taskSlug(rigID, meta.Title)
}

// authorPickupPR builds an authoring rig around your own PR: you're coming back
// to keep working, not to review. It starts the workspace at branch@origin (via
// resolveStartRev) so your pushed commits come back and you stay on a pushable
// branch — the crucial difference from review's read-only pull/N/head — and
// leaves kind unset (authoring: done when the work merges). Identity comes from
// the branch (see prRigIdentity), so it's idempotent against the rig its
// originating issue-up created: a live one is switched to, a down'd one rebuilds
// at the same path for claude --resume.
func authorPickupPR(pr *prRef, meta prMeta) error {
	rigID, basedirName := prRigIdentity(pr, meta)
	if done, err := attachExistingRig(rigID); err != nil {
		return err
	} else if done {
		return nil
	}

	repoPath, err := ensureGhqClone(pr.Owner, pr.Repo)
	if err != nil {
		return err
	}
	if err := ensureJJColocated(repoPath); err != nil {
		return fmt.Errorf("colocating jj on %s: %w", repoPath, err)
	}

	// Your branch lives on origin, so start there: resolveStartRev fetches then
	// prefers branch@origin, recovering whatever you'd pushed even if the local
	// rig was long gone. The durable state was always the PR plus origin.
	startRev := resolveStartRev(repoPath, meta.Branch)

	basedir, err := basedirPath(basedirName)
	if err != nil {
		return err
	}

	m := manifest{ID: rigID, Title: meta.Title} // kind "" = authoring
	if err := createBasedir(basedir, m); err != nil {
		return err
	}

	repo := repoRef{Owner: pr.Owner, Name: pr.Repo, Path: repoPath}
	repoDest, err := addRepoWorkspace(basedir, rigID, repo, startRev, meta.Branch)
	if err != nil {
		return err
	}

	sess := sessionSpec{
		rectoCmd: "recto",
		prompt: fmt.Sprintf(
			"Resuming your PR #%d (%s) on its branch in a dedicated jj workspace. Read the PR and any review feedback, then help me address it.",
			pr.Number, meta.Title,
		),
	}
	session, err := spawnSession(basedir, repoDest, sess)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "rig: up %s/%s#%d — %s\n", pr.Owner, pr.Repo, pr.Number, basedir)
	return attachOrReport(session)
}

type prRef struct {
	Owner  string
	Repo   string
	Number int
}

var prURLRe = regexp.MustCompile(`github\.com/([^/]+)/([^/]+)/pull/([0-9]+)`)

// parsePRURL pulls an owner/repo/number out of a GitHub PR URL, or returns nil
// when the string isn't one. Shared by `rig up` (a PR URL means "author my own
// PR") and `rig review` (a PR URL to review).
func parsePRURL(s string) *prRef {
	m := prURLRe.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	n, _ := strconv.Atoi(m[3])
	return &prRef{Owner: m[1], Repo: m[2], Number: n}
}

// pickReviewPR opens an fzf picker over the open PRs awaiting your review. These
// are others' work by construction (you don't request review from yourself), so
// callers skip the authorship check and go straight to the review pickup.
// Returns nil (no error) when the picker is cancelled.
func pickReviewPR() (*prRef, error) {
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

// prMeta is what a single `gh pr view` tells us about a PR at pickup time: its
// head branch, title, and author login. Author is here so the authorship split
// (authoring vs review) rides the same gh call as the branch/title we need
// anyway, no extra round-trip.
type prMeta struct {
	Branch string
	Title  string
	Author string
}

// prDetails fetches a PR's head branch, title, and author login via gh.
func prDetails(owner, repo string, number int) (prMeta, error) {
	out, err := exec.Command("gh", "pr", "view", strconv.Itoa(number),
		"-R", owner+"/"+repo, "--json", "headRefName,title,author").Output()
	if err != nil {
		return prMeta{}, fmt.Errorf("gh pr view %s/%s#%d: %w", owner, repo, number, err)
	}
	var v struct {
		HeadRefName string `json:"headRefName"`
		Title       string `json:"title"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return prMeta{}, fmt.Errorf("parsing gh pr view output: %w", err)
	}
	if v.HeadRefName == "" {
		return prMeta{}, fmt.Errorf("no head branch for %s/%s#%d", owner, repo, number)
	}
	return prMeta{Branch: v.HeadRefName, Title: v.Title, Author: v.Author.Login}, nil
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
