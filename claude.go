package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var nonAlnumRe = regexp.MustCompile(`[^a-zA-Z0-9]`)

// claudeProjectDirName mirrors Claude Code's project-dir mangling: every
// non-alphanumeric character in the absolute cwd becomes a dash, so
// /home/phinze/workspaces/foo lands at ~/.claude/projects/-home-phinze-workspaces-foo.
func claudeProjectDirName(path string) string {
	return nonAlnumRe.ReplaceAllString(path, "-")
}

// claudeSessionActivity returns the newest mtime (unix seconds) of any
// claude session file recorded for cwds inside the rig's basedir. Claude
// Code keeps one JSONL per session under ~/.claude/projects/<mangled cwd>/,
// appending on every real turn — human-driven or autonomous — and never on
// mere TUI repaint, which makes file mtime an honest agent-attention signal
// that persists across detach and reboot. Matches the basedir itself plus
// any cwd under it (claude is spawned in the primary repo workspace, not
// the basedir). Returns 0 when nothing is found.
func claudeSessionActivity(home, basedir string) int64 {
	mangled := claudeProjectDirName(basedir)
	root := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	var latest int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() != mangled && !strings.HasPrefix(e.Name(), mangled+"-") {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			if info, err := f.Info(); err == nil {
				if t := info.ModTime().Unix(); t > latest {
					latest = t
				}
			}
		}
	}
	return latest
}
