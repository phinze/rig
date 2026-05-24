package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type primaryRepo struct {
	Name string // short repo name, used as subdir under basedir
	Path string // absolute path to source repo
}

// detectPrimaryRepo derives the primary repo from cwd. It expects to be run
// from inside (or under) a checkout that lives at ~/src/<host>/<owner>/<repo>.
func detectPrimaryRepo() (primaryRepo, error) {
	out, err := exec.Command("git", "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return primaryRepo{}, fmt.Errorf("not in a git repo — cd into a checkout first")
	}
	gitCommon := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitCommon) {
		cwd, _ := os.Getwd()
		gitCommon = filepath.Join(cwd, gitCommon)
	}
	repoPath, err := filepath.EvalSymlinks(filepath.Dir(gitCommon))
	if err != nil {
		return primaryRepo{}, fmt.Errorf("resolving repo path: %w", err)
	}
	return primaryRepo{Name: filepath.Base(repoPath), Path: repoPath}, nil
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

func revExists(repoPath, rev string) bool {
	cmd := exec.Command("jj", "-R", repoPath, "log", "-r", rev, "--no-graph", "-T", `"x"`)
	out, err := cmd.Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
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
