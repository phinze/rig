package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// A PR often sits in a stack: its base branch is another open PR's head, and
// another PR may be based on its head in turn. A review rig for one PR of a
// stack holds the whole stack, so the reviewer reads each PR as its own layer
// and the agent knows which commits belong to which. The rig is named for the
// top PR and its workspace sits at the top head; each lower PR's head is
// pinned at a bookmark of its own.

// stackPR is what walking a stack needs to know about one PR.
type stackPR struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	// A PR from a fork names a branch that lives in the fork, so a branch
	// name alone can't link it into this repository's stack.
	IsCrossRepository bool `json:"isCrossRepository"`
}

// stackLister lists a repo's open PRs whose head (by "head") or base (by
// "base") is the given branch. Tests substitute their own.
type stackLister func(owner, repo, by, branch string) ([]stackPR, error)

// ghStackLister asks GitHub.
func ghStackLister(owner, repo, by, branch string) ([]stackPR, error) {
	out, err := exec.Command("gh", "pr", "list", "-R", owner+"/"+repo,
		"--"+by, branch, "--state", "open",
		"--json", "number,url,headRefName,baseRefName,isCrossRepository").Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list --%s %s: %w", by, branch, err)
	}
	var prs []stackPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parsing gh pr list output: %w", err)
	}
	return prs, nil
}

// ghStackPR reads one PR's branches.
func ghStackPR(pr *prRef) (stackPR, error) {
	out, err := exec.Command("gh", "pr", "view", strconv.Itoa(pr.Number),
		"-R", pr.Owner+"/"+pr.Repo, "--json", "number,url,headRefName,baseRefName,isCrossRepository").Output()
	if err != nil {
		return stackPR{}, fmt.Errorf("gh pr view %s: %w", pr.URL(), err)
	}
	var s stackPR
	if err := json.Unmarshal(out, &s); err != nil {
		return stackPR{}, fmt.Errorf("parsing gh pr view output: %w", err)
	}
	return s, nil
}

// maxStack bounds a walk, so a cycle in GitHub's data can't hang a pickup.
const maxStack = 12

// reviewChain finds the stack a PR sits in, bottom first. Down, it follows
// the open PR whose head branch is this one's base; up, the open PR based on
// this one's head. Either direction stops where there isn't exactly one: at
// trunk below, at the top or a fork above. A PR in no stack is a chain of one.
// Only PRs whose head lives in this repository link: a fork's branch of the
// same name is somebody else's work.
func reviewChain(pr *prRef, start stackPR, list stackLister) ([]*prRef, error) {
	seen := map[int]bool{start.Number: true}
	chain := []*prRef{pr}
	sameRepo := func(prs []stackPR) []stackPR {
		var out []stackPR
		for _, p := range prs {
			if !p.IsCrossRepository {
				out = append(out, p)
			}
		}
		return out
	}
	// Down to the bottom of the stack.
	for cur := start; len(chain) < maxStack; {
		below, err := list(pr.Owner, pr.Repo, "head", cur.BaseRefName)
		if err != nil {
			return nil, err
		}
		below = sameRepo(below)
		if len(below) != 1 || seen[below[0].Number] {
			break
		}
		cur = below[0]
		seen[cur.Number] = true
		chain = append([]*prRef{{Owner: pr.Owner, Repo: pr.Repo, Number: cur.Number}}, chain...)
	}
	// Up to the top. A fork PR's head branch isn't one this repository's
	// PRs can be based on, so nothing stacks above it.
	for cur := start; len(chain) < maxStack && !start.IsCrossRepository; {
		above, err := list(pr.Owner, pr.Repo, "base", cur.HeadRefName)
		if err != nil {
			return nil, err
		}
		above = sameRepo(above)
		if len(above) != 1 || seen[above[0].Number] {
			break
		}
		cur = above[0]
		seen[cur.Number] = true
		chain = append(chain, &prRef{Owner: pr.Owner, Repo: pr.Repo, Number: cur.Number})
	}
	return chain, nil
}

// discoverReviewChain is reviewChain against GitHub. Discovery is a
// convenience: when it can't run, the review proceeds on the one PR.
func discoverReviewChain(pr *prRef) []*prRef {
	start, err := ghStackPR(pr)
	if err != nil {
		return []*prRef{pr}
	}
	chain, err := reviewChain(pr, start, ghStackLister)
	if err != nil {
		return []*prRef{pr}
	}
	return chain
}

// stackBookmarkName pins a lower PR's head in a stack review. A sibling of
// the workspace's own bookmark rather than a child of it, since a Git ref
// can't be both a branch and a directory of branches.
func stackBookmarkName(rigID, repoName string, number int) string {
	return fmt.Sprintf("%s-pr%d", reviewBookmarkName(rigID, repoName), number)
}

// stackURLs renders a chain as the manifest keeps it.
func stackURLs(chain []*prRef) []string {
	urls := make([]string, 0, len(chain))
	for _, pr := range chain {
		urls = append(urls, pr.URL())
	}
	return urls
}

// stackSummary names a chain for a person or an agent: #41 → #42 → #43.
func stackSummary(chain []*prRef) string {
	parts := make([]string, 0, len(chain))
	for _, pr := range chain {
		parts = append(parts, "#"+strconv.Itoa(pr.Number))
	}
	return strings.Join(parts, " → ")
}

// parseStackArgs reads `rig review URL URL...` as an explicit stack, bottom
// first, all in one repository.
func parseStackArgs(args []string) ([]*prRef, error) {
	var chain []*prRef
	for _, arg := range args {
		pr := parsePRURL(arg)
		if pr == nil {
			return nil, fmt.Errorf("not a PR URL: %s", arg)
		}
		if len(chain) > 0 && !strings.EqualFold(chain[0].Owner+"/"+chain[0].Repo, pr.Owner+"/"+pr.Repo) {
			return nil, fmt.Errorf("a review stack lives in one repository; %s is in %s/%s", arg, pr.Owner, pr.Repo)
		}
		chain = append(chain, pr)
	}
	return chain, nil
}
