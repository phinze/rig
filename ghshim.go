package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runGHShim invokes the real gh with repository context derived from cwd.
// Agents inherit their environment when the rig starts, then commonly execute
// commands with a different cwd without passing through a shell hook. Resolving
// here keeps a runtime-started agent from opening cloud's PR against runtime.
func runGHShim(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	env := os.Environ()
	path := withoutRigShim(os.Getenv("PATH"), os.Getenv("RIG_BASEDIR"))
	if basedir, err := findBasedir(cwd); err == nil {
		path = withoutRigShim(path, basedir)
		m, readErr := readManifest(basedir)
		if readErr != nil {
			return fmt.Errorf("reading manifest: %w", readErr)
		}
		if subdir, repoErr := repoSubdirForCwd(basedir, cwd, m); repoErr == nil {
			env = setEnv(env, "GH_REPO", m.Repos[subdir])
		} else {
			env = unsetEnv(env, "GH_REPO")
		}
	} else if os.Getenv("RIG_BASEDIR") != "" {
		// The agent left its rig. Don't let the repo it started in override gh's
		// normal discovery in the new cwd.
		env = unsetEnv(env, "GH_REPO")
	}
	realGH, err := findExecutable("gh", path)
	if err != nil {
		return err
	}
	env = setEnv(env, "PATH", path)

	cmd := exec.Command(realGH, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func withoutRigShim(path, basedir string) string {
	if basedir == "" {
		return path
	}
	shimDir := filepath.Clean(filepath.Join(basedir, ".rig", "bin"))
	parts := filepath.SplitList(path)
	out := parts[:0]
	for _, part := range parts {
		if filepath.Clean(part) != shimDir {
			out = append(out, part)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
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
