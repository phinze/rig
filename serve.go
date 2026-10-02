package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phinze/rig/internal/mux"
	"github.com/phinze/rig/internal/mux/rex"
)

// `rig serve` is how another machine's radar learns about this one without
// ssh. It answers one question, "what's on this host", with the
// rows this host's own radar would draw: rigs with their PR state, sessions,
// and agent panes. The radar on the Mac asks it through a rig+http surface
// (served.go) and gets real rig rows back instead of bare sessions, and ssh
// is left for the one thing that needs a terminal, entering a session.
//
// It does one thing besides answer: show a session on this host's screen,
// for a radar on a host that can't reach this one's client. That's the
// foxtrotbase radar, seen through the Mac's Rex, entering a Mac rig: the
// screen is the Mac's, so the Mac moves it. It changes what's displayed and
// nothing else, and only for a session or agent pane the board lists.
//
// Identity is the tailnet's. Every request's peer address goes through
// `tailscale whois`, and only a caller on the --allow list gets an answer.
// The list is explicit rather than "whoever owns this node" because a server
// is often a tagged device, whose owner is the tag and not a person.

// serveVersion is the wire document's version. A reader ignores fields it
// doesn't know, so additions don't bump it; a change in meaning does.
const serveVersion = 1

// servePort is where serve listens unless told otherwise, and what a
// rig+http surface without a port dials.
const servePort = "7744"

// serveDoc is the whole answer. Rigs use `rig ls`'s row shape, which is
// already rig's compatibility boundary, plus the session each lives in, since
// a session name is derived from the path relative to *this* host's home and
// the reader can't recompute it.
type serveDoc struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generatedAt"`
	// Icon is the glyph this host chose to stand for it on other machines'
	// boards (--icon). The host picks it rather than each viewer, so every
	// radar that can see a machine draws it the same way.
	Icon     string         `json:"icon,omitempty"`
	Rigs     []serveRig     `json:"rigs"`
	Sessions []serveSession `json:"sessions"`
	// Panes are agent panes only. Every other pane is noise to a board, and
	// a busy host has hundreds.
	Panes []servePane `json:"panes"`
}

type serveRig struct {
	rigRowJSON
	Session string `json:"session"`
}

// serveSession and servePane are mux's types on the wire. Kind is the
// backend kind that listed them (tmux, rex); the reader adds the place.
type serveSession struct {
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	LastAttached int64  `json:"lastAttached"`
}

type servePane struct {
	Kind       string `json:"kind"`
	Session    string `json:"session"`
	WindowIdx  string `json:"windowIdx"`
	WindowName string `json:"windowName"`
	Target     string `json:"target"`
	Command    string `json:"command"`
	Path       string `json:"path"`
	Title      string `json:"title"`
	Activity   int64  `json:"activity"`
}

func runServe(args []string) error {
	listen, icon := "", ""
	var allow []string
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		if !hasVal && (name == "--listen" || name == "--allow" || name == "--icon") {
			if i+1 >= len(args) {
				return fmt.Errorf("rig serve: %s needs a value", name)
			}
			i++
			val = args[i]
		}
		switch name {
		case "--listen":
			listen = val
		case "--allow":
			allow = append(allow, val)
		case "--icon":
			icon = val
		default:
			return fmt.Errorf("usage: rig serve --allow LOGIN|NODE|tag:TAG [--allow ...] [--listen ADDR] [--icon GLYPH]")
		}
	}
	if len(allow) == 0 {
		return fmt.Errorf("rig serve: name at least one --allow (a tailnet login like you@github, a node name, or tag:NAME)")
	}
	if listen == "" {
		ip, err := tailnetIPv4()
		if err != nil {
			return fmt.Errorf("rig serve: no --listen given and no tailnet address to default to: %w", err)
		}
		listen = net.JoinHostPort(ip, servePort)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	g := newServeGate(allow, tailscaleWhois, isLoopbackAddr(ln.Addr()))
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go refreshServedPRs(ctx, home, servePRCheckEvery)
	go pollServedSurfaces(ctx, servePollEvery)

	fmt.Fprintf(os.Stderr, "rig serve: listening on %s for %s\n", ln.Addr(), strings.Join(allow, ", "))
	build := func() serveDoc {
		doc := buildServeDoc(home, localBackends())
		doc.Icon = icon
		return doc
	}
	srv := &http.Server{Handler: serveHandler(g, build), ReadHeaderTimeout: 5 * time.Second}
	return srv.Serve(ln)
}

func serveHandler(g *serveGate, build func() serveDoc) http.Handler {
	h := http.NewServeMux()
	h.HandleFunc("POST /v1/show", func(w http.ResponseWriter, r *http.Request) {
		if err := g.check(r.Context(), r.RemoteAddr); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		var req showRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "unreadable request", http.StatusBadRequest)
			return
		}
		if err := show(req); err != nil {
			code := http.StatusConflict // this host couldn't, which isn't the caller's mistake
			if errors.Is(err, errNotListed) {
				code = http.StatusNotFound
			}
			http.Error(w, err.Error(), code)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h.HandleFunc("GET /v1/board", func(w http.ResponseWriter, r *http.Request) {
		if err := g.check(r.Context(), r.RemoteAddr); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("watched") == "1" {
			peerLooked.Store(time.Now().UnixNano())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(build())
	})
	return h
}

// buildServeDoc is what this host's radar would scan, in wire form. It's the
// radar's local tier and not `ls --full`: PRs come from the shared radar
// cache, which refreshServedPRs keeps warm, and nothing here runs jj or gh,
// because a reader polls this every couple of seconds.
func buildServeDoc(home string, backends []mux.Backend) serveDoc {
	now := time.Now()
	doc := serveDoc{Version: serveVersion, GeneratedAt: now, Rigs: []serveRig{}, Sessions: []serveSession{}, Panes: []servePane{}}
	if rigs, err := listRigs(); err == nil {
		cache := loadRadarCache()
		for _, s := range rigStatuses(rigs, home, now) {
			e, looked := cache[s.Slug]
			s.PRs = e.PRs
			doc.Rigs = append(doc.Rigs, serveRig{
				rigRowJSON: rigsForJSON([]rigStatus{s}, looked)[0],
				Session:    rigSessionName(s.Path),
			})
		}
	}
	for _, s := range allSessions(backends) {
		doc.Sessions = append(doc.Sessions, serveSession{Kind: surfaceKind(s.Surface), Name: s.Name, Path: s.Path, LastAttached: s.LastAttached})
	}
	for _, ps := range eachBackend(backends, func(b mux.Backend) []mux.Pane {
		ps, _ := b.AllPanes()
		return ps
	}) {
		for _, p := range ps {
			if !isAgentPane(p) {
				continue
			}
			doc.Panes = append(doc.Panes, servePane{
				Kind: surfaceKind(p.Surface), Session: p.Session,
				WindowIdx: p.WindowIdx, WindowName: p.WindowName, Target: p.Target,
				Command: p.Command, Path: p.Path, Title: p.Title, Activity: p.Activity,
			})
		}
	}
	return doc
}

// showRequest asks for a session or agent pane to be put on this host's
// screen. Kind is the backend kind the board listed it under.
type showRequest struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

var errNotListed = errors.New("not on this host's board")

// show is the handler's half that does something, behind a variable so a
// test can stand in for a screen it doesn't have.
var show = func(req showRequest) error {
	b, err := backendByName(req.Kind)
	if err != nil {
		return err
	}
	if !listsTarget(b, req.Target) {
		return fmt.Errorf("%q: %w", req.Target, errNotListed)
	}
	switch b := b.(type) {
	case portalBackend:
		return b.Show(req.Target)
	case rex.Backend:
		b.Viewer = true
		return b.Attach(req.Target)
	default:
		return fmt.Errorf("%s can't show a session: %w", b.Name(), mux.ErrNoClientSwitch)
	}
}

// listsTarget is whether a target is one the board would have offered: a
// live session, or an agent pane's target. Anything else is refused before it
// reaches a multiplexer, so a caller can only ever ask for what it was shown.
func listsTarget(b mux.Backend, target string) bool {
	if target == "" {
		return false
	}
	for _, s := range b.Sessions() {
		if s.Name == target {
			return true
		}
	}
	panes, _ := b.AllPanes()
	for _, p := range panes {
		if isAgentPane(p) && p.Target == target {
			return true
		}
	}
	return false
}

// surfaceKind is the kind half of a surface.
func surfaceKind(surface string) string {
	kind, _, _ := strings.Cut(surface, "@")
	return kind
}

// peerLooked is when another host's radar last said it was watching this
// board, in UnixNano; zero if none has.
var peerLooked atomic.Int64

// servePRCheckEvery is how often serve's PR refresher asks whether anyone is
// watching. It's an atomic load until somebody is.
const servePRCheckEvery = 5 * time.Second

// refreshServedPRs refreshes this host's PR answers while another host's
// radar is watching them, and does nothing otherwise. Nobody else needs it to:
// a radar here fetches its own rigs' PRs while it's open, and `ls --full` asks
// for itself, but a radar elsewhere draws remote rigs from this host's answer
// and can't fetch them. So the only viewer serve stands in for is a peer, and
// a peer only says it's watching while a person is (served.go fetch), never on
// serve's own backoff, which is what keeps two hosts from keeping each other's
// GitHub traffic awake. With nobody looking anywhere, rig asks GitHub nothing.
func refreshServedPRs(ctx context.Context, home string, every time.Duration) {
	for {
		if now := time.Now(); now.Sub(time.Unix(0, peerLooked.Load())) < radarWatchedFor {
			refreshStalePRs(home, now)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func refreshStalePRs(home string, now time.Time) {
	rigs, err := listRigs()
	if err != nil {
		return
	}
	cache := loadRadarCache()
	var stale []rigStatus
	for _, s := range rigStatuses(rigs, home, now) {
		if e, ok := cache[s.Slug]; ok {
			s.PRs = e.PRs
			if now.Sub(e.At) < prTTL(s, now) {
				continue
			}
		}
		stale = append(stale, s)
	}
	if len(stale) == 0 {
		return
	}
	enrichWithPRs(stale)
	// Reload before writing so a radar that saved in the meantime keeps its
	// answers; ours win only for the rigs we just fetched.
	prs, fetchedAt := map[string][]rigPR{}, map[string]time.Time{}
	for slug, e := range loadRadarCache() {
		prs[slug], fetchedAt[slug] = e.PRs, e.At
	}
	for _, s := range stale {
		prs[s.Slug], fetchedAt[s.Slug] = s.PRs, now
	}
	saveRadarCache(prs, fetchedAt)
}

// whoisIdentity is who a tailnet address belongs to.
type whoisIdentity struct {
	Login string   // the owning user's login, "tagged-devices" for a tagged node
	Node  string   // the node's short name
	Tags  []string // the node's ACL tags, tag:NAME
}

type whoisFunc func(ctx context.Context, ip string) (whoisIdentity, error)

// tailscaleWhois asks the local tailscaled who an address is. It shells out
// rather than linking tailscale's client, which is the same trade rig makes
// with jj, gh, and tmux.
func tailscaleWhois(ctx context.Context, ip string) (whoisIdentity, error) {
	out, err := exec.CommandContext(ctx, "tailscale", "whois", "--json", ip).Output()
	if err != nil {
		return whoisIdentity{}, fmt.Errorf("tailscale whois %s: %w", ip, err)
	}
	var raw struct {
		Node struct {
			Name string
			Tags []string
		}
		UserProfile struct{ LoginName string }
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return whoisIdentity{}, fmt.Errorf("tailscale whois %s: %w", ip, err)
	}
	node, _, _ := strings.Cut(raw.Node.Name, ".")
	return whoisIdentity{Login: raw.UserProfile.LoginName, Node: node, Tags: raw.Node.Tags}, nil
}

func tailnetIPv4() (string, error) {
	out, err := exec.Command("tailscale", "ip", "-4").Output()
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if ip == "" {
		return "", errors.New("tailscale reported no IPv4 address")
	}
	return ip, nil
}

func isLoopbackAddr(a net.Addr) bool {
	tcp, ok := a.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

// serveGate decides whether a peer gets an answer. Answers are cached per
// address for a few minutes, since the reader polls and each whois is a
// process; a failed whois is never cached, so a hiccup doesn't lock anyone out
// for longer than one request.
type serveGate struct {
	allow    map[string]bool
	whois    whoisFunc
	loopback bool // listening on loopback, where there's no tailnet to ask

	mu    sync.Mutex
	known map[string]gateEntry
}

type gateEntry struct {
	ok bool
	at time.Time
}

const gateTTL = 5 * time.Minute

func newServeGate(allow []string, whois whoisFunc, loopback bool) *serveGate {
	g := &serveGate{allow: map[string]bool{}, whois: whois, loopback: loopback, known: map[string]gateEntry{}}
	for _, a := range allow {
		g.allow[a] = true
	}
	return g
}

func (g *serveGate) check(ctx context.Context, remoteAddr string) error {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return fmt.Errorf("unreadable peer address")
	}
	// A server bound to loopback is a test or a local experiment, and anyone
	// who can reach it can already read this user's files or isn't on this
	// machine at all. A tailnet listener never takes this path.
	if g.loopback {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	g.mu.Lock()
	e, ok := g.known[host]
	g.mu.Unlock()
	if ok && time.Since(e.at) < gateTTL {
		if e.ok {
			return nil
		}
		return fmt.Errorf("%s is not allowed", host)
	}
	id, err := g.whois(ctx, host)
	if err != nil {
		return fmt.Errorf("can't identify %s", host)
	}
	allowed := g.allows(id)
	g.mu.Lock()
	g.known[host] = gateEntry{ok: allowed, at: time.Now()}
	g.mu.Unlock()
	if !allowed {
		return fmt.Errorf("%s (%s) is not allowed", host, id.Node)
	}
	return nil
}

// allows matches an identity against the list. A tagged node's login is the
// placeholder "tagged-devices", which names no one, so it never matches; a
// tagged caller has to be allowed by node name or tag.
func (g *serveGate) allows(id whoisIdentity) bool {
	if id.Login != "" && id.Login != "tagged-devices" && g.allow[id.Login] {
		return true
	}
	if id.Node != "" && g.allow[id.Node] {
		return true
	}
	for _, t := range id.Tags {
		if g.allow[t] {
			return true
		}
	}
	return false
}
