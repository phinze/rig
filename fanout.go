package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// The board verbs that aren't ls (waiting, history, notify list) answer for
// every host by running themselves on each one. A table gets each host's
// answer as its own section under the local one, verbatim, because those
// tables are sorted and aligned by their own host's rows and splicing them
// would mean re-deriving each one here. JSON is merged instead, each row
// tagged with its location, because a program reading it wants one list.

// remoteSections prints each other host's answer to argv after this host's,
// headed by its place. A host that has nothing to say (no parked rigs, an
// empty inbox) still gets its header, so silence reads as "asked, nothing"
// rather than "didn't ask".
func remoteSections(argv ...string) {
	hosts := remoteHosts()
	if len(hosts) == 0 {
		return
	}
	type section struct {
		out bytes.Buffer
		err error
	}
	results := eachHost(hosts, func(b servedBackend) *section {
		s := &section{}
		if servedBreaker.Down(b.URL) {
			s.err = errors.New("unreachable (retrying shortly)")
			return s
		}
		s.err = b.exec(execRequest{Argv: argv}, &s.out, &s.out)
		return s
	})
	for i, b := range hosts {
		fmt.Printf("\n── %s ──\n", surfacePlace(b.Surface()))
		_, _ = os.Stdout.Write(results[i].out.Bytes())
		if err := results[i].err; err != nil {
			if _, ok := errors.AsType[remoteExit](err); !ok {
				fmt.Printf("rig: %v\n", err)
			}
		}
	}
}

// remoteJSONRows runs argv (a --format=json command answering an array) on
// every other host and returns their rows with location set, plus each host's
// reachability, the same account ls gives in its hosts field.
func remoteJSONRows[T any](setLocation func(*T, string), argv ...string) ([]T, []hostBoard) {
	hosts := remoteHosts()
	type answer struct {
		rows []T
		host hostBoard
	}
	answers := eachHost(hosts, func(b servedBackend) answer {
		a := answer{host: hostBoard{Place: surfacePlace(b.Surface())}}
		if servedBreaker.Down(b.URL) {
			a.host.Error = "unreachable (retrying shortly)"
			return a
		}
		out, err := b.execCapture(argv...)
		if err != nil {
			a.host.Error = err.Error()
			return a
		}
		if err := json.Unmarshal(out, &a.rows); err != nil {
			a.host.Error = fmt.Sprintf("unreadable answer: %v", err)
			return a
		}
		a.host.Reachable = true
		for i := range a.rows {
			setLocation(&a.rows[i], a.host.Place)
		}
		return a
	})
	var rows []T
	var reach []hostBoard
	for _, a := range answers {
		rows = append(rows, a.rows...)
		reach = append(reach, a.host)
	}
	return rows, reach
}

// warnUnreachable says which hosts a merged JSON answer is missing, on
// stderr so the document stays parseable.
func warnUnreachable(hosts []hostBoard) {
	for _, h := range hosts {
		if !h.Reachable {
			fmt.Fprintf(os.Stderr, "rig: %s didn't answer: %s\n", h.Place, h.Error)
		}
	}
}
