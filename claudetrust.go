package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// Claude asks "do you trust the files in this folder?" the first time it opens a
// directory, and --dangerously-skip-permissions doesn't cover it. Unlike codex,
// trust on an ancestor does cover the children: a ~/workspaces entry trusted by
// hand once is why every rig on foxtrotbase started clean (642 rig workspaces
// there have no trust flag of their own), and its absence is why a laptop asked
// on every single rig. Leaning on that ancestor would mean rig depends on a
// click nobody remembers making, so rig seeds each directory it creates, the
// same as it does for codex.
//
// Like codex trust this is best-effort. A machine where claude never ran has no
// ~/.claude.json, and rig doesn't manufacture one.

// claudeConfigPath returns claude's global config file, and whether claude is
// set up here at all. CLAUDE_CONFIG_DIR relocates it, the same as it relocates
// everything else claude keeps.
func claudeConfigPath(home string) (string, bool) {
	path := filepath.Join(home, ".claude.json")
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		path = filepath.Join(dir, ".claude.json")
	}
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}

// claudeConfig is ~/.claude.json held loosely: rig touches one flag on a few
// project entries and must hand every other byte of claude's state back as it
// found it, so values stay raw and only the path down to the flag is decoded.
type claudeConfig struct {
	top      map[string]json.RawMessage
	projects map[string]map[string]json.RawMessage
}

func readClaudeConfig(path string) (*claudeConfig, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &claudeConfig{top: map[string]json.RawMessage{}, projects: map[string]map[string]json.RawMessage{}}
	if err := json.Unmarshal(body, &c.top); err != nil {
		return nil, err
	}
	if raw, ok := c.top["projects"]; ok {
		if err := json.Unmarshal(raw, &c.projects); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// write replaces the file atomically. Claude rewrites the whole file too, so a
// write of its landing between our read and our rename is lost, and one of ours
// landing inside its window can be. The window is milliseconds; the cost of
// losing ours is one prompt, which is what we were living with anyway.
func (c *claudeConfig) write(path string) error {
	projects, err := json.Marshal(c.projects)
	if err != nil {
		return err
	}
	c.top["projects"] = projects
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	// Claude writes JSON.stringify output, which leaves <, >, and & alone.
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c.top); err != nil {
		return err
	}
	return writeFileAtomic(path, b.Bytes(), 0o600)
}

// seedClaudeTrust marks each of dirs trusted, creating the project entry when
// claude hasn't met the directory yet. An entry claude already trusts is left
// alone, and nothing is written when every dir is already trusted.
func seedClaudeTrust(home string, dirs ...string) error {
	path, ok := claudeConfigPath(home)
	if !ok {
		return nil
	}
	c, err := readClaudeConfig(path)
	if err != nil {
		return err
	}
	trusted := json.RawMessage("true")
	changed := false
	for _, dir := range dirs {
		// Claude keys projects by its realpath'd cwd, so a symlinked spelling
		// alone would be seeded and still asked about. Same reasoning as codex.
		for _, spelling := range []string{dir, resolvePath(dir)} {
			if spelling == "" {
				continue
			}
			entry := c.projects[spelling]
			if entry == nil {
				entry = map[string]json.RawMessage{}
				c.projects[spelling] = entry
			}
			if bytes.Equal(entry["hasTrustDialogAccepted"], trusted) {
				continue
			}
			entry["hasTrustDialogAccepted"] = trusted
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return c.write(path)
}

// dropClaudeTrust removes the project entries for basedir and everything under
// it. The whole entry goes, not just the flag: the directory is gone, and
// claude otherwise keeps a record per rig forever (foxtrotbase had over a
// thousand). Transcripts live under ~/.claude/projects/, not here, so a
// resurrected rig can still resume its sessions.
func dropClaudeTrust(home, basedir string) error {
	path, ok := claudeConfigPath(home)
	if !ok {
		return nil
	}
	c, err := readClaudeConfig(path)
	if err != nil {
		return err
	}
	canonicalBase := resolveExistingPath(basedir)
	changed := false
	for dir := range c.projects {
		if isUnder(dir, basedir) || isUnder(resolveExistingPath(dir), canonicalBase) {
			delete(c.projects, dir)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return c.write(path)
}
