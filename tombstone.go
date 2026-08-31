package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const tombstoneVersion = 1

// tombstoneRetention is how long a torn-down rig stays resurrectable. It's a
// regret window, not a backup policy: long enough to cover a weekend and the
// Monday morning where you notice something you wanted is gone, short enough
// that the directory stays readable at a glance. Nothing here holds file
// content, so the whole store is kilobytes.
const tombstoneRetention = 7 * 24 * time.Hour

// tombstoneRetentionLabel is the window as a person would say it. Duration's
// own String() renders this as "168h0m0s", which is accurate and unreadable in
// the two places users actually meet it.
func tombstoneRetentionLabel() string {
	days := int(tombstoneRetention.Hours() / 24)
	if days == 1 {
		return "day"
	}
	return fmt.Sprintf("%d days", days)
}

// tombstone is what a rig leaves behind when it's torn down: enough to stand
// the same rig back up and, more importantly, to reopen the conversation that
// was running in it.
//
// It exists because rig's teardown gates reason entirely in commits, branches,
// and PR states, and those are the wrong units for the thing that actually
// hurts to lose. A rig can be perfectly clean by every VCS measure and still be
// carrying the only copy of a long exploration. The gates can't see that, so
// instead of trying to teach them, teardown records the facts that make the
// loss reversible and gets on with it.
//
// Everything here must be captured *before* deletion. The agent stores are
// keyed by cwd or workspace, not by rig, so once the basedir is gone there is
// no query that finds the session again. That asymmetry is the whole design:
// the tombstone is cheap to write and impossible to reconstruct.
type tombstone struct {
	Version    int    `json:"version"`
	ID         string `json:"id"`
	Title      string `json:"title"`
	Basedir    string `json:"basedir"`
	Kind       string `json:"kind,omitempty"`
	Agent      string `json:"agent,omitempty"`
	Tracker    string `json:"tracker,omitempty"`
	TrackerID  string `json:"tracker_id,omitempty"`
	TrackerURL string `json:"tracker_url,omitempty"`
	// Created is the rig's own birthday, carried over from the manifest; Died
	// is when teardown ran. Both are needed to render "lived 3 days, died 2h
	// ago", which is how you recognise a rig you'd forgotten the name of.
	Created time.Time `json:"created,omitzero"`
	Died    time.Time `json:"died"`
	Parked  bool      `json:"parked,omitempty"`
	// Repos maps subdir to "owner/repo" and Sources maps subdir to the local
	// source checkout the jj workspace was added from. Resurrection needs both:
	// the slug to rewrite the manifest, the path to re-add the workspace.
	Repos    map[string]string   `json:"repos,omitempty"`
	Sources  map[string]string   `json:"sources,omitempty"`
	Branches map[string][]string `json:"branches,omitempty"`
	PRs      map[string]int      `json:"prs,omitempty"`
	Session  *sessionRef         `json:"session,omitempty"`

	path string
}

// subject is what this rig was about, for a history row. Prefers the title the
// rig was created with and falls back to the id, which is always present.
func (t *tombstone) subject() string {
	if t.Title != "" {
		return t.Title
	}
	return t.ID
}

// resurrectable reports whether there's a conversation to reopen. A tombstone
// without one is still worth showing (it records that the rig existed and what
// it held) but Enter on it can only rebuild the workspaces.
func (t *tombstone) resurrectable() bool {
	return t.Session != nil && t.Session.ID != ""
}

func tombstoneDir() (string, error) {
	state, err := rigStateDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(state, "tombstones")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// tombstonePath keys a tombstone by id *and* basedir hash, matching how
// teardown jobs are named. Ids repeat: `rig up` on the same ticket twice is
// ordinary, and the second rig's tombstone must not overwrite the first's while
// the first is still inside the regret window.
func tombstonePath(id, basedir string) (string, error) {
	dir, err := tombstoneDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(resolvePath(basedir)))
	return filepath.Join(dir, fmt.Sprintf("%s-%x.json", id, sum[:8])), nil
}

// recordTombstone captures a rig on its way out. It is deliberately
// best-effort at the call site: failing to write a tombstone must never block
// or fail a teardown the user asked for, because the alternative is a rig that
// won't go away. Errors are returned so the caller can warn, not abort.
func recordTombstone(basedir string, m manifest, sources map[string]string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	agent, err := parseAgent(m.Agent)
	if err != nil {
		agent = agentClaude
	}
	t := &tombstone{
		Version:    tombstoneVersion,
		ID:         m.ID,
		Title:      m.Title,
		Basedir:    resolvePath(basedir),
		Kind:       m.Kind,
		Agent:      string(agent),
		Tracker:    m.Tracker,
		TrackerID:  m.TrackerID,
		TrackerURL: m.TrackerURL,
		Created:    m.Created,
		Died:       time.Now(),
		Parked:     !m.Parked.IsZero(),
		Repos:      m.Repos,
		Sources:    sources,
		Branches:   m.Branches,
		PRs:        m.PRs,
		Session:    agentSessionRef(home, basedir, agent),
	}
	return writeTombstone(t)
}

func writeTombstone(t *tombstone) error {
	path, err := tombstonePath(t.ID, t.Basedir)
	if err != nil {
		return err
	}
	blob, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename so a reader never sees a half-written tombstone, and so
	// a crash mid-write can't leave one that parses as an empty rig.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(blob, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	t.path = path
	return nil
}

func readTombstone(path string) (*tombstone, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t tombstone
	if err := json.Unmarshal(blob, &t); err != nil {
		return nil, err
	}
	if t.Version != tombstoneVersion {
		return nil, fmt.Errorf("unsupported tombstone version %d in %s", t.Version, path)
	}
	t.path = path
	return &t, nil
}

// listTombstones returns the rigs still inside the regret window, newest death
// first, pruning anything past it on the way through. Pruning here rather than
// on a timer means the store cleans itself whenever anyone looks at it, and a
// machine that never looks never grows one either, since nothing is written
// unless a teardown happens.
func listTombstones(now time.Time) ([]*tombstone, error) {
	dir, err := tombstoneDir()
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []*tombstone
	for _, path := range paths {
		t, err := readTombstone(path)
		if err != nil {
			// An unreadable tombstone is not worth failing a board over, but it
			// also must not be silently deleted: it may be the only record of
			// something. Leave it and skip it.
			continue
		}
		if now.Sub(t.Died) > tombstoneRetention {
			_ = os.Remove(path)
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Died.After(out[j].Died) })
	return out, nil
}

// findTombstone resolves a rig id to its tombstone, newest first when a id has
// been reused. Returns nil when nothing matches.
func findTombstone(id string, now time.Time) (*tombstone, error) {
	all, err := listTombstones(now)
	if err != nil {
		return nil, err
	}
	for _, t := range all {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, nil
}
