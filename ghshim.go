package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ghShim is the body rig used to write to <basedir>/.rig/bin/gh. It no longer
// writes one, but rigs made before jj's colocated workspaces still have it on
// disk, and agents started in them still have it at the front of PATH.
const ghShim = "#!/bin/sh\nexec rig __gh \"$@\"\n"

// runGHShim keeps those older shims working until their rigs are gone. The
// shim existed to set GH_REPO from the invocation cwd, because a workspace with
// no .git gave gh nothing to infer from and an agent's startup GH_REPO went
// stale as soon as it ran gh from another repo. Colocated workspaces have a
// git remote, so all that's left to do is drop the stale value and step out of
// the way. Once no rig predates colocation, this, ghShim, and the __gh command
// can go.
func runGHShim(args []string) error {
	path := withoutRigShims(os.Getenv("PATH"))
	realGH, err := findExecutable("gh", path)
	if err != nil {
		return err
	}
	env := unsetEnv(os.Environ(), "GH_REPO")
	env = setEnv(env, "PATH", path)

	cmd := exec.Command(realGH, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// withoutRigShims removes every generated Rig gh shim from path. A shell can
// carry an older rig's shim when tmux starts a new pane from that shell's
// environment. Leaving that stale shim behind would make `rig __gh` select it
// as the real gh, which recursively invokes `rig __gh` again.
func withoutRigShims(path string) string {
	parts := filepath.SplitList(path)
	out := parts[:0]
	for _, part := range parts {
		if !isRigShimDir(part) {
			out = append(out, part)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
}

func isRigShimDir(dir string) bool {
	dir = filepath.Clean(dir)
	if filepath.Base(dir) != "bin" || filepath.Base(filepath.Dir(dir)) != ".rig" {
		return false
	}
	body, err := os.ReadFile(filepath.Join(dir, "gh"))
	return err == nil && string(body) == ghShim
}

func findExecutable(name, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found beyond the rig shim", name)
}

func setEnv(env []string, key, value string) []string {
	env = unsetEnv(env, key)
	return append(env, key+"="+value)
}

func unsetEnv(env []string, key string) []string {
	prefix := key + "="
	out := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}
