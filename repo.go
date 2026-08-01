package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type repoRef struct {
	Owner string // e.g. "phinze" — may be "" when not derivable from path
	Name  string // short repo name, used as the subdir under the basedir
	Path  string // absolute path to the source repo
}

// nameWithOwner returns "owner/repo", or just "repo" when the owner is unknown.
func (r repoRef) nameWithOwner() string {
	if r.Owner == "" {
		return r.Name
	}
	return r.Owner + "/" + r.Name
}

// detectPrimaryRepo derives the primary repo from cwd. It expects to be run
// from inside (or under) a checkout that lives at ~/src/<host>/<owner>/<repo>.
func detectPrimaryRepo() (repoRef, error) {
	out, err := exec.Command("git", "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return repoRef{}, fmt.Errorf("not in a git repo — cd into a checkout first")
	}
	gitCommon := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitCommon) {
		cwd, _ := os.Getwd()
		gitCommon = filepath.Join(cwd, gitCommon)
	}
	repoPath, err := filepath.EvalSymlinks(filepath.Dir(gitCommon))
	if err != nil {
		return repoRef{}, fmt.Errorf("resolving repo path: %w", err)
	}
	return repoRef{
		Owner: ownerFromPath(repoPath),
		Name:  filepath.Base(repoPath),
		Path:  repoPath,
	}, nil
}

// resolveRepo picks the primary repo for a new rig. An explicit --repo
// owner/repo (override) wins outright and is cloned on demand. Otherwise it
// fzf-picks over the repos ghq manages, ranked by zoxide frecency — with the
// cwd repo, if you're standing in one, pinned to the top as the default row.
// Being in a checkout no longer *assumes* that repo (you'd silently rig the
// wrong one when the task is for elsewhere); it just pre-selects it, one Enter
// away. The one exception is a non-interactive caller (no tty to draw a picker):
// there cwd is the only answer we can give without a flag. A zero repoRef (empty
// Path) with a nil error means the picker was cancelled; callers abort quietly,
// mirroring resolveIssueID's "" convention.
func resolveRepo(override string, pick *agentPick) (repoRef, error) {
	if override != "" {
		owner, name, ok := strings.Cut(override, "/")
		if !ok || owner == "" || name == "" {
			return repoRef{}, fmt.Errorf("--repo wants owner/repo, got %q", override)
		}
		path, err := ensureGhqClone(owner, name)
		if err != nil {
			return repoRef{}, err
		}
		return repoRef{Owner: owner, Name: name, Path: path}, nil
	}

	cwd, cwdErr := detectPrimaryRepo()
	var cwdRepo *repoRef
	if cwdErr == nil {
		cwdRepo = &cwd
	}

	// No tty means no picker to choose from, so cwd is the only thing we can
	// resolve without a flag; absent even that, point at the escape hatches.
	if !stdinIsTTY() {
		if cwdRepo != nil {
			return *cwdRepo, nil
		}
		return repoRef{}, noTTYError("Pick repo: ")
	}

	ghq, err := ghqRepos()
	if err != nil && cwdRepo == nil {
		return repoRef{}, err
	}
	repos := repoCandidates(cwdRepo, ghq)
	if len(repos) == 0 {
		return repoRef{}, fmt.Errorf("no repos found via ghq — cd into a checkout, pass --repo owner/repo, or `ghq get` one first")
	}

	rows := make([]string, len(repos))
	for i, r := range repos {
		// col1 owner/repo is shown; the absolute path rides in the hidden last
		// column as the lookup key (paths are unique; short names may not be).
		rows[i] = r.nameWithOwner() + "\t\t\t" + r.Path
	}
	sel, err := fzfSelect(rows, "Pick repo: ", pick)
	if err != nil {
		return repoRef{}, err
	}
	if sel == "" {
		return repoRef{}, nil // cancelled
	}
	cols := strings.Split(sel, "\t")
	path := cols[len(cols)-1]
	for _, r := range repos {
		if r.Path == path {
			return r, nil
		}
	}
	return repoRef{}, fmt.Errorf("unexpected repo selection: %q", sel)
}

// repoCandidates orders the repo picker: the cwd repo (when there is one) goes
// first so it's the default-highlighted row you confirm rather than silently
// accept, then the ghq repos with the cwd one de-duplicated out. Split out from
// resolveRepo so the ordering is testable without a tty or shell-outs.
func repoCandidates(cwd *repoRef, ghq []repoRef) []repoRef {
	if cwd == nil {
		return ghq
	}
	out := make([]repoRef, 0, len(ghq)+1)
	out = append(out, *cwd)
	for _, r := range ghq {
		if r.Path != cwd.Path {
			out = append(out, r)
		}
	}
	return out
}

// ghqRepos lists the repos ghq manages as repoRefs, ranked by zoxide frecency
// (most-recently-visited first), with repos zoxide hasn't seen keeping ghq's
// own order behind them. `ghq list -p` gives absolute paths, so we match
// zoxide's dirs and build the ref without re-deriving the path.
func ghqRepos() ([]repoRef, error) {
	out, err := exec.Command("ghq", "list", "-p").Output()
	if err != nil {
		return nil, fmt.Errorf("ghq list: %w", err)
	}
	var repos []repoRef
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		repos = append(repos, repoRef{
			Owner: ownerFromPath(p),
			Name:  filepath.Base(p),
			Path:  p,
		})
	}
	rankReposByFrecency(repos, zoxideDirs())
	return repos, nil
}

// rankReposByFrecency stable-sorts repos so those appearing earlier in dirs
// (zoxide's most-frecent-first list) come first; repos absent from dirs keep
// their relative order at the tail. Split out from ghqRepos so it's testable
// without shelling out. dirs are symlink-resolved to match ghqRepos' paths.
func rankReposByFrecency(repos []repoRef, dirs []string) {
	rank := make(map[string]int, len(dirs))
	for i, d := range dirs {
		if resolved, err := filepath.EvalSymlinks(d); err == nil {
			d = resolved
		}
		if _, seen := rank[d]; !seen {
			rank[d] = i
		}
	}
	const unranked = 1 << 30
	rankOf := func(r repoRef) int {
		if i, ok := rank[r.Path]; ok {
			return i
		}
		return unranked
	}
	sort.SliceStable(repos, func(i, j int) bool {
		return rankOf(repos[i]) < rankOf(repos[j])
	})
}

// ownerFromPath pulls the owner segment out of a ghq-style checkout path
// (~/src/<host>/<owner>/<repo>). Returns "" if the path isn't under ~/src or
// doesn't have the expected depth, so GH_REPO derivation degrades gracefully.
func ownerFromPath(repoPath string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	srcRoot := filepath.Join(home, "src")
	rel, err := filepath.Rel(srcRoot, repoPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 3 {
		return ""
	}
	return parts[len(parts)-2]
}

// ensureJJColocated initializes jj alongside the existing git repo if needed.
func ensureJJColocated(repoPath string) error {
	if _, err := os.Stat(filepath.Join(repoPath, ".jj")); err == nil {
		return nil
	}
	cmd := exec.Command("jj", "git", "init", "--colocate", repoPath)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// resolveStartRev mirrors jpickup: prefer remote bookmark, then local, then trunk().
func resolveStartRev(repoPath, branchName string) string {
	// Best-effort fetch; harmless if the branch isn't on origin yet.
	_ = exec.Command("jj", "-R", repoPath, "git", "fetch", "--branch", branchName).Run()

	if revExists(repoPath, branchName+"@origin") {
		return branchName + "@origin"
	}
	if revExists(repoPath, branchName) {
		return branchName
	}
	return "trunk()"
}

// jjGitFetch refreshes a repo's remote-tracking state so trunk() reflects
// what has actually merged. Callers treat failure (offline, no remote) as
// "check against a stale trunk", which still fails closed.
func jjGitFetch(repoPath string) error {
	return exec.Command("jj", "-R", repoPath, "git", "fetch").Run()
}

// jjRevsetEmpty reports whether revset matches no commits in repoArg. jj
// errors are returned as errors so call sites can fail closed — never
// conflated with "no matches" (contrast revExists, which fails open in the
// direction its callers want).
func jjRevsetEmpty(repoArg, revset string) (bool, error) {
	cmd := exec.Command("jj", "-R", repoArg, "log", "-r", revset, "--no-graph", "-T", `"x"`)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return false, fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return false, err
	}
	return len(strings.TrimSpace(string(out))) == 0, nil
}

func revExists(repoPath, rev string) bool {
	cmd := exec.Command("jj", "-R", repoPath, "log", "-r", rev, "--no-graph", "-T", `"x"`)
	out, err := cmd.Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

// repoBranches answers "which branches back this repo's work in the rig?" It
// prefers the manifest's recorded branches (authoritative, captured at creation
// and extended by `rig track`), and only falls back to the jj-bookmark
// heuristic for repos we didn't record: added repos, or rigs whose manifests
// predate branch recording. The primary is first. An empty result means no
// branch could be determined.
func repoBranches(m manifest, subdir, workspacePath string) ([]string, error) {
	if bs := m.Branches[subdir]; len(bs) > 0 {
		return bs, nil
	}
	b, err := jjPRBranch(workspacePath)
	if err != nil {
		return nil, err
	}
	if b == "" {
		return nil, nil
	}
	return []string{b}, nil
}

// jjWorkspaceName is the workspace identity registered with the source repo.
// Scoping it by rig keeps multi-rig listings legible in `jj workspace list`.
func jjWorkspaceName(rigID, repoName string) string {
	return fmt.Sprintf("%s-%s", rigID, repoName)
}

func jjWorkspaceAdd(repoPath, wsName, startRev, dest string) error {
	cmd := exec.Command("jj", "-R", repoPath,
		"workspace", "add",
		"--revision", startRev,
		"--name", wsName,
		dest,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// jjWorkspaceForget removes workspace registrations. The repoArg should be
// the source repo (default workspace), not the workspace being forgotten —
// otherwise jj warns that the current workspace is being destroyed. Workspace
// dirs on disk can be deleted before or after this call.
func jjWorkspaceForget(repoArg string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	args := append([]string{"-R", repoArg, "workspace", "forget"}, names...)
	cmd := exec.Command("jj", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// workspaceRegistered reports whether a workspace name is registered with the
// source repo (regardless of whether its directory still exists on disk).
func workspaceRegistered(repoPath, wsName string) bool {
	out, err := exec.Command("jj", "-R", repoPath, "workspace", "list").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), wsName+":") {
			return true
		}
	}
	return false
}

// jjSourceRepo returns the source (default-workspace) repo path that backs
// the given workspace. In a non-default jj workspace, .jj/repo is a text file
// holding the relative path to the source repo's .jj/repo directory.
func jjSourceRepo(workspacePath string) (string, error) {
	repoFile := filepath.Join(workspacePath, ".jj", "repo")
	info, err := os.Stat(repoFile)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		// The default workspace IS the source repo.
		return workspacePath, nil
	}
	raw, err := os.ReadFile(repoFile)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(string(raw))
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(repoFile), target)
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	// abs points at the source repo's .jj/repo; strip those segments.
	return filepath.Dir(filepath.Dir(abs)), nil
}
