package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runEnv implements `rig env`: print shell export lines describing the rig
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
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	for _, line := range envExports(cwd, home) {
		fmt.Println(line)
	}
	return nil
}

// envExports computes the export lines for cwd. Kept pure (no Getwd/Getenv)
// for testability.
func envExports(cwd, home string) []string {
	if basedir, err := findBasedir(cwd); err == nil {
		return rigExports(basedir, cwd)
	}
	return legacyExports(cwd, home)
}

// rigExports emits the rig's identity: basedir and id everywhere under the
// rig, plus the working-tree id (same shape as the jj workspace name) and
// GH_REPO when cwd is inside one of the rig's repo workspaces.
func rigExports(basedir, cwd string) []string {
	m, err := readManifest(basedir)
	if err != nil {
		return nil
	}
	out := []string{"export RIG_BASEDIR=" + shellQuote(basedir)}
	if m.ID != "" {
		out = append(out, "export RIG_ID="+shellQuote(m.ID))
	}

	rel, err := filepath.Rel(basedir, cwd)
	if err != nil || rel == "." {
		return out
	}
	sub, _, _ := strings.Cut(rel, string(filepath.Separator))
	nwo := m.Repos[sub]
	if nwo == "" {
		return out // not inside a known repo workspace
	}
	if m.ID != "" {
		out = append(out, "export RIG_WORKSPACE="+shellQuote(m.ID+"-"+sub))
		// Tool knobs for tools rig composes with, emitted only where the
		// tool is actually in play. iso keys dev containers off
		// ISO_SESSION; without this, same-named checkouts cross-wire
		// (the override carries the whole name, dev- purpose prefix
		// included — see mirendev/runtime#849).
		if dirExists(filepath.Join(basedir, sub, ".iso")) {
			out = append(out, "export ISO_SESSION="+shellQuote(isoSessionName(m.ID, sub)))
		}
	}
	return append(out, "export GH_REPO="+shellQuote(nwo))
}

// isoSessionName is the iso session identity rig exports as ISO_SESSION for
// a repo workspace (dev- purpose prefix included; see the comment in
// rigExports). Teardown stops sessions by this exact name, so the two sides
// share one definition.
func isoSessionName(rigID, sub string) string {
	return "dev-" + rigID + "-" + sub
}

// legacyExports handles the pre-rig workspace layout,
// ~/workspaces/<host>/<owner>/<repo>/..., where owner/repo is encoded in the
// path itself. Ages out as those sessions finish.
func legacyExports(cwd, home string) []string {
	prefix := filepath.Join(home, "workspaces") + string(filepath.Separator)
	rel, ok := strings.CutPrefix(cwd, prefix)
	if !ok {
		return nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 3 || parts[1] == "" || parts[2] == "" {
		return nil
	}
	return []string{"export GH_REPO=" + shellQuote(parts[1]+"/"+parts[2])}
}
