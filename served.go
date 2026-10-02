package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/phinze/rig/internal/mux"
	"github.com/phinze/rig/internal/mux/tmux"
)

// servedBackend is a tmux host that runs `rig serve`: listed over HTTP,
// entered over ssh. It is the tmux+ssh surface with its listing swapped out,
// so everything that enters, switches, or kills still goes through the portal
// exactly as it did, and the only thing that stops using ssh is the board.
//
// Only the host's tmux sessions come through. Enter on a row needs a way in,
// and this surface's way in is a tmux portal; a Rex session on the same host
// would need a rex surface of its own.
type servedBackend struct {
	portalBackend
	URL string // the serve endpoint, http://host:port
}

// servedClient is bounded like a remote rex call: a board can't wait on a
// tailnet longer than that, and the radar asks again in a couple of seconds.
var servedClient = &http.Client{Timeout: 3 * time.Second}

// servedBreaker is keyed by URL. Only a failure to connect trips it; a serve
// that answered with an error is up.
var servedBreaker = &mux.Breaker{For: 30 * time.Second}

func newServedBackend(place, endpoint string) (servedBackend, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return servedBackend{}, fmt.Errorf("surface %s: rig endpoint %q should be http://host[:port]", place, endpoint)
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), servePort)
	}
	u.Path = ""
	return servedBackend{portalBackend{tmux.Backend{Host: u.Hostname(), Place: place}}, u.String()}, nil
}

func (b servedBackend) Endpoint() string { return b.URL }

func (b servedBackend) Unreachable() bool { return servedBreaker.Down(b.URL) }

// remoteBoard is one fetch's worth of a served host, with the place already
// applied: what radarRemoteCmd folds into the board.
type remoteBoard struct {
	icon     string // the host's chosen glyph, "" if it named none
	sessions []mux.Session
	panes    []mux.Pane
	rigs     []rigStatus
}

// boardSource is a surface that can describe its rigs and not only its
// sessions, in one round trip.
type boardSource interface {
	Board() (remoteBoard, error)
}

func (b servedBackend) Board() (remoteBoard, error) {
	doc, err := b.fetch()
	if err != nil {
		return remoteBoard{}, err
	}
	return b.lift(doc), nil
}

func (b servedBackend) fetch() (serveDoc, error) {
	if servedBreaker.Down(b.URL) {
		return serveDoc{}, errors.New("host unreachable (retrying shortly)")
	}
	resp, err := servedClient.Get(b.URL + "/v1/board")
	if err != nil {
		servedBreaker.Note(b.URL, true)
		return serveDoc{}, err
	}
	defer resp.Body.Close()
	servedBreaker.Note(b.URL, false)
	if resp.StatusCode != http.StatusOK {
		return serveDoc{}, fmt.Errorf("%s: %s", b.URL, resp.Status)
	}
	var doc serveDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return serveDoc{}, fmt.Errorf("%s: %w", b.URL, err)
	}
	return doc, nil
}

// lift turns the wire document into this machine's types, tagged with this
// surface. Rows from another kind of multiplexer on that host are dropped,
// since this surface has no way into them.
func (b servedBackend) lift(doc serveDoc) remoteBoard {
	surface := b.Surface()
	out := remoteBoard{icon: doc.Icon}
	for _, s := range doc.Sessions {
		if s.Kind != "tmux" {
			continue
		}
		out.sessions = append(out.sessions, mux.Session{Surface: surface, Name: s.Name, Path: s.Path, LastAttached: s.LastAttached})
	}
	for _, p := range doc.Panes {
		if p.Kind != "tmux" {
			continue
		}
		out.panes = append(out.panes, mux.Pane{
			Surface: surface, Session: p.Session,
			WindowIdx: p.WindowIdx, WindowName: p.WindowName, Target: p.Target,
			Command: p.Command, Path: p.Path, Title: p.Title, Activity: p.Activity,
		})
	}
	for _, r := range doc.Rigs {
		if r.Backend != "" && r.Backend != "tmux" {
			continue
		}
		s := r.rigStatus
		s.Repos = nil
		looked := false
		for _, repo := range r.Repos {
			s.Repos = append(s.Repos, rigRepo{Name: repo.Name, Branches: repo.Branches, WIP: repo.WIP, WIPUncertain: repo.WIPUncertain})
			if repo.PRs != nil {
				looked = true
				s.PRs = append(s.PRs, *repo.PRs...)
			}
		}
		s.Location = surfacePlace(surface)
		s.remote = &remoteRig{surface: surface, session: r.Session, prsLooked: looked, icon: doc.Icon}
		out.rigs = append(out.rigs, s)
	}
	return out
}

// Sessions and AllPanes answer from serve rather than ssh, for every caller
// that lists surfaces without asking for a board.
func (b servedBackend) Sessions() []mux.Session {
	board, _ := b.Board()
	return board.sessions
}

func (b servedBackend) AllPanes() ([]mux.Pane, error) {
	board, err := b.Board()
	return board.panes, err
}

func (b servedBackend) HasSession(name string) bool {
	for _, s := range b.Sessions() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// remoteRig marks a rig row another host's serve described. A rig still
// lives where its multiplexer runs; this is a view of one, and everything
// that would change it (park, wake, teardown) belongs on its own host.
type remoteRig struct {
	surface   string // the surface whose portal enters it
	session   string // its session name on that host
	prsLooked bool   // the host had PR answers for it, possibly none
	icon      string // the host's glyph; see serveDoc.Icon
}

// placeLabel is how a row from another host says where it lives: the host's
// icon when it chose one, else its name. The name is never lost, since
// filtering still matches on it.
func placeLabel(place, icon string) string {
	if icon != "" {
		return icon
	}
	return place + ":"
}
