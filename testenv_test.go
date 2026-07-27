package main

import (
	"strings"
	"testing"
)

// hermeticGitVars is the environment that makes a test's git and jj invocations
// independent of whoever happens to be running them: a fixed identity, and the
// host's global and system git config switched off outright.
//
// The identity half was always here. The config half was the gap: with the
// host's gitconfig still in play, a developer who signs their commits has every
// test that builds a fixture repo depending on a reachable signing agent. Close
// a laptop lid, or let an agent time out, and five unrelated tests fail with
// "communication with agent failed" — a signature nothing in rig ever looks at.
// /dev/null reads as a valid, empty config file, and identity already comes from
// the environment, so nothing here needs the host's config for any other reason.
func hermeticGitVars() []string {
	return []string{
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"JJ_USER=Test", "JJ_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	}
}

// setHermeticGit applies the same settings to the test process, for tests that
// shell out with the ambient environment rather than building their own.
func setHermeticGit(t *testing.T) {
	t.Helper()
	for _, kv := range hermeticGitVars() {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
}
