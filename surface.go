package main

import (
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/phinze/rig/internal/mux"
	"github.com/phinze/rig/internal/mux/rex"
)

// A surface is a multiplexer somewhere else that this machine's boards list
// beside its own: the devbox's Rex server, say, seen from the Mac. Each one is
// a line in the config's [surfaces] table, keyed by the place name it shows
// under and valued kind+endpoint:
//
//	[surfaces]
//	devbox = "rex+https://devbox.tail1234.ts.net"
//
// A remote surface only ever contributes plain session rows. Rigs are local by
// construction (the basedir, manifest, and agent all live where the multiplexer
// runs), so the devbox's rigs are rigs to the rig binary on the devbox and
// sessions to everyone else. Listing them as rigs would mean reading another
// host's manifests, which is the `ssh rig ls` design PERS-17 set aside.
//
// Only rex has a remote form so far. tmux is spelled the same way when it gets
// one (tmux+ssh://host, attached through a portal session), which is why the
// kind rides in the value rather than being assumed.

// placePattern keeps a place name to something that reads cleanly after an @
// in a surface and as a label on a board row.
var placePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func validPlace(place string) bool { return placePattern.MatchString(place) }

// parseSurface turns one [surfaces] entry into a backend, or says why it
// can't. It never reaches the network: a surface is described here and only
// contacted by the listing that uses it.
func parseSurface(place, spec string) (mux.Backend, error) {
	if !validPlace(place) {
		return nil, fmt.Errorf("surface name %q: use lowercase letters, digits, and dashes", place)
	}
	kind, endpoint, ok := strings.Cut(spec, "+")
	if !ok || endpoint == "" {
		return nil, fmt.Errorf("surface %s = %q: want kind+endpoint, like rex+https://host", place, spec)
	}
	switch kind {
	case "rex":
		if !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "unix://") {
			return nil, fmt.Errorf("surface %s: rex endpoint %q should be http(s):// or unix:// (tailnet:// is listen-only)", place, endpoint)
		}
		return rex.Backend{Server: endpoint, Place: place}, nil
	default:
		return nil, fmt.Errorf("surface %s: no remote form for %q yet (rex is the only one)", place, kind)
	}
}

// surfaceBackends is every configured surface this binary can drive, in place
// order. A bad entry costs that surface and nothing else, the same bargain
// readRigConfig makes with a broken line. It says nothing about it here,
// because this runs inside the radar's scan, where a line on stderr would draw
// over the board; `rig config` is where a skipped surface explains itself. A
// rex surface on a machine without the rex CLI is skipped too, since that's an
// install question and not a typo.
func surfaceBackends() []mux.Backend {
	surfaces := readRigConfig().Surfaces
	var out []mux.Backend
	for _, place := range slices.Sorted(maps.Keys(surfaces)) {
		b, err := parseSurface(place, surfaces[place])
		if err != nil || (b.Name() == "rex" && !rex.Installed()) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// listSurfaces is `rig config`'s account of the [surfaces] table: each entry
// with the surface it became, or why it was skipped.
func listSurfaces(w io.Writer) {
	surfaces := readRigConfig().Surfaces
	if len(surfaces) == 0 {
		return
	}
	fmt.Fprintln(w, "\nsurfaces:")
	for _, place := range slices.Sorted(maps.Keys(surfaces)) {
		b, err := parseSurface(place, surfaces[place])
		switch {
		case err != nil:
			fmt.Fprintf(w, "  %s\tskipped: %v\n", place, err)
		case b.Name() == "rex" && !rex.Installed():
			fmt.Fprintf(w, "  %s\tskipped: the rex CLI isn't installed here\n", place)
		default:
			fmt.Fprintf(w, "  %s\t%s\n", b.Surface(), b.Endpoint())
		}
	}
}

// surfacePlace is the place half of a surface, "" for this machine's own.
func surfacePlace(surface string) string {
	_, place, _ := strings.Cut(surface, "@")
	return place
}

// remoteHome matches the home directory at the front of a path reported by
// another host, whose $HOME this process can't know. Both spellings are here
// because the far side of a Mac is usually Linux and the other way round.
var remoteHome = regexp.MustCompile(`^/(home|Users)/[^/]+`)

// remoteTitle is how a remote session reads on a board: its place, then its
// path with that host's home folded to ~. The place is what tells it apart
// from the local rig at the same path, which otherwise reads identically.
func remoteTitle(place, path string) string {
	if path == "" {
		return place + ":"
	}
	return place + ":" + remoteHome.ReplaceAllString(path, "~")
}
