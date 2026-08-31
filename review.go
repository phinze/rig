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
	refresh := false
	filtered := args[:0]
	for _, arg := range args {
		if arg == "--refresh" {
			refresh = true
			continue
		}
		filtered = append(filtered, arg)
	}
	args = filtered
	pick, args, err := extractAgentFlag(args)
	if err != nil {
		return err
	}
	defer pick.cleanup()
	if refresh {
		if len(args) != 1 {
			return fmt.Errorf("usage: rig review --refresh https://github.com/OWNER/REPO/pull/NUMBER")
		}
		pr := parsePRURL(args[0])
		if pr == nil {
			return fmt.Errorf("usage: rig review --refresh https://github.com/OWNER/REPO/pull/NUMBER")
		}
		return refreshReviewRig(pr)
	}
	if len(args) >= 1 {
		pr := parsePRURL(args[0])
		if pr == nil {
			return fmt.Errorf("usage: rig review [https://github.com/OWNER/REPO/pull/NUMBER]")
		}
		return pickupPR(pr, "review", pick)
	}

	pr, err := pickReviewPR(pick)
	if err != nil {
		return err
	}
	if pr == nil {
		return nil // picker cancelled
	}
	if done, err := attachExistingReviewRig(pr); err != nil {
		return err
	} else if done {
		return nil
	}
	meta, err := prDetails(pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	return reviewPickupPR(pr, meta, pick)
}

// refreshReviewRig is the only path that moves an existing review snapshot.
// Fetching elsewhere stays harmless because the reserved bookmark keeps the
// old PR head reachable; this command deliberately advances that bookmark and
// rebases the empty review working-copy commit onto the new head.
func refreshReviewRig(pr *prRef) error {
	rigs, err := listRigs()
	if err != nil {
		return err
	}
	found, err := existingReviewRig(rigs, pr)
	if err != nil {
		return err
	}
	if found == nil {
		return fmt.Errorf("no review rig found for %s", pr.URL())
	}
	m, err := readManifest(found.Path)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	repoName := reviewRepoForPR(m, pr)
	if repoName == "" {
		return fmt.Errorf("review rig has no repository for %s", pr.URL())
	}
	workspace := filepath.Join(found.Path, repoName)
	if dirty, err := jjWorkspaceDirty(workspace); err != nil {
		return fmt.Errorf("checking review workspace: %w", err)
	} else if dirty {
		return fmt.Errorf("review workspace has local changes; commit or discard them before refreshing")
	}
	source, err := jjSourceRepo(workspace)
	if err != nil {
		return fmt.Errorf("resolving source repo: %w", err)
	}
	bookmark := reviewBookmarkName(m.ID, repoName)
	oldHead, err := jjCommitID(workspace, "@-")
	if err != nil {
		return fmt.Errorf("resolving current review head: %w", err)
	}
	if err := fetchReviewHead(source, bookmark, pr.Number); err != nil {
		return err
	}
	newHead, err := jjCommitID(source, bookmark)
	if err != nil {
		return fmt.Errorf("resolving refreshed review head: %w", err)
	}
	if oldHead != newHead {
		if err := jjRebaseWorkspace(workspace, bookmark); err != nil {
			return fmt.Errorf("moving review workspace to refreshed head: %w", err)
		}
	}

	// Recto owns the attached PR snapshot. Ask through its public command so a
	// live viewer updates in place; a parked or older session will restore it on
	// launch, so an unavailable companion is only a warning.
	cmd := exec.Command("recto", "pr", pr.URL())
	cmd.Dir = workspace
	if output, err := cmd.CombinedOutput(); err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		fmt.Fprintf(os.Stderr, "rig: warning: refreshed workspace but could not update Recto: %s\n", detail)
	}
	if oldHead == newHead {
		fmt.Fprintf(os.Stderr, "rig: review %s is already at %s\n", pr.URL(), shortCommitID(newHead))
	} else {
		fmt.Fprintf(os.Stderr, "rig: refreshed %s from %s to %s\n", pr.URL(), shortCommitID(oldHead), shortCommitID(newHead))
	}
	return activateRig(*found)
}

func reviewRepoForPR(m manifest, pr *prRef) string {
	for subdir, repository := range m.Repos {
		if strings.EqualFold(repository, pr.Owner+"/"+pr.Repo) &&
			(len(m.Branches[subdir]) > 0 || len(m.Repos) == 1) {
			return subdir
		}
	}
	return ""
}

func shortCommitID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// pickupPR materializes a rig around an existing PR, choosing authoring vs
// review by who owns it. verb is the command the user reached for; when
// authorship disagrees with it we say so and do the right thing anyway, because
// "my work" has exactly one home (up) and "other work" exactly one (review).
func pickupPR(pr *prRef, verb string, pick *agentPick) error {
	// A review rig is reconstructible from the URL alone: pr-<n> plus its
	// owner/repo in the manifest distinguishes it from same-numbered PRs in
	// other repos. Resolve that locally before touching GitHub so repeating
	// `rig review <url>` is the fast resume operation users expect. This also
	// catches `rig up <someone-else's-pr>` after it was routed to review once.
	if done, err := attachExistingReviewRig(pr); err != nil {
		return err
	} else if done {
		return nil
	}

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
		return authorPickupPR(pr, meta, pick)
	}
	if verb == "up" {
		fmt.Fprintf(os.Stderr, "rig: PR #%d isn't yours — setting up a review, not authoring\n", pr.Number)
	}
	return reviewPickupPR(pr, meta, pick)
}

// existingReviewRig finds the local review rig for a PR without relying on the
// globally-ambiguous pr-<n> id. The manifest's repo and branch mappings supply
// the other half of the identity: an untracked repo merely added for research
// must not make same-numbered PRs look like this rig's review. kind=review also
// avoids grabbing an unrelated authoring rig with the same id and repo.
func existingReviewRig(rigs []rigInfo, pr *prRef) (*rigInfo, error) {
	wantID := fmt.Sprintf("pr-%d", pr.Number)
	wantRepo := pr.Owner + "/" + pr.Repo
	var found *rigInfo
	for i := range rigs {
		if rigs[i].ID != wantID {
			continue
		}
		m, err := readManifest(rigs[i].Path)
		if err != nil {
			return nil, fmt.Errorf("reading manifest: %w", err)
		}
		if !m.isReview() {
			continue
		}
		matchesRepo := false
		for subdir, repo := range m.Repos {
			if strings.EqualFold(repo, wantRepo) && (len(m.Branches[subdir]) > 0 || len(m.Repos) == 1) {
				matchesRepo = true
				break
			}
		}
		if !matchesRepo {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple review rigs match %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
		}
		found = &rigs[i]
	}
	return found, nil
}

func attachExistingReviewRig(pr *prRef) (bool, error) {
	rigs, err := listRigs()
	if err != nil {
		return false, err
	}
	found, err := existingReviewRig(rigs, pr)
	if err != nil {
		return false, err
	}
	if found == nil {
		return false, nil
	}
	// Older review rigs predate the explicit locator. Record it on the first
	// URL-based resume so Recto can restore the attached review on its own from
	// then on, without relying on the globally ambiguous pr-<n> id.
	m, err := readManifest(found.Path)
	if err != nil {
		return false, fmt.Errorf("reading manifest: %w", err)
	}
	reviewRepo := reviewRepoForPR(m, pr)
	if reviewRepo == "" {
		return false, fmt.Errorf("review rig has no repository for %s", pr.URL())
	}
	if m.ReviewPRs == nil {
		m.ReviewPRs = map[string]string{}
	}
	if m.ReviewPRs[reviewRepo] == "" {
		m.ReviewPRs[reviewRepo] = pr.URL()
		if err := writeManifest(found.Path, m); err != nil {
			return false, fmt.Errorf("recording review PR: %w", err)
		}
	}
	return true, activateRig(*found)
}

// reviewPickupPR builds a read-only review rig: the PR head fetched fork-safe
// via pull/N/head, kind=review (done when you've posted a review, never gated
// on merge), and the agent dropped straight into /review-pr.
func reviewPickupPR(pr *prRef, meta prMeta, pick *agentPick) error {
	// Past every resume check, so this is a rig we're about to build: `rig review
	// <url>` prompts for nothing else, and this is where its agent bar gets a turn.
	if ok, err := pick.ensurePicked(); err != nil || !ok {
		return err
	}
	repoPath, err := ensureGhqClone(pr.Owner, pr.Repo)
	if err != nil {
		return err
	}
	if err := ensureJJColocated(repoPath); err != nil {
		return fmt.Errorf("colocating jj on %s: %w", repoPath, err)
	}

	// Task id is just pr-<n>: jj workspace names get the repo appended and are
	// registered per source repo, and the basedir gets the title slug, so the
	// repo name isn't needed for uniqueness anywhere the id travels. The slug
	// derives from the PR title (Linear-style id-plus-title shape) rather than
	// the branch, which often embeds a whole ticket slug of its own.
	rigID := fmt.Sprintf("pr-%d", pr.Number)
	reviewBookmark := reviewBookmarkName(rigID, pr.Repo)
	if err := fetchReviewHead(repoPath, reviewBookmark, pr.Number); err != nil {
		return err
	}
	basedir, err := basedirPath(taskSlug(rigID, meta.Title))
	if err != nil {
		return err
	}

	m := manifest{
		ID: rigID, Title: meta.Title, Kind: "review",
		ReviewPRs: map[string]string{pr.Repo: pr.URL()},
		Agent:     string(pick.kind), MainRepo: pr.Repo,
	}
	if err := createBasedir(basedir, m); err != nil {
		return err
	}

	repo := repoRef{Owner: pr.Owner, Name: pr.Repo, Path: repoPath}
	repoDest, err := addRepoWorkspace(basedir, rigID, repo, reviewBookmark, meta.Branch)
	if err != nil {
		return err
	}

	sess := sessionSpec{
		rectoCmd: rectoCommand(),
		repo:     repo.Name,
		agent:    pick.kind,
		prompt: fmt.Sprintf(
			"/review-pr %d (%s). You are already on the PR branch in a dedicated jj workspace; skip branch verification.",
			pr.Number, meta.Title,
		),
	}
	session, err := spawnSession(basedir, repoDest, sess)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "rig: review %s/%s#%d — %s\n", pr.Owner, pr.Repo, pr.Number, basedir)
	return attachOrReport(session)
}

// prRigIdentity derives an authoring pickup's rig id and basedir slug. Linear's
// canonical PR-attachment lookup is authoritative when available. The literal
// and reversible issue tokens used by old and new Rig branches remain offline
// fallbacks. A PR with none of these signals falls back to pr-<n>.
func prRigIdentity(pr *prRef, meta prMeta, linked []linkedLinearTask) (rigID, basedirName string) {
	if tk, ok := primaryLinkedLinearTask(linked); ok {
		basedirName := tk.basedirName()
		if basedirName == "" {
			basedirName = taskSlug(tk.rigID(), tk.Title)
		}
		return tk.rigID(), basedirName
	}
	slug := stripBranchUserPrefix(meta.Branch)
	if id := leadingIssueID(slug); id != "" {
		return id, slug
	}
	if id := leadingEscapedIssueID(slug); id != "" {
		return id, restoreIssueSlug(slug)
	}
	rigID = fmt.Sprintf("pr-%d", pr.Number)
	return rigID, taskSlug(rigID, meta.Title)
}

// authorPickupPR builds an authoring rig around your own PR: you're coming back
// to keep working, not to review. It starts the workspace at branch@origin (via
// resolveStartRev) so your pushed commits come back and you stay on a pushable
// branch — the crucial difference from review's read-only pull/N/head — and
// leaves kind unset (authoring: done when the work merges). Identity comes from
// Linear's PR link with branch fallbacks (see prRigIdentity), so it's idempotent
// against the rig its originating issue-up created: a live one is switched to,
// a down'd one rebuilds at the same path for agent resume.
func authorPickupPR(pr *prRef, meta prMeta, pick *agentPick) error {
	linked, err := linkedLinearTasksForPR(pr.URL())
	if err != nil {
		fmt.Fprintf(os.Stderr, "rig: warning: couldn't ask Linear about PR links: %v\n", err)
	}
	rigID, basedirName := prRigIdentity(pr, meta, linked)
	if done, err := attachExistingRig(rigID); err != nil {
		return err
	} else if done {
		return nil
	}
	if ok, err := pick.ensurePicked(); err != nil || !ok {
		return err
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

	m := manifest{ID: rigID, Title: meta.Title, Agent: string(pick.kind), MainRepo: pr.Repo} // kind "" = authoring
	if err := createBasedir(basedir, m); err != nil {
		return err
	}

	repo := repoRef{Owner: pr.Owner, Name: pr.Repo, Path: repoPath}
	repoDest, err := addRepoWorkspace(basedir, rigID, repo, startRev, meta.Branch)
	if err != nil {
		return err
	}

	// This pickup is the sharpest case for rectoCommand's `--pr`: the workspace
	// lands on branch@origin with a fresh empty change on top, so `@-` is the PR
	// head and a diff against it is empty. You'd come back to your own PR and be
	// shown nothing. The merge-base shows the PR.
	sess := sessionSpec{
		rectoCmd: rectoCommand(),
		repo:     repo.Name,
		agent:    pick.kind,
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

func (pr *prRef) URL() string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", pr.Owner, pr.Repo, pr.Number)
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
func pickReviewPR(pick *agentPick) (*prRef, error) {
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
	rigs, err := listRigs()
	if err != nil {
		return nil, err
	}
	rows, err = withoutInFlightReviewRigs(rows, rigs)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("all open PRs awaiting your review already have rigs in flight")
	}

	sel, err := fzfSelect(rows, "Review PR: ", pick)
	if err != nil {
		return nil, err
	}
	if sel == "" {
		return nil, nil
	}

	return reviewPRFromPickerRow(sel)
}

func reviewPRFromPickerRow(row string) (*prRef, error) {
	cols := strings.SplitN(row, "\t", 4)
	if len(cols) < 2 {
		return nil, fmt.Errorf("unexpected picker selection: %q", row)
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

// withoutInFlightReviewRigs removes work already represented by an active
// review rig. Parked reviews stay pickable: selecting one is an intentional
// request to wake its existing workspace and conversation.
func withoutInFlightReviewRigs(rows []string, rigs []rigInfo) ([]string, error) {
	kept := make([]string, 0, len(rows))
	for _, row := range rows {
		pr, err := reviewPRFromPickerRow(row)
		if err != nil {
			return nil, err
		}
		found, err := existingReviewRig(rigs, pr)
		if err != nil {
			return nil, err
		}
		if found != nil && found.Parked.IsZero() {
			continue
		}
		kept = append(kept, row)
	}
	return kept, nil
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

// reviewBookmarkName is a repo-local reachability root for the exact PR head a
// review workspace started from. The workspace itself is not enough: when a
// tracked remote branch is force-pushed, jj can abandon the old head and rebase
// the empty working-copy commit even though nobody touched that workspace.
func reviewBookmarkName(rigID, repoName string) string {
	return "rig-review/" + jjWorkspaceName(rigID, repoName)
}

// fetchReviewHead fetches the fork-safe pull ref directly into Rig's local
// bookmark. The force is deliberate: this function is also the primitive an
// explicit refresh uses to move an existing snapshot. Untracking after jj
// imports the Git ref matters on installations that auto-track every new
// bookmark; a review pin must not join the ordinary push set.
func fetchReviewHead(repoPath, bookmark string, number int) error {
	spec := fmt.Sprintf("+pull/%d/head:refs/heads/%s", number, bookmark)
	cmd := exec.Command("git", "-C", repoPath, "fetch", "origin", spec, "--quiet")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fetching pull/%d/head: %w", number, err)
	}
	// The command also imports the colocated Git ref before changing tracking.
	// No matching remote is already the desired state, so verify the outcome
	// below instead of making that benign case an error.
	_ = exec.Command("jj", "-R", repoPath, "bookmark", "untrack", bookmark, "--remote", "origin").Run()
	tracked, err := exec.Command("jj", "-R", repoPath, "bookmark", "list",
		"--tracked", "--remote", "origin", "-T", `name ++ "\n"`, bookmark).Output()
	if err != nil {
		return fmt.Errorf("checking review bookmark %s: %w", bookmark, err)
	}
	if strings.TrimSpace(string(tracked)) != "" {
		return fmt.Errorf("review bookmark %s is still tracking origin", bookmark)
	}
	return nil
}
