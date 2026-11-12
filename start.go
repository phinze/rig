package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// A repo's start is the commit its work began from: where the workspace
// forked off trunk when the rig made it. A branch point moves with every
// rebase and empties out once work lands on trunk, but the start doesn't, so
// a viewer can anchor there and keep showing what this rig did after it
// shipped.

// startRevset names that commit from inside a workspace. For a workspace made
// on trunk it is trunk's tip at the time; for one made on an existing branch
// it is where that branch left trunk.
const startRevset = "fork_point(trunk() | @)"

// workspaceStart reads the start of a workspace that was just created.
func workspaceStart(ws string) (string, error) {
	return jjCommitIDAt(ws, "", startRevset)
}

// recoverWorkspaceStart works out the start of a workspace made before rig
// recorded one, from jj's operation log: it asks the operation that created
// the workspace's first working copy where that commit forked off trunk.
// That operation sees trunk as it was then, so the answer matches what
// recording at creation would have said.
func recoverWorkspaceStart(ws string) (string, error) {
	name, err := jjWorkspaceNameAt(ws)
	if err != nil {
		return "", err
	}
	out, err := exec.Command("jj", "-R", ws, "--ignore-working-copy", "op", "log",
		"--no-graph", "-T", `id ++ "\t" ++ description ++ "\n"`).Output()
	if err != nil {
		return "", fmt.Errorf("jj op log: %w", jjErr(err))
	}
	want := "create initial working-copy commit in workspace " + name
	for line := range strings.SplitSeq(string(out), "\n") {
		id, desc, ok := strings.Cut(line, "\t")
		if ok && desc == want {
			// The op log is newest first, so a workspace recreated under the
			// same name reports its latest start.
			return jjCommitIDAt(ws, id, fmt.Sprintf("fork_point(trunk() | %s@)", name))
		}
	}
	return "", fmt.Errorf("jj's operation log no longer shows workspace %s being created", name)
}

// jjWorkspaceNameAt names the jj workspace rooted at ws.
func jjWorkspaceNameAt(ws string) (string, error) {
	out, err := exec.Command("jj", "-R", ws, "--ignore-working-copy", "workspace", "list",
		"-T", `name ++ "\t" ++ self.root() ++ "\n"`).Output()
	if err != nil {
		return "", fmt.Errorf("jj workspace list: %w", jjErr(err))
	}
	want := resolvePath(ws)
	for line := range strings.SplitSeq(string(out), "\n") {
		name, root, ok := strings.Cut(line, "\t")
		if ok && resolvePath(root) == want {
			return name, nil
		}
	}
	return "", fmt.Errorf("no jj workspace is rooted at %s", ws)
}

// jjCommitIDAt resolves revset to exactly one commit id in the repo at ws, as
// of operation op when op isn't empty.
func jjCommitIDAt(ws, op, revset string) (string, error) {
	args := []string{"-R", ws, "--ignore-working-copy"}
	if op != "" {
		args = append(args, "--at-op", op)
	}
	args = append(args, "log", "-r", revset, "--no-graph", "-T", `commit_id ++ "\n"`)
	out, err := exec.Command("jj", args...).Output()
	if err != nil {
		return "", fmt.Errorf("jj log -r %s: %w", revset, jjErr(err))
	}
	ids := strings.Fields(string(out))
	if len(ids) != 1 {
		return "", fmt.Errorf("%s names %d commits, not one", revset, len(ids))
	}
	return ids[0], nil
}

// jjErr folds a failed command's stderr into its error.
func jjErr(err error) error {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}

// recordRepoStart notes a repo's start in the manifest.
func recordRepoStart(basedir, subdir, commit string) error {
	m, err := readManifest(basedir)
	if err != nil {
		return err
	}
	if m.Starts == nil {
		m.Starts = map[string]string{}
	}
	m.Starts[subdir] = commit
	return writeManifest(basedir, m)
}

// repoStart is the start of a rig's repo: the recorded one, else recovered
// from jj when the rig predates recording it.
func repoStart(basedir string, m manifest, subdir string) (string, error) {
	if c := m.Starts[subdir]; c != "" {
		return c, nil
	}
	return recoverWorkspaceStart(filepath.Join(basedir, subdir))
}
