package main

import (
	"fmt"
	"os"
	"os/exec"
)

// direnvAllow runs `direnv allow <path>` so rig-written or jj-snapshotted
// .envrc files load on first cd instead of throwing the blocked-content
// error. Skips silently if direnv isn't installed or the dir has no .envrc.
func direnvAllow(dir string) error {
	if _, err := exec.LookPath("direnv"); err != nil {
		return nil // direnv not installed; leave .envrc unblessed
	}
	if _, err := os.Stat(dir + "/.envrc"); err != nil {
		return nil
	}
	cmd := exec.Command("direnv", "allow", dir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("direnv allow %s: %w", dir, err)
	}
	return nil
}
