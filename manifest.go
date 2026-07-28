package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const manifestName = ".rig.toml"

type manifest struct {
	ID      string
	Title   string
	Created time.Time
	// Agent is the terminal agent this rig was created for. Empty means Claude,
	// preserving the behavior of manifests written before agent selection.
	Agent string
	// Kind records how the rig came to be, which sets its terminal condition.
	// "" and "up" are authoring rigs — done when the work merges, so teardown
	// guards their local commits. "review" is a `rig review` pickup of someone
	// else's PR: the workspace holds the author's commits fetched read-only, so
	// there's nothing of yours to merge and the rig is done once you've posted a
	// review, never gated on the PR itself merging. Absent on rigs predating this
	// field, which read as authoring — the safe default.
	Kind string
	// Parked, when non-zero, marks a rig as dormant: its work is done and up
	// for human review, so it drops out of `rig switch` and its tmux session is
	// killed, but the basedir (and its agent session history) stay on disk.
	// `rig wake` clears it and stands the session back up at the same path. Zero
	// means an ordinary in-flight rig.
	Parked time.Time
	// Repos maps a repo's subdir name under the basedir to its
	// "owner/repo" slug. The global direnvrc reads this to set GH_REPO,
	// since the flat basedir path no longer encodes owner/repo the way
	// the old ~/workspaces/<host>/<owner>/<repo> shape did.
	Repos map[string]string
	// Branches maps a repo's subdir to the branches its work rides on. The
	// first is the primary, captured at workspace creation (up's Linear branch,
	// review's PR head) — the authoritative answer to "which PR is this repo's?",
	// recorded before the branch is even pushed so pr/ls/reap resolve the rig's
	// own PR rather than guessing from whatever bookmark the workspace sits on.
	// Any that follow are secondaries recorded by `rig track` — a bugfix PR you
	// spun off the same repo while in here — so down/reap gate on all of them,
	// not just the primary. Absent for added repos (no branch yet) and rigs
	// predating this field, which fall back to the jj-bookmark heuristic. The
	// TOML encodes each as a string array; a legacy scalar reads as a one-element
	// list.
	Branches map[string][]string
	// PRs maps a repo's subdir to a pull request number this rig was once
	// observed to have. It exists because a branch is a perishable record and a
	// PR number isn't: GitHub deletes the head branch on merge, after which
	// branch-keyed lookups return nothing and a rig that shipped becomes
	// indistinguishable from one that never produced anything. Both read as "no
	// PR, clean tree", which is exactly the pair sweep has to tell apart before
	// deciding whether a teardown is a formality or a discard.
	//
	// Recorded opportunistically the first time a PR is seen for a repo, so it's
	// absent on rigs that predate the field or never got swept. Absent means
	// "unknown", never "never had one" — callers must degrade, not conclude.
	PRs map[string]int
}

// isReview reports whether this rig is a `rig review` pickup, whose terminal
// condition is "review posted" rather than "work merged".
func (m manifest) isReview() bool { return m.Kind == "review" }

func writeManifest(basedir string, m manifest) error {
	var b strings.Builder
	fmt.Fprintf(&b, "id    = %q\n", m.ID)
	fmt.Fprintf(&b, "title = %q\n", m.Title)
	if m.Kind != "" {
		fmt.Fprintf(&b, "kind  = %q\n", m.Kind)
	}
	if m.Agent != "" && m.Agent != string(agentClaude) {
		fmt.Fprintf(&b, "agent = %q\n", m.Agent)
	}
	if !m.Created.IsZero() {
		fmt.Fprintf(&b, "created = %q\n", m.Created.Format(time.RFC3339))
	}
	if !m.Parked.IsZero() {
		fmt.Fprintf(&b, "parked = %q\n", m.Parked.Format(time.RFC3339))
	}
	writeTable(&b, "repos", m.Repos)
	writeBranchTable(&b, "branches", m.Branches)
	writeIntTable(&b, "prs", m.PRs)
	return os.WriteFile(filepath.Join(basedir, manifestName), []byte(b.String()), 0o644)
}

// writeIntTable emits a sorted [name] table of key = number pairs, unquoted so
// they read back as TOML integers. Same empty-map elision as writeTable.
func writeIntTable(b *strings.Builder, name string, table map[string]int) {
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
		fmt.Fprintf(b, "%s = %d\n", k, table[k])
	}
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

// writeBranchTable emits a sorted [name] table whose values are string arrays
// (subdir → ["primary", "secondary", ...]). Empty maps and empty slices leave
// no output, mirroring writeTable so a rig with no recorded branch grows no
// dangling header. Always array-valued, even for one branch, so the shape is
// uniform; readManifest still accepts a legacy scalar.
func writeBranchTable(b *strings.Builder, name string, table map[string][]string) {
	if len(table) == 0 {
		return
	}
	keys := make([]string, 0, len(table))
	for k, vals := range table {
		if len(vals) > 0 {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	sort.Strings(keys)
	fmt.Fprintf(b, "\n[%s]\n", name)
	for _, k := range keys {
		quoted := make([]string, len(table[k]))
		for i, v := range table[k] {
			quoted[i] = fmt.Sprintf("%q", v)
		}
		fmt.Fprintf(b, "%s = [%s]\n", k, strings.Join(quoted, ", "))
	}
}

// parseTOMLStringArray reads a `["a", "b"]` array literal into its elements.
// Deliberately minimal, matching the hand-rolled reader: it splits on commas
// and decodes each quoted string, which is enough for branch names (git refs
// carry no commas or quotes). Empty elements are dropped.
func parseTOMLStringArray(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = parseTOMLString(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseTOMLString reverses the %q encoding used by writeManifest. Merely
// trimming the surrounding quotes leaves escapes such as \" in the value;
// every later read-modify-write then escapes those backslashes again.
func parseTOMLString(s string) string {
	s = strings.TrimSpace(s)
	if value, err := strconv.Unquote(s); err == nil {
		return value
	}
	// Keep accepting the deliberately small legacy format if a hand-edited
	// manifest contains an unquoted value or an escape strconv rejects.
	return strings.Trim(s, `"`)
}

// readManifest is intentionally a minimal hand-rolled parser for the scalar
// fields and two simple tables rig itself emits. Swap for a real TOML library
// if the schema grows further.
func readManifest(basedir string) (manifest, error) {
	f, err := os.Open(filepath.Join(basedir, manifestName))
	if err != nil {
		return manifest{}, err
	}
	defer f.Close()

	m := manifest{Repos: map[string]string{}, Branches: map[string][]string{}, PRs: map[string]int{}}
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
		val = strings.TrimSpace(val)
		switch section {
		case "":
			val = parseTOMLString(val)
			switch key {
			case "id":
				m.ID = val
			case "title":
				m.Title = val
			case "kind":
				m.Kind = val
			case "agent":
				m.Agent = val
			case "created":
				if t, err := time.Parse(time.RFC3339, val); err == nil {
					m.Created = t
				}
			case "parked":
				if t, err := time.Parse(time.RFC3339, val); err == nil {
					m.Parked = t
				}
			}
		case "repos":
			m.Repos[key] = parseTOMLString(val)
		case "branches":
			// Array form is the current shape; a bare scalar is a legacy
			// single-branch manifest, read as a one-element list.
			if strings.HasPrefix(val, "[") {
				m.Branches[key] = parseTOMLStringArray(val)
			} else {
				m.Branches[key] = []string{parseTOMLString(val)}
			}
		case "prs":
			// A value we can't parse is dropped rather than recorded as zero: a
			// bogus PR number would be worse than the absent-means-unknown default.
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && n > 0 {
				m.PRs[key] = n
			}
		}
	}
	if err := sc.Err(); err != nil {
		return manifest{}, err
	}
	return m, nil
}

func (m manifest) agentKind() agentKind {
	if m.Agent == "" {
		return agentClaude
	}
	a, err := parseAgent(m.Agent)
	if err != nil {
		return agentClaude
	}
	return a
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
			m.Branches = map[string][]string{}
		}
		m.Branches[subdir] = []string{branch}
	}
	return writeManifest(basedir, m)
}

// addBranchToManifest records an additional branch for a repo already in the
// rig — the `rig track` path for a secondary PR spun off the same repo. It
// appends rather than replaces (the primary keeps its slot) and is idempotent:
// a branch already tracked, primary or not, is a no-op. Reports whether it
// actually added anything so callers can tailor their output.
func addBranchToManifest(basedir, subdir, branch string) (bool, error) {
	m, err := readManifest(basedir)
	if err != nil {
		return false, err
	}
	if slices.Contains(m.Branches[subdir], branch) {
		return false, nil
	}
	if m.Branches == nil {
		m.Branches = map[string][]string{}
	}
	m.Branches[subdir] = append(m.Branches[subdir], branch)
	return true, writeManifest(basedir, m)
}

// recordObservedPRs notes the PR numbers seen for a rig's repos, so the fact
// that this rig once shipped something survives GitHub deleting the branch.
// Best-effort by design: it takes the mutation lock without blocking and gives
// up on contention, because this rides along with a read-only scan and must
// never make one wait or fail. Reports whether it wrote anything.
//
// Existing entries are never overwritten. The first PR a repo had is the one
// that answers "did work happen here", and a later secondary shouldn't displace
// it.
func recordObservedPRs(basedir string, seen map[string]int) (bool, error) {
	if len(seen) == 0 {
		return false, nil
	}
	lock, err := acquireRigMutationLockMode(basedir, true)
	if err != nil {
		return false, nil // busy: try again on the next scan
	}
	defer func() { _ = lock.Close() }()

	m, err := readManifest(basedir)
	if err != nil {
		return false, err
	}
	if m.PRs == nil {
		m.PRs = map[string]int{}
	}
	changed := false
	for subdir, number := range seen {
		if number <= 0 || m.PRs[subdir] != 0 {
			continue
		}
		m.PRs[subdir] = number
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, writeManifest(basedir, m)
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
