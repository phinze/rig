package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// iso peers are the extra containers a repo's .iso/peers.yml defines, started
// with `iso peers up` (runtime's hack/dev-distributed is the one that matters).
// rig never starts them, so everything here reads iso's own docker labels
// rather than iso's CLI: `iso peers status` answers for one workspace from its
// cwd, and a board wants every workspace in one call.
//
// The labels are the contract. iso stamps every resource it makes with
// iso.managed, iso.session, and iso.project.dir (the workspace it ran from);
// peer containers add iso.peer and iso.peer.name. Builds of iso from before
// peers were session-scoped labelled every peer iso.session=peers, so a
// session filter simply doesn't see those; that's the right failure for
// teardown, which must never reach past its own session.

// isoPeer is one peer container as the board reports it. State is docker's
// (running, exited, created, ...), not a rig vocabulary.
type isoPeer struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// isoDockerTimeout bounds every docker call here. The board and radar read
// peers on every pass, and a wedged daemon must cost a missing column, not a
// hung `rig ls`.
const isoDockerTimeout = 3 * time.Second

func isoDocker(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), isoDockerTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "docker", args...).Output()
}

// isoPeersByWorkspace lists every iso peer container on the machine, keyed by
// the resolved workspace it was started from. A missing docker is no peers,
// not an error: most machines that run rig never run iso.
func isoPeersByWorkspace() (map[string][]isoPeer, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, nil
	}
	out, err := isoDocker("ps", "-a",
		"--filter", "label=iso.managed=true",
		"--filter", "label=iso.peer=true",
		"--format", `{{.Label "iso.project.dir"}}`+"\t"+`{{.Label "iso.peer.name"}}`+"\t"+`{{.State}}`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing iso peers: %w", err)
	}
	return parseISOPeers(string(out)), nil
}

func parseISOPeers(out string) map[string][]isoPeer {
	found := map[string][]isoPeer{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
			continue
		}
		dir := resolvePath(parts[0])
		found[dir] = append(found[dir], isoPeer{Name: parts[1], State: parts[2]})
	}
	for dir := range found {
		sort.Slice(found[dir], func(i, j int) bool { return found[dir][i].Name < found[dir][j].Name })
	}
	return found
}

// peersUnder gathers the peers of every workspace directly inside basedir,
// which is a rig's view of them: radar draws one tail per rig, not per repo.
func peersUnder(all map[string][]isoPeer, basedir string) []isoPeer {
	if len(all) == 0 || basedir == "" {
		return nil
	}
	base := resolvePath(basedir)
	var peers []isoPeer
	for dir, ps := range all {
		if filepath.Dir(dir) == base {
			peers = append(peers, ps...)
		}
	}
	return peers
}

// runningPeers counts the peers docker reports as running.
func runningPeers(peers []isoPeer) int {
	n := 0
	for _, p := range peers {
		if p.State == "running" {
			n++
		}
	}
	return n
}

// removeISOLeftovers finishes what `iso stop` was supposed to: every container
// and network iso labelled with this session and workspace. It runs after iso
// stop on purpose, so the normal path finds nothing; it exists because iso stop
// warns and moves on when a network still has endpoints, and `iso peers up`
// attaches the session's services to a peers network that stop tries to remove
// before those services are gone. That left runtime-dev-mir-1979-runtime-iso-peers
// behind on 2026-10-01, and -mir-1827- before it without anyone noticing.
//
// Volumes are never touched. iso stop already removes the session's own, and
// the cache volumes are shared across every session of the repo.
//
// It fails closed, like compose cleanup: a resource that survives is an error,
// so the teardown job stays on disk and reap retries it.
func removeISOLeftovers(item isoCleanup) error {
	workspace := resolveExistingPath(item.Workspace)
	mine := func(dir string) bool { return dir != "" && resolveExistingPath(dir) == workspace }
	sessionFilter := "label=iso.session=" + item.Session

	list := func(kind string) ([]string, error) {
		args := []string{kind, "ls", "-a"}
		if kind == "network" {
			args = []string{kind, "ls"}
		}
		args = append(args, "--filter", "label=iso.managed=true", "--filter", sessionFilter,
			"--format", `{{.ID}}`+"\t"+`{{.Label "iso.project.dir"}}`)
		out, err := isoDocker(args...)
		if err != nil {
			return nil, fmt.Errorf("listing iso %ss for %s: %w", kind, item.Session, err)
		}
		var ids []string
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			id, dir, ok := strings.Cut(line, "\t")
			if ok && id != "" && mine(dir) {
				ids = append(ids, id)
			}
		}
		return ids, nil
	}

	for _, kind := range []string{"container", "network"} {
		ids, err := list(kind)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			continue
		}
		args := append([]string{kind, "rm"}, ids...)
		if kind == "container" {
			args = append([]string{"rm", "-f"}, ids...)
		}
		fmt.Fprintf(os.Stderr, "rig: removing %d iso %s(s) left by session %s\n", len(ids), kind, item.Session)
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("removing iso %ss for %s: %w: %s", kind, item.Session, err, strings.TrimSpace(string(out)))
		}
	}
	for _, kind := range []string{"container", "network"} {
		ids, err := list(kind)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			return fmt.Errorf("iso session %s still has %s resources: %v", item.Session, kind, ids)
		}
	}
	return nil
}
