package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/phinze/rig/internal/mux"
	"github.com/phinze/rig/internal/mux/tmux"
)

// servedBackend is a tmux host that runs `rig serve`: listed over HTTP,
// entered over ssh. It is the tmux+ssh surface with its listing swapped out,
// so everything that enters, switches, or kills still goes through the portal
// exactly as it did, and the only thing that stops using ssh is the board.
//
// The host's tmux rows are this surface's own. Its Rex rows come through too,
// under a sibling surface (servedRex) at the same place, because their way in
// is different: no portal, just the host telling Rex.app to show them.
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

// Attach enters a row from this host. When this machine can host a portal
// (it's on screen in Rex) it does, exactly as tmux+ssh would. When it can't,
// the screen you're looking at isn't this machine's: from foxtrotbase's
// radar, seen through the Mac's Rex, the client to move is the Mac's. So the
// host that owns the session is asked to show it, and its own rig moves its
// own screen.
func (b servedBackend) Attach(target string) error {
	adoptLocalPortal()
	if os.Getenv("REX_SESSION") != "" && localRex() != nil {
		return b.portalBackend.Attach(target)
	}
	return b.show(target)
}

func (b servedBackend) show(target string) error {
	return b.showAs("tmux", target)
}

func (b servedBackend) showAs(kind, target string) error {
	body, err := json.Marshal(showRequest{Kind: kind, Target: target})
	if err != nil {
		return err
	}
	resp, err := servedClient.Post(b.URL+"/v1/show", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("asking %s to show it: %v: %w", surfacePlace(b.Surface()), err, mux.ErrNoClientSwitch)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("%s couldn't show it: %s: %w", surfacePlace(b.Surface()), strings.TrimSpace(string(msg)), mux.ErrNoClientSwitch)
}

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
// sessions, in one round trip, and whose answer can be cached (remotecache.go).
type boardSource interface {
	mux.Backend
	Endpoint() string
	Board() (remoteBoard, error)
	poll(now time.Time, watched bool) remoteCacheEntry
	lift(serveDoc) remoteBoard
}

func (b servedBackend) Board() (remoteBoard, error) {
	doc, err := b.fetch(false)
	if err != nil {
		return remoteBoard{}, err
	}
	return b.lift(doc), nil
}

// fetch asks for the board. watched says a person is looking at the answer,
// which is what tells the far host its PRs are worth refreshing (see
// refreshServedPRs); a poll serve makes on its own backoff never says so.
func (b servedBackend) fetch(watched bool) (serveDoc, error) {
	if servedBreaker.Down(b.URL) {
		return serveDoc{}, errors.New("host unreachable (retrying shortly)")
	}
	endpoint := b.URL + "/v1/board"
	if watched {
		endpoint += "?watched=1"
	}
	resp, err := servedClient.Get(endpoint)
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

// lift turns the wire document into this machine's types, each row tagged
// with the surface that enters it: this one for tmux, its Rex sibling for
// Rex. Rows from any other kind of multiplexer are dropped, since neither has
// a way into them.
func (b servedBackend) lift(doc serveDoc) remoteBoard {
	out := remoteBoard{icon: doc.Icon}
	for _, s := range doc.Sessions {
		surface, ok := b.surfaceFor(s.Kind)
		if !ok {
			continue
		}
		out.sessions = append(out.sessions, mux.Session{Surface: surface, Name: s.Name, Path: s.Path, LastAttached: s.LastAttached})
	}
	for _, p := range doc.Panes {
		surface, ok := b.surfaceFor(p.Kind)
		if !ok {
			continue
		}
		out.panes = append(out.panes, mux.Pane{
			Surface: surface, Session: p.Session,
			WindowIdx: p.WindowIdx, WindowName: p.WindowName, Target: p.Target,
			Command: p.Command, Path: p.Path, Title: p.Title, Activity: p.Activity,
		})
	}
	for _, r := range doc.Rigs {
		kind := r.Backend
		if kind == "" {
			kind = "tmux"
		}
		surface, ok := b.surfaceFor(kind)
		if !ok {
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

// surfaceFor is the surface that enters a row the host listed under kind.
func (b servedBackend) surfaceFor(kind string) (string, bool) {
	switch kind {
	case "tmux":
		return b.Surface(), true
	case "rex":
		return b.rex().Surface(), true
	}
	return "", false
}

// Sessions and AllPanes answer from serve rather than ssh, for every caller
// that lists surfaces without asking for a board. Each surface answers for
// its own rows only, so a caller that lists both never counts one twice.
func (b servedBackend) Sessions() []mux.Session {
	board, _ := b.Board()
	return sessionsOn(board.sessions, b.Surface())
}

func (b servedBackend) AllPanes() ([]mux.Pane, error) {
	board, err := b.Board()
	return panesOn(board.panes, b.Surface()), err
}

func sessionsOn(all []mux.Session, surface string) []mux.Session {
	var out []mux.Session
	for _, s := range all {
		if s.Surface == surface {
			out = append(out, s)
		}
	}
	return out
}

func panesOn(all []mux.Pane, surface string) []mux.Pane {
	var out []mux.Pane
	for _, p := range all {
		if p.Surface == surface {
			out = append(out, p)
		}
	}
	return out
}

// servedRex is the Rex half of a served host: rex@place beside its
// tmux@place. Rex.app already shows that host's sessions once it's added
// under Remote Hosts, so entering one needs no portal and no ssh. The host
// is asked to show it, and its rig runs session.select on its own server,
// which relays the action to the app: the Mac's, since that's the only one.
// Asking the host rather than selecting from here is what lets it focus the
// agent's block first, which is session state on its server, not the app's.
//
// It is never one of knownBackends: its parent's board already carries its
// rows, so listing it again would draw them twice. surfaceNamed finds it
// through the parent instead.
type servedRex struct {
	servedBackend
}

func (b servedBackend) rex() servedRex { return servedRex{b} }

func (b servedRex) Name() string    { return "rex" }
func (b servedRex) Surface() string { return "rex@" + b.Place }

func (b servedRex) Attach(target string) error { return b.showAs("rex", target) }

func (b servedRex) Sessions() []mux.Session {
	board, _ := b.Board()
	return sessionsOn(board.sessions, b.Surface())
}

func (b servedRex) AllPanes() ([]mux.Pane, error) {
	board, err := b.Board()
	return panesOn(board.panes, b.Surface()), err
}

func (b servedRex) HasSession(name string) bool {
	for _, s := range b.Sessions() {
		if s.Name == name {
			return true
		}
	}
	return false
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
