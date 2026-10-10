package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// The lifecycle verbs reach other hosts two ways, matching how each already
// finds its rig. The pickers (switch, wake, resume) fold remote rigs into the
// rows they filter, so a fuzzy query or the fzf list covers every host and a
// remote pick forwards. The exact-name verbs (park, down, resurrect) take a
// remote rig only when nothing here answers to the name, or when the name says
// where with @place, because those are the verbs where guessing wrong costs a
// rig.

// withRemoteStatuses appends every rig the served surfaces describe to a
// picker's rows. They carry remote, which is how the picker draws them as
// id@place and how the verb knows to forward.
func withRemoteStatuses(statuses []rigStatus) []rigStatus {
	rows, _ := remoteRigs(nil)
	for _, r := range rows {
		statuses = append(statuses, r.row)
	}
	return statuses
}

// remoteHandle is how a picker row names itself: id for a rig here, id@place
// for one elsewhere, the same spelling every verb accepts.
func remoteHandle(s rigStatus) string {
	if s.remote != nil {
		return s.ID + "@" + s.Location
	}
	return s.ID
}

// remoteOf turns a remote picker row back into the target to forward to.
func remoteOf(s rigStatus) (remoteTarget, error) {
	place := surfacePlace(s.remote.surface)
	host, ok := servedAt(place)
	if !ok {
		return remoteTarget{}, fmt.Errorf("%s is no longer a configured surface", place)
	}
	return remoteTarget{host: host, place: place, row: s}, nil
}

// forwardPicked forwards a verb for a remote picker row.
func forwardPicked(s rigStatus, verb string, rest ...string) error {
	t, err := remoteOf(s)
	if err != nil {
		return err
	}
	return forward(t, append([]string{verb, t.row.ID}, rest...)...)
}

// exactRemote finds the remote rig an exact-name verb means: name@place
// outright, or a bare name when no rig here has it, matched on id or slug
// only. ok is false when the name should be left to the local path, which
// owns the error for a name that matches nothing anywhere.
func exactRemote(name string, localHit bool) (remoteTarget, bool, error) {
	if localOnly {
		return remoteTarget{}, false, nil
	}
	id, place := splitPlace(name)
	if place == "" && localHit {
		return remoteTarget{}, false, nil
	}
	if place == thisPlace() {
		return remoteTarget{}, false, nil
	}
	var hosts []servedBackend
	if place != "" {
		h, ok := servedAt(place)
		if !ok {
			return remoteTarget{}, false, fmt.Errorf("no rig surface for %s", place)
		}
		hosts = []servedBackend{h}
	}
	rows, down := remoteRigs(hosts)
	var hits []remoteTarget
	for _, r := range rows {
		if r.row.ID == id || r.row.Slug == id {
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], true, nil
	case 0:
		if place != "" {
			if len(down) > 0 {
				return remoteTarget{}, false, fmt.Errorf("can't reach %s", place)
			}
			return remoteTarget{}, false, fmt.Errorf("no rig named %q on %s", id, place)
		}
		return remoteTarget{}, false, nil
	default:
		var names []string
		for _, h := range hits {
			names = append(names, h.addr())
		}
		return remoteTarget{}, false, fmt.Errorf("%q names a rig on more than one host: %s", name, strings.Join(names, ", "))
	}
}

// execQuiet runs a verb for a remote row without writing to the terminal,
// for the radar, whose board owns the screen while it runs. A refusal over
// there comes back as its last line of stderr, which is where every rig
// command says why.
func execQuiet(s rigStatus, verb string) (*attachFrame, error) {
	t, err := remoteOf(s)
	if err != nil {
		return nil, err
	}
	sender := currentSender()
	var errOut bytes.Buffer
	attach, err := t.host.execAttach(execRequest{Argv: []string{verb, t.row.ID}, From: sender.addr, FromName: sender.name}, io.Discard, &errOut)
	if _, ok := errors.AsType[remoteExit](err); ok {
		lines := strings.Split(strings.TrimSpace(errOut.String()), "\n")
		return nil, fmt.Errorf("%s on %s: %s", verb, t.place, strings.TrimPrefix(lines[len(lines)-1], "rig: "))
	}
	return attach, err
}
