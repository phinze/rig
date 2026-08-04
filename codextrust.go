package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Codex gates every directory it has not seen before behind a "do you trust the
// contents of this directory?" prompt, and a rig is nothing but directories it
// has never seen. Verified against codex 0.146.0: the prompt is not covered by
// --dangerously-bypass-approvals-and-sandbox, trusting an ancestor directory
// does not cover its children, and a -c projects."…".trust_level override is
// ignored because the gate reads config.toml from disk rather than the merged
// config. Seeding the entry ourselves is the only lever there is, so rig writes
// it at the moment it creates the directory.
//
// We seed no matter which agent the rig runs. Trust is a property of the
// directory rather than of the process that opens it, and an ad hoc codex in a
// Claude rig is exactly the case where the prompt is most annoying and least
// informative — the human answering it already decided to trust the rig when
// they created it.
//
// Everything here is best-effort by design. A machine without codex has no
// ~/.codex to seed, and a rig must not fail to come up (or refuse to go away)
// over a config file belonging to a tool it isn't running.

// codexConfigPath returns codex's config file under home, and whether codex is
// set up here at all. A missing ~/.codex means codex was never run on this
// machine, and rig has no business manufacturing config for it.
func codexConfigPath(home string) (string, bool) {
	dir := filepath.Join(home, ".codex")
	if !dirExists(dir) {
		return "", false
	}
	return filepath.Join(dir, "config.toml"), true
}

// seedCodexTrust appends a trusted [projects."…"] entry for each of dirs that
// doesn't already have one. Appending a table header at EOF is always valid
// TOML, which is what lets this stay a plain append rather than a parse and
// rewrite of a file codex owns. The existence check is not politeness: TOML
// rejects a duplicate table outright, so a second copy of an entry would break
// codex's config load entirely.
//
// Codex rewrites the whole file when it records a trust decision of its own, so
// a write landing in that window can be lost. The cost is one prompt, which is
// the thing we were already living with.
func seedCodexTrust(home string, dirs ...string) error {
	path, ok := codexConfigPath(home)
	if !ok {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	existing := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		if dir, ok := codexProjectHeader(line); ok {
			existing[dir] = true
		}
	}

	var add []string
	for _, dir := range dirs {
		// A rig reached through a symlinked ~/workspaces is a different string
		// than the one codex canonicalizes cwd to, and the gate matches on the
		// string. Seeding both spellings costs one stanza and removes the whole
		// class of "we seeded it and it still asked".
		for _, spelling := range []string{dir, resolvePath(dir)} {
			if spelling == "" || existing[spelling] {
				continue
			}
			existing[spelling] = true
			add = append(add, spelling)
		}
	}
	if len(add) == 0 {
		return nil
	}

	var b strings.Builder
	if len(body) > 0 && !strings.HasSuffix(string(body), "\n") {
		b.WriteString("\n")
	}
	for _, dir := range add {
		fmt.Fprintf(&b, "\n[projects.%s]\ntrust_level = \"trusted\"\n", strconv.Quote(dir))
	}

	// 0o600 matches what codex creates the file with; it holds no secrets today
	// but it sits next to auth.json and inherits the neighborhood's posture.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// dropCodexTrust removes the trust entries for basedir and everything under it,
// so a torn-down rig doesn't leave codex accumulating stanzas for directories
// that no longer exist. Resurrect re-seeds on the way back up, since it rebuilds
// the basedir and its workspaces through the same calls a fresh rig does.
func dropCodexTrust(home, basedir string) error {
	path, ok := codexConfigPath(home)
	if !ok {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	lines := strings.Split(string(body), "\n")
	kept := make([]string, 0, len(lines))
	dropping := false
	changed := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			// Any table header ends the previous table, so the decision to keep
			// or drop is remade here and nowhere else.
			dir, isProject := codexProjectHeader(line)
			dropping = isProject && isUnder(dir, basedir)
			if dropping {
				changed = true
				// Take the blank line that separated this stanza from the one
				// above with it, or removals leave a widening gap behind.
				for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
					kept = kept[:len(kept)-1]
				}
			}
		}
		if !dropping {
			kept = append(kept, line)
		}
	}
	if !changed {
		return nil
	}

	return writeFileAtomic(path, []byte(strings.Join(kept, "\n")), 0o600)
}

// codexProjectHeader parses a [projects."…"] table header, returning the
// directory it names. Codex serializes the path as a TOML basic string, which
// strconv.Unquote reads directly.
func codexProjectHeader(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	inner, ok := strings.CutPrefix(trimmed, "[projects.")
	if !ok {
		return "", false
	}
	inner, ok = strings.CutSuffix(inner, "]")
	if !ok {
		return "", false
	}
	dir, err := strconv.Unquote(inner)
	if err != nil {
		return "", false
	}
	return dir, true
}

// writeFileAtomic replaces path via a temp file in the same directory, so a
// crash mid-write can't leave codex with a truncated config.
func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".rig-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// seedCodexTrustFor is the call site's form: resolve home, seed, and warn rather
// than fail. Used by the paths that create rig directories.
//
// It seeds only directories under the rigs root, which is the whole of what rig
// creates. Marking a directory trusted is granting authority, so it's bounded to
// the tree rig owns rather than to whatever path a caller happens to pass —
// otherwise a stray call (or a test that builds its fixture in /tmp) teaches
// codex to trust somewhere nobody decided to trust.
func seedCodexTrustFor(dirs ...string) {
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
}
