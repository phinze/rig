package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// seedTrustFor is the call site's form: resolve home, seed every agent's
// directory trust, and warn rather than fail. Used by the paths that create rig
// directories. See codextrust.go and claudetrust.go for why each agent needs it.
//
// It seeds only directories under the rigs root, which is the whole of what rig
// creates. Marking a directory trusted is granting authority, so it's bounded to
// the tree rig owns rather than to whatever path a caller happens to pass —
// otherwise a stray call (or a test that builds its fixture in /tmp) teaches
// an agent to trust somewhere nobody decided to trust.
func seedTrustFor(dirs ...string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	root := filepath.Join(home, "workspaces")
	var scoped []string
	for _, dir := range dirs {
		if isUnder(dir, root) {
			scoped = append(scoped, dir)
		}
	}
	if len(scoped) == 0 {
		return
	}
	if err := seedCodexTrust(home, scoped...); err != nil {
		fmt.Fprintf(os.Stderr, "rig: warning: could not mark %s trusted for codex: %v\n",
			strings.Join(scoped, ", "), err)
	}
	if err := seedClaudeTrust(home, scoped...); err != nil {
		fmt.Fprintf(os.Stderr, "rig: warning: could not mark %s trusted for claude: %v\n",
			strings.Join(scoped, ", "), err)
	}
}

// dropTrustFor hands back the directory-trust entries rig seeded on the way up.
// Warn rather than fail: a leftover entry names a directory that no longer
// exists, which is untidy but harmless, and teardown must not stall on it.
func dropTrustFor(basedir string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	if err := dropCodexTrust(home, basedir); err != nil {
		fmt.Fprintf(os.Stderr, "rig: warning: could not drop codex trust for %s: %v\n", basedir, err)
	}
	if err := dropClaudeTrust(home, basedir); err != nil {
		fmt.Fprintf(os.Stderr, "rig: warning: could not drop claude trust for %s: %v\n", basedir, err)
	}
}
