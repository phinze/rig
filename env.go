package main

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
)

// runEnv implements `rig env`: print shell setup lines describing the rig
// identity of the current directory, for the direnv stdlib to eval. All the
// layout and manifest knowledge lives here in rig rather than in shell glue,
// so the host's direnvrc reduces to `eval "$(rig env)"`. Prints nothing (and
// exits 0) outside any rig, so the eval is a no-op there.
func runEnv(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: rig env")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	for _, line := range envExports(cwd) {
		fmt.Println(line)
	}
	return nil
}

// envExports computes the shell setup lines for cwd. Kept pure (no Getwd/Getenv)
// for testability.
func envExports(cwd string) []string {
	if basedir, err := findBasedir(cwd); err == nil {
		return rigExports(basedir, cwd)
	}
	return nil
}

// rigExports emits the rig's identity: basedir and id everywhere under the
// rig, plus the working-tree id (same shape as the jj workspace name) when
// cwd is inside one of the rig's repo workspaces.
//
// GH_REPO is unset rather than exported. Workspaces are colocated, so gh reads
// the repo from cwd's git remote like it would in any checkout. An exported
// GH_REPO was worse than none: an agent keeps the value from the directory it
// started in, so one that later ran gh from a second repo in the same rig
// aimed at the first. The PATH_rm clears the gh shim that used to patch over
// that, from shells that still carry it.
//
// Deliberately absent: the rig's agent. Every other key here exists because
// something downstream reads it — a dev server wants RIG_PORT, iso wants
// ISO_SESSION — and nothing ever read RIG_AGENT. What it did instead was collide with the input side: parseAgent reads that same name
// to seed the picker, so a rig quietly made its own agent the starting position
// for the next rig you created from inside it. RIG_AGENT is now yours alone,
// a standing preference set in your shell, and rig only reads it.
func rigExports(basedir, cwd string) []string {
	m, err := readManifest(basedir)
	if err != nil {
		return nil
	}
	out := []string{
		"export RIG_BASEDIR=" + shellQuote(basedir),
		"PATH_rm " + shellQuote(filepath.Join(basedir, ".rig", "bin")),
		"unset GH_REPO",
	}
	if m.ID != "" {
		out = append(out, "export RIG_ID="+shellQuote(m.ID))
	}

	rel, err := filepath.Rel(basedir, cwd)
	if err != nil || rel == "." {
		return out
	}
	sub, _, _ := strings.Cut(rel, string(filepath.Separator))
	if m.Repos[sub] == "" {
		return out // not inside a known repo workspace
	}
	if m.ID != "" {
		workspace := m.ID + "-" + sub
		out = append(out, "export RIG_WORKSPACE="+shellQuote(workspace))
		// A stable dev-server port per workspace, so several live rigs (the
		// parallelism the sandbox boundary is built for) don't collide on the
		// default port. Hashing the workspace identity makes it deterministic
		// across machines and reboots; being keyed off cwd means each repo
		// workspace in a multi-repo rig gets its own port for free.
		out = append(out, fmt.Sprintf("export RIG_PORT=%d", hashPort(workspace)))
		// Tool knobs for tools rig composes with, emitted only where the
		// tool is actually in play. iso keys dev containers off
		// ISO_SESSION; without this, same-named checkouts cross-wire
		// (the override carries the whole name, dev- purpose prefix
		// included — see mirendev/runtime#849).
		if dirExists(filepath.Join(basedir, sub, ".iso")) {
			out = append(out, "export ISO_SESSION="+shellQuote(isoSessionName(m.ID, sub)))
		}
	}
	return out
}

// hashPort maps an arbitrary key to a stable port in 10000-19999, matching
// worktrunk's hash_port range. FNV keeps it dependency-free and reproducible:
// the same workspace name always lands on the same port, on any machine.
func hashPort(key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return 10000 + int(h.Sum32()%10000)
}

// isoSessionName is the iso session identity rig exports as ISO_SESSION for
// a repo workspace (dev- purpose prefix included; see the comment in
// rigExports). Teardown stops sessions by this exact name, so the two sides
// share one definition.
func isoSessionName(rigID, sub string) string {
	return "dev-" + rigID + "-" + sub
}
