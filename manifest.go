package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const manifestName = ".rig.toml"

type manifest struct {
	ID      string
	Title   string
	Created time.Time
	// Repos maps a repo's subdir name under the basedir to its
	// "owner/repo" slug. The global direnvrc reads this to set GH_REPO,
	// since the flat basedir path no longer encodes owner/repo the way
	// the old ~/workspaces/<host>/<owner>/<repo> shape did.
	Repos map[string]string
	// Branches maps a repo's subdir to the branch its work rides on, captured
	// at workspace creation (up's Linear branch, review's PR head). It's the
	// authoritative answer to "which PR is this repo's?" — recorded before the
	// branch is even pushed, so pr/ls/reap resolve the rig's own PR rather than
	// guessing from whatever bookmark the workspace happens to sit on. Absent
	// for added repos (no branch yet) and rigs predating this field, which fall
	// back to the jj-bookmark heuristic.
	Branches map[string]string
}

func writeManifest(basedir string, m manifest) error {
	var b strings.Builder
	fmt.Fprintf(&b, "id    = %q\n", m.ID)
	fmt.Fprintf(&b, "title = %q\n", m.Title)
	if !m.Created.IsZero() {
		fmt.Fprintf(&b, "created = %q\n", m.Created.Format(time.RFC3339))
	}
	writeTable(&b, "repos", m.Repos)
	writeTable(&b, "branches", m.Branches)
	return os.WriteFile(filepath.Join(basedir, manifestName), []byte(b.String()), 0o644)
}

// writeTable emits a sorted [name] table of key = "value" pairs, or nothing
// when the map is empty, so an unused table (e.g. branches on an older rig)
// leaves no dangling header.
func writeTable(b *strings.Builder, name string, table map[string]string) {
	if len(table) == 0 {
		return
	}
	fmt.Fprintf(b, "\n[%s]\n", name)
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s = %q\n", k, table[k])
	}
}

// readManifest is intentionally a minimal hand-rolled parser. We only emit
// `key = "value"` pairs and a single `[repos]` table, so we only need to read
// those back. Swap for a real TOML library if the schema grows further.
func readManifest(basedir string) (manifest, error) {
	f, err := os.Open(filepath.Join(basedir, manifestName))
	if err != nil {
		return manifest{}, err
	}
	defer f.Close()

	m := manifest{Repos: map[string]string{}, Branches: map[string]string{}}
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch section {
		case "":
			switch key {
			case "id":
				m.ID = val
			case "title":
				m.Title = val
			case "created":
				if t, err := time.Parse(time.RFC3339, val); err == nil {
					m.Created = t
				}
			}
		case "repos":
			m.Repos[key] = val
		case "branches":
			m.Branches[key] = val
		}
	}
	if err := sc.Err(); err != nil {
		return manifest{}, err
	}
	return m, nil
}

// addRepoToManifest records a repo's subdir → owner/repo mapping (and its
// branch, when known), read-modify-writing the manifest so `rig add` and the
// initial `up` share one code path. An empty branch records nothing, leaving
// the repo to the bookmark-heuristic fallback.
func addRepoToManifest(basedir, subdir, nameWithOwner, branch string) error {
	m, err := readManifest(basedir)
	if err != nil {
		return err
	}
	if m.Repos == nil {
		m.Repos = map[string]string{}
	}
	m.Repos[subdir] = nameWithOwner
	if branch != "" {
		if m.Branches == nil {
			m.Branches = map[string]string{}
		}
		m.Branches[subdir] = branch
	}
	return writeManifest(basedir, m)
}

func writeRootEnvrc(basedir string, m manifest) error {
	body := fmt.Sprintf("export RIG_BASEDIR=$PWD\nexport RIG_ID=%s\n", m.ID)
	return os.WriteFile(filepath.Join(basedir, ".envrc"), []byte(body), 0o644)
}

// findBasedir walks up from start looking for the rig manifest. Returns the
// directory containing it, or an error if not under any rig.
func findBasedir(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, manifestName)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not inside a rig (no %s found in any parent)", manifestName)
		}
		dir = parent
	}
}
