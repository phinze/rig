package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const manifestName = ".rig.toml"

type manifest struct {
	ID    string
	Title string
}

func writeManifest(basedir string, m manifest) error {
	body := fmt.Sprintf("id    = %q\ntitle = %q\n", m.ID, m.Title)
	return os.WriteFile(filepath.Join(basedir, manifestName), []byte(body), 0o644)
}

// readManifest is intentionally a minimal hand-rolled parser. We only emit
// `key = "value"` pairs, so we only need to read them back. Swap for a real
// TOML library when the schema grows.
func readManifest(basedir string) (manifest, error) {
	f, err := os.Open(filepath.Join(basedir, manifestName))
	if err != nil {
		return manifest{}, err
	}
	defer f.Close()

	var m manifest
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch key {
		case "id":
			m.ID = val
		case "title":
			m.Title = val
		}
	}
	if err := sc.Err(); err != nil {
		return manifest{}, err
	}
	return m, nil
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
