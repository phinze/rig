package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const manifestName = ".rig.toml"

type manifest struct {
	ID    string
	Title string
	// Repos maps a repo's subdir name under the basedir to its
	// "owner/repo" slug. The global direnvrc reads this to set GH_REPO,
	// since the flat basedir path no longer encodes owner/repo the way
	// the old ~/workspaces/<host>/<owner>/<repo> shape did.
	Repos map[string]string
}

func writeManifest(basedir string, m manifest) error {
	var b strings.Builder
	fmt.Fprintf(&b, "id    = %q\n", m.ID)
	fmt.Fprintf(&b, "title = %q\n", m.Title)
	if len(m.Repos) > 0 {
		b.WriteString("\n[repos]\n")
		keys := make([]string, 0, len(m.Repos))
		for k := range m.Repos {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s = %q\n", k, m.Repos[k])
		}
	}
	return os.WriteFile(filepath.Join(basedir, manifestName), []byte(b.String()), 0o644)
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

	m := manifest{Repos: map[string]string{}}
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
			}
		case "repos":
			m.Repos[key] = val
		}
	}
	if err := sc.Err(); err != nil {
		return manifest{}, err
	}
	return m, nil
}

// addRepoToManifest records a repo's subdir → owner/repo mapping, read-modify-
// writing the manifest so `rig add` and the initial `up` share one code path.
func addRepoToManifest(basedir, subdir, nameWithOwner string) error {
	m, err := readManifest(basedir)
	if err != nil {
		return err
	}
	if m.Repos == nil {
		m.Repos = map[string]string{}
	}
	m.Repos[subdir] = nameWithOwner
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
