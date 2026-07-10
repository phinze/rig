package main

import (
	"bufio"
	"encoding/json"
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

// codexSessionActivity returns the newest mtime of a Codex rollout whose cwd
// is the rig root or one of its repo workspaces. Each rollout identifies its
// cwd in an early session_meta record and appends on every turn.
func codexSessionActivity(home, basedir string) int64 {
	out := map[string]int64{basedir: 0}
	updateCodexActivity(home, []string{basedir}, out)
	return out[basedir]
}

func updateCodexActivity(home string, basedirs []string, out map[string]int64) {
	root := filepath.Join(home, ".codex", "sessions")
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for i := 0; i < 20 && sc.Scan(); i++ {
			var row struct {
				Type    string `json:"type"`
				Payload struct {
					Cwd string `json:"cwd"`
				} `json:"payload"`
			}
			if json.Unmarshal(sc.Bytes(), &row) != nil || row.Type != "session_meta" {
				continue
			}
			if info, err := d.Info(); err == nil {
				for _, basedir := range basedirs {
					if pathInside(basedir, row.Payload.Cwd) && info.ModTime().Unix() > out[basedir] {
						out[basedir] = info.ModTime().Unix()
					}
				}
			}
			break
		}
		return nil
	})
}

// antigravitySessionActivity reads Antigravity CLI's prompt history. Unlike
// its opaque conversation store, history.jsonl records both workspace and the
// turn timestamp, which gives us the same persistent attention signal.
func antigravitySessionActivity(home, basedir string) int64 {
	out := map[string]int64{basedir: 0}
	updateAntigravityActivity(home, []string{basedir}, out)
	return out[basedir]
}

func updateAntigravityActivity(home string, basedirs []string, out map[string]int64) {
	f, err := os.Open(filepath.Join(home, ".gemini", "antigravity-cli", "history.jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	conversationActivity := map[string]int64{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var row struct {
			Timestamp      int64  `json:"timestamp"`
			Workspace      string `json:"workspace"`
			ConversationID string `json:"conversationId"`
		}
		if json.Unmarshal(sc.Bytes(), &row) != nil {
			continue
		}
		activity := row.Timestamp / 1000
		if row.ConversationID != "" {
			conversationTime, ok := conversationActivity[row.ConversationID]
			if !ok {
				for _, ext := range []string{".pb", ".db"} {
					path := filepath.Join(home, ".gemini", "antigravity-cli", "conversations", row.ConversationID+ext)
					if info, err := os.Stat(path); err == nil && info.ModTime().Unix() > conversationTime {
						conversationTime = info.ModTime().Unix()
					}
				}
				conversationActivity[row.ConversationID] = conversationTime
			}
			if conversationTime > activity {
				activity = conversationTime
			}
		}
		for _, basedir := range basedirs {
			if pathInside(basedir, row.Workspace) && activity > out[basedir] {
				out[basedir] = activity
			}
		}
	}
}

func agentSessionActivity(home, basedir string) int64 {
	return agentSessionActivities(home, []string{basedir})[basedir]
}

func agentSessionActivities(home string, basedirs []string) map[string]int64 {
	out := make(map[string]int64, len(basedirs))
	for _, basedir := range basedirs {
		out[basedir] = claudeSessionActivity(home, basedir)
	}
	updateCodexActivity(home, basedirs, out)
	updateAntigravityActivity(home, basedirs, out)
	return out
}

func pathInside(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
