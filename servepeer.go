package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// The window shows rigs on other hosts too. Each host's serve already renders
// its own rigs and follows its own Rex app, so a frame here doesn't need to
// know anything about a rig elsewhere: it names it slug@place, and this serve
// carries the request to that place's serve with the suffix taken off. The
// window stays one origin, which an installed web app and sameOrigin both
// need, the same reason /alt/ is proxied rather than linked.
//
// The far serve admits this host by its node name, as it already does for
// /v1/exec, and this serve's own gate has admitted the person before anything
// is carried. Only places in the surfaces table are reachable, so a caller
// can't aim this at an arbitrary host.
//
// What's on screen is whichever host saw a switch most recently. A host
// reports nothing when the app moves to another host's session, so the
// newest report is the current one. Only a peer's rigs count: a bare session
// there says nothing about where the person is.

// peerFocus is the last focus heard from one place and, while its stream is
// down, why.
type peerFocus struct {
	f   webFocus
	err string
}

type peerFocuses struct {
	byPlace  map[string]peerFocus
	watchers int
	cancel   context.CancelFunc
	linger   *time.Timer
}

// peerLinger is how long peer streams outlive the last frame, so a frame
// that reconnects (EventSource retries in two seconds) doesn't redial every
// host.
var peerLinger = 30 * time.Second

// peerIdle is how long a peer stream or proxied response may go without a
// byte before it's taken as gone. Both serve and recto send a keepalive every
// 15s, and a laptop that sleeps mid-stream drops the connection without a
// word, so silence is the only sign.
var peerIdle = 45 * time.Second

// peerDial bounds reaching a peer. A sleeping laptop doesn't refuse a
// connection, it just never answers.
var peerDial = 3 * time.Second

// peerTransport carries everything sent to another host's serve. A failure to
// connect trips the same breaker the radar uses, so a laptop that's gone
// costs one timeout rather than one per request.
var peerTransport = &http.Transport{
	DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{Timeout: peerDial}
		conn, err := d.DialContext(ctx, network, addr)
		if ctx.Err() == nil {
			servedBreaker.Note("http://"+addr, err != nil)
		}
		return conn, err
	},
	// A show waits on the far host's Rex, as showClient allows for.
	ResponseHeaderTimeout: 15 * time.Second,
	IdleConnTimeout:       90 * time.Second,
}

var errPeerDown = errors.New("unreachable")

// peerUnreachable is what a frame reads while a place can't be reached.
func peerUnreachable(place string) string {
	return place + " isn't answering: asleep or off the tailnet? (retrying)"
}

// mergedLocked is the focus a frame should show: the newest of this host's
// and every peer's rig. w.mu must be held.
func (w *serveWeb) mergedLocked() webFocus {
	best, bestErr := w.cur, ""
	for _, p := range w.peers.byPlace {
		if p.f.Rig != "" && p.f.Since > best.Since {
			best, bestErr = p.f, p.err
		}
	}
	if bestErr != "" {
		best.Error = strings.TrimPrefix(best.Error+"; "+bestErr, "; ")
	}
	return best
}

func (w *serveWeb) setPeer(place string, p peerFocus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.peers.byPlace == nil {
		w.peers.byPlace = map[string]peerFocus{}
	}
	w.peers.byPlace[place] = p
	w.broadcastLocked()
}

func (w *serveWeb) peerFailed(place string, msg string) {
	w.mu.Lock()
	p := w.peers.byPlace[place]
	w.mu.Unlock()
	p.err = msg
	w.setPeer(place, p)
}

// watchPeers starts following every peer's focus when the first frame
// arrives. Nobody looking means no stream held open to a laptop.
func (w *serveWeb) watchPeers() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.peers.watchers++
	if w.peers.linger != nil {
		w.peers.linger.Stop()
		w.peers.linger = nil
	}
	if w.peers.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.peers.cancel = cancel
	for _, b := range peerServes() {
		go w.followPeer(ctx, surfacePlace(b.Surface()), b.URL)
	}
}

// unwatchPeers lets the streams go a while after the last frame leaves.
func (w *serveWeb) unwatchPeers() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.peers.watchers--
	if w.peers.watchers > 0 || w.peers.cancel == nil {
		return
	}
	w.peers.linger = time.AfterFunc(peerLinger, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.peers.watchers == 0 && w.peers.cancel != nil {
			w.peers.cancel()
			w.peers.cancel = nil
		}
	})
}

// peerServes is every served surface other than this host, which a surfaces
// table copied between machines may list.
func peerServes() []servedBackend {
	var out []servedBackend
	for _, s := range servedSurfaces() {
		if b, ok := s.(servedBackend); ok && surfacePlace(b.Surface()) != thisPlace() {
			out = append(out, b)
		}
	}
	return out
}

// followPeer holds a place's own focus stream open for the life of ctx,
// redialing one that drops.
func (w *serveWeb) followPeer(ctx context.Context, place, endpoint string) {
	for ctx.Err() == nil {
		err := streamPeerFocus(ctx, endpoint, func(f webFocus) {
			w.setPeer(place, peerFocus{f: qualifyFocus(f, place)})
		})
		if ctx.Err() != nil {
			return
		}
		msg := peerUnreachable(place)
		if !errors.Is(err, errPeerDown) {
			msg = fmt.Sprintf("lost %s's focus: %v (retrying)", place, err)
		}
		w.peerFailed(place, msg)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// qualifyFocus names a peer's rig the way the frame addresses it.
func qualifyFocus(f webFocus, place string) webFocus {
	if f.Rig != "" {
		f.Rig += "@" + place
		if f.ID != "" {
			f.ID += "@" + place
		}
	}
	// The peer's own focus troubles are its frame's to show, not this one's.
	f.Error = ""
	return f
}

// streamPeerFocus reads one place's /v1/focus?local=1 until it ends. An older
// serve ignores local and sends its own focus anyway, which is the same thing.
func streamPeerFocus(ctx context.Context, endpoint string, seen func(webFocus)) error {
	if servedBreaker.Down(endpoint) {
		return errPeerDown
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/v1/focus?local=1", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: peerTransport}).Do(req)
	if err != nil {
		if servedBreaker.Down(endpoint) {
			return errPeerDown
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	sc := bufio.NewScanner(newIdleReader(resp.Body, peerIdle, cancel))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var f webFocus
		if json.Unmarshal([]byte(data), &f) == nil {
			seen(f)
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() != nil {
		return errPeerDown // went silent
	}
	return errors.New("stream ended")
}

// idleReader calls stop when the reader under it goes quiet for too long, so
// a peer that vanished mid-stream frees whatever is waiting on it.
type idleReader struct {
	r io.ReadCloser
	t *time.Timer
	d time.Duration
}

func newIdleReader(r io.ReadCloser, d time.Duration, stop func()) *idleReader {
	return &idleReader{r: r, d: d, t: time.AfterFunc(d, stop)}
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.t.Reset(i.d)
	}
	return n, err
}

func (i *idleReader) Close() error {
	i.t.Stop()
	return i.r.Close()
}

// peered sends a {rig} route that names slug@place to that place's serve, and
// lets every other request through to next. A place that is this host is
// just a longer spelling of a local rig.
func peered(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		slug, place := splitPlace(r.PathValue("rig"))
		if place == "" {
			next(rw, r)
			return
		}
		if place == thisPlace() {
			r.SetPathValue("rig", slug)
			next(rw, r)
			return
		}
		b, ok := servedAt(place)
		if !ok {
			peerError(rw, r, http.StatusNotFound, fmt.Sprintf("no surface named %q on this host", place))
			return
		}
		proxyToPeer(rw, r, b.URL, place, slug)
	}
}

func proxyToPeer(rw http.ResponseWriter, r *http.Request, endpoint, place, slug string) {
	if servedBreaker.Down(endpoint) {
		peerError(rw, r, http.StatusBadGateway, peerUnreachable(place))
		return
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		peerError(rw, r, http.StatusBadGateway, err.Error())
		return
	}
	// Every {rig} route is /v1/<route>/<rig>/...: the rig is the third
	// segment, and only it changes.
	segs := strings.Split(r.URL.EscapedPath(), "/")
	if len(segs) < 4 {
		peerError(rw, r, http.StatusNotFound, "no rig in "+r.URL.Path)
		return
	}
	segs[3] = url.PathEscape(slug)
	path := strings.Join(segs, "/")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.RawPath = path
			pr.Out.URL.Path, _ = url.PathUnescape(path)
			pr.Out.Host = target.Host
			// sameOrigin ran here already; the far serve would only see a
			// foreign origin.
			pr.Out.Header.Del("Origin")
		},
		Transport:     peerTransport,
		FlushInterval: -1, // recto's event stream has to arrive as it's written
		ModifyResponse: func(resp *http.Response) error {
			// Closing the body under a blocked read ends it, and the proxy
			// with it, so the page's stream reconnects and gets the
			// unreachable page instead of waiting forever.
			body := resp.Body
			resp.Body = newIdleReader(body, peerIdle, func() { body.Close() })
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, r *http.Request, err error) {
			msg := fmt.Sprintf("%s's serve didn't answer: %v", place, err)
			if servedBreaker.Down(endpoint) {
				msg = peerUnreachable(place)
			}
			peerError(rw, r, http.StatusBadGateway, msg)
		},
	}
	proxy.ServeHTTP(rw, r)
}

// peerError answers in the shape the route's caller reads: a page in the
// frame's iframe, which retries itself on a 502, or JSON for the frame's own
// calls.
func peerError(rw http.ResponseWriter, r *http.Request, code int, msg string) {
	if strings.HasPrefix(r.URL.Path, "/v1/surface/") {
		surfaceError(rw, code, msg)
		return
	}
	jsonError(rw, code, errors.New(msg))
}

// peerRowsClient bounds the switcher's wait on any one host, like a board's.
var peerRowsClient = &http.Client{Transport: peerTransport, Timeout: 3 * time.Second}

// peerSwitchRows asks every peer for its own rigs at once, named slug@place,
// and names the places that couldn't answer so the switcher's silence about
// them isn't mistaken for "no rigs there".
func peerSwitchRows(ctx context.Context) (rows []switchRow, down []string) {
	peers := peerServes()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, b := range peers {
		place := surfacePlace(b.Surface())
		wg.Go(func() {
			got, err := fetchPeerRows(ctx, b.URL)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				down = append(down, place)
				return
			}
			for _, row := range got {
				row.Slug += "@" + place
				row.ID += "@" + place
				rows = append(rows, row)
			}
		})
	}
	wg.Wait()
	sort.Strings(down)
	return rows, down
}

func fetchPeerRows(ctx context.Context, endpoint string) ([]switchRow, error) {
	if servedBreaker.Down(endpoint) {
		return nil, errPeerDown
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/v1/rigs?local=1", nil)
	if err != nil {
		return nil, err
	}
	resp, err := peerRowsClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	var doc struct {
		Rigs []switchRow `json:"rigs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	return doc.Rigs, nil
}

// mergeSwitchRows folds peers' rows in by recency. A row with no time (a
// peer too old to send one) keeps its place after those with one.
func mergeSwitchRows(local, peers []switchRow) []switchRow {
	out := slices.Concat(local, peers)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Touched > out[j].Touched })
	return out
}
