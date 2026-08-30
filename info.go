package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// rigContext is the small, stable machine-facing view of the rig containing
// cwd. Consumers should ask Rig for this instead of learning `.rig/`'s
// storage schema or walking Rig's directory layout themselves.
type rigContext struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	Root          string `json:"root"`
	Kind          string `json:"kind,omitempty"`
	Repo          string `json:"repo,omitempty"`
	Repository    string `json:"repository,omitempty"`
	ReviewPR      string `json:"review_pr,omitempty"`
}

func runInfo(args []string) error {
	if len(args) != 1 || args[0] != "--format=json" {
		return fmt.Errorf("usage: rig info --format=json")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	basedir, err := findBasedir(cwd)
	if err != nil {
		return err
	}
	m, err := readManifest(basedir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	context := rigContextFor(basedir, cwd, m)
	blob, err := json.MarshalIndent(context, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(blob))
	return nil
}

func rigContextFor(basedir, cwd string, m manifest) rigContext {
	root, err := filepath.Abs(basedir)
	if err != nil {
		root = basedir
	}
	context := rigContext{SchemaVersion: 1, ID: m.ID, Root: root, Kind: m.Kind}
	rel, err := filepath.Rel(basedir, cwd)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return context
	}
	repo, _, _ := strings.Cut(rel, string(filepath.Separator))
	repository := m.Repos[repo]
	if repository == "" {
		return context
	}
	context.Repo = repo
	context.Repository = repository
	context.ReviewPR = reviewPRForRepo(m, repo)
	return context
}

func reviewPRForRepo(m manifest, repo string) string {
	if !m.isReview() || m.Repos[repo] == "" {
		return ""
	}
	if locator := m.ReviewPRs[repo]; locator != "" {
		return locator
	}
	// Compatibility for review rigs created before review_prs became manifest
	// state. It is only safe when the rig has one repo; after `rig add`, nothing
	// local says which same-numbered repository the old id originally named.
	// Rig owns this historical naming rule; API consumers never need to.
	if len(m.Repos) != 1 {
		return ""
	}
	number, err := strconv.Atoi(strings.TrimPrefix(m.ID, "pr-"))
	if err != nil || number < 1 || m.ID != fmt.Sprintf("pr-%d", number) {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", m.Repos[repo], number)
}
