package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// rig's caches are accelerators, never sources of errors: a missing, torn, or
// unwritable file reads as empty and writes as nothing, and every caller
// already knows how to get the answer the slow way. What each file trusts and
// for how long belongs to its caller; this is only where they live and how
// they reach disk.

func cacheFilePath(name string) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "rig", name), nil
}

// readCacheFile decodes the named cache into v, reporting whether it could.
func readCacheFile(name string, v any) bool {
	path, err := cacheFilePath(name)
	if err != nil {
		return false
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(blob, v) == nil
}

// writeCacheFile replaces the named cache with v, atomically (write + rename)
// so a process killed mid-write can't leave a torn file for the next reader.
// The temp file is unique per write because serve and a radar can both be
// writing the same cache at once.
func writeCacheFile(name string, v any) {
	path, err := cacheFilePath(name)
	if err != nil {
		return
	}
	blob, err := json.Marshal(v)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return
	}
	_, werr := tmp.Write(blob)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Chmod(tmp.Name(), 0o644) != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}
