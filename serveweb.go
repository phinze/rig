package main

import (
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/phinze/rig/internal/mux"
)

// serve's web side is a window that follows what the person is looking at.
// A frame page (/) listens to /v1/focus, a stream of which rig the person's
// Rex app is showing, and swaps in /v1/surface/<rig>/: that rig's repos as
// recto renders them. Recto does the rendering behind a unix socket and never
// listens on TCP; serve is the only door, so the whois gate in front of every
// other route is in front of this one too.
//
// Nothing here changes a rig. The proxy is not a forwarded verb, and focus is
// read from the backend's own event stream rather than polled, so a frame
// left open costs one long-lived process and no per-row calls.

//go:embed serveweb.html
var serveFrameHTML []byte

// webFocus is what /v1/focus sends: the rig on screen, or the bare session
// when the app is showing something that isn't a rig. Error is set while the
// focus stream itself is broken, so the frame can say so instead of holding
// a stale rig as if it were current.
type webFocus struct {
	Rig     string `json:"rig,omitempty"`
	ID      string `json:"id,omitempty"`
	Title   string `json:"title,omitempty"`
	Session string `json:"session,omitempty"`
	Client  string `json:"client,omitempty"`
	Since   int64  `json:"since,omitempty"`
	Error   string `json:"error,omitempty"`
}

type serveWeb struct {
	rectoSock string
	// rectoDone closes once runRecto has returned and its child is gone, so
	// shutdown can wait for it rather than exit with recto still running.
	rectoDone chan struct{}

	mu       sync.Mutex
	cur      webFocus
	rectoErr string
	subs     map[chan webFocus]struct{}

	// starts caches recovered workspace starts by root. Recovery walks jj's
	// operation log, and the page asks on every refresh. Failures are cached too: a workspace whose
	// creation has left the op log won't grow it back.
	startsMu sync.Mutex
	starts   map[string]string
}

func newServeWeb(rectoSock string) *serveWeb {
	return &serveWeb{
		rectoSock: rectoSock,
		rectoDone: make(chan struct{}),
		subs:      map[chan webFocus]struct{}{},
		starts:    map[string]string{},
	}
}

// rectoSocketPath is serve's own recto socket, apart from the default one so
// a recto serve someone runs by hand doesn't fight this one for the path.
func rectoSocketPath() (string, error) {
	dir, err := rigRuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "recto-serve.sock"), nil
}

// lastFocusPath remembers the most recent focus across restarts. Focus only
// arrives when the person switches sessions, so without it a restarted serve
// would show nothing until the next switch.
func lastFocusPath() string {
	dir, err := rigStateDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "serve-focus.json")
}

// restoreFocus seeds the stream with the remembered focus, re-resolved in case
// the rig went away while serve was down.
func (w *serveWeb) restoreFocus() {
	path := lastFocusPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var f webFocus
	if json.Unmarshal(raw, &f) != nil || f.Session == "" {
		return
	}
	since := f.Since
	f = resolveFocus(mux.Focus{Session: f.Session, Client: f.Client})
	f.Since = since
	w.mu.Lock()
	w.cur = f
	w.mu.Unlock()
}

func (w *serveWeb) publish(f webFocus) {
	if f.Error == "" {
		if path := lastFocusPath(); path != "" {
			if raw, err := json.Marshal(f); err == nil {
				_ = os.MkdirAll(filepath.Dir(path), 0o755)
				_ = writeFileAtomic(path, raw, 0o644)
			}
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cur = f
	for ch := range w.subs {
		// A reader that fell behind should end on the newest focus, not the
		// oldest it has queued: make room by dropping its oldest.
		select {
		case ch <- f:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- f:
			default:
			}
		}
	}
}

func (w *serveWeb) setFocusError(msg string) {
	w.mu.Lock()
	f := w.cur
	w.mu.Unlock()
	f.Error = msg
	w.publish(f)
}

func (w *serveWeb) clearFocusError() {
	w.mu.Lock()
	f := w.cur
	w.mu.Unlock()
	if f.Error != "" {
		f.Error = ""
		w.publish(f)
	}
}

// streamSettled is how long a restarted focus stream has to stay up before
// its earlier failure stops being news. Focus itself only arrives when the
// person switches sessions, so it can't be what clears the banner.
const streamSettled = 10 * time.Second

// follow runs every backend's focus stream for the life of ctx, restarting a
// stream that ends.
func (w *serveWeb) follow(ctx context.Context, backends []mux.Backend) {
	watching := 0
	for _, b := range backends {
		fw, ok := b.(mux.FocusWatcher)
		if !ok {
			continue
		}
		watching++
		go func() {
			for ctx.Err() == nil {
				settled := time.AfterFunc(streamSettled, w.clearFocusError)
				err := fw.WatchFocus(ctx, func(f mux.Focus) { w.publish(resolveFocus(f)) })
				settled.Stop()
				if ctx.Err() != nil {
					return
				}
				w.setFocusError(fmt.Sprintf("lost %s focus: %v (retrying)", b.Surface(), err))
				select {
				case <-ctx.Done():
				case <-time.After(5 * time.Second):
				}
			}
		}()
	}
	if watching == 0 {
		w.setFocusError("no multiplexer on this host reports which session you're looking at")
	}
}

func resolveFocus(f mux.Focus) webFocus {
	out := webFocus{Session: f.Session, Client: f.Client, Since: time.Now().UnixMilli()}
	rigs, err := listRigs()
	if err != nil {
		out.Error = "can't list rigs: " + err.Error()
		return out
	}
	for _, r := range rigs {
		if rigSessionName(r.Path) == f.Session {
			out.Rig, out.ID, out.Title = r.Slug, r.ID, r.Title
			break
		}
	}
	return out
}

// runRecto keeps `recto serve` running on serve's socket. A recto that won't
// start is recorded so the surface route can say why instead of a bare 502.
func (w *serveWeb) runRecto(ctx context.Context) {
	defer close(w.rectoDone)
	if err := os.MkdirAll(filepath.Dir(w.rectoSock), 0o700); err != nil {
		w.setRectoErr(err.Error())
		return
	}
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "recto", "serve", "--socket", w.rectoSock)
		cmd.Stderr = os.Stderr
		started := time.Now()
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		w.setRectoErr(fmt.Sprintf("recto serve exited after %s: %v", time.Since(started).Round(time.Second), err))
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

func (w *serveWeb) setRectoErr(msg string) {
	fmt.Fprintf(os.Stderr, "rig serve: %s\n", msg)
	w.mu.Lock()
	w.rectoErr = msg
	w.mu.Unlock()
}

func (w *serveWeb) mount(h *http.ServeMux, g *serveGate) {
	gated := func(next http.HandlerFunc) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			if _, err := g.check(r.Context(), r.RemoteAddr); err != nil {
				http.Error(rw, err.Error(), http.StatusForbidden)
				return
			}
			next(rw, r)
		}
	}
	h.HandleFunc("GET /{$}", gated(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Header().Set("Cache-Control", "no-store")
		_, _ = rw.Write(serveFrameHTML)
	}))
	h.HandleFunc("GET /v1/focus", gated(w.serveFocus))
	h.HandleFunc("GET /v1/comments/{rig}", gated(serveComments))
	h.HandleFunc("POST /v1/comments/{rig}", gated(sameOrigin(postComment)))
	h.HandleFunc("POST /v1/comments/{rig}/send", gated(sameOrigin(postSendComments)))
	h.HandleFunc("POST /v1/comments/{rig}/{id}/discard", gated(sameOrigin(postDiscardComment)))
	h.HandleFunc("POST /v1/show/{rig}", gated(sameOrigin(postShowRig)))
	proxy := w.rectoProxy()
	h.HandleFunc("GET /v1/surface/{rig}/{rest...}", gated(func(rw http.ResponseWriter, r *http.Request) {
		roots, group, label, m, err := surfaceRoots(r.PathValue("rig"))
		if err != nil {
			surfaceError(rw, http.StatusNotFound, err.Error())
			return
		}
		// The rig decides what the page shows; a caller can't widen it.
		q := r.URL.Query()
		q.Del("root")
		q.Del("group")
		q.Del("start")
		for _, root := range roots {
			q.Add("root", root)
			// Where each repo's work began, so a page whose branch point has
			// emptied out (work pushed straight to trunk) still shows it.
			if start := w.start(group, m, filepath.Base(root)); start != "" {
				q.Add("start", root+":"+start)
			}
		}
		// The basedir binds the repos together, so a tour written across
		// them (recto tour --group) lives there.
		q.Set("group", group)
		q.Set("label", label)
		// A chief of staff organizes attention across every rig, so its page
		// gathers what each one is asking.
		q.Del("peer")
		if m.isCoS() {
			for _, p := range rigPeers(group) {
				q.Add("peer", p)
			}
		}
		out := r.Clone(r.Context())
		out.URL = &url.URL{Path: "/" + r.PathValue("rest"), RawQuery: q.Encode()}
		proxy.ServeHTTP(rw, out)
	}))
}

func (w *serveWeb) serveFocus(rw http.ResponseWriter, r *http.Request) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-store")
	ch := make(chan webFocus, 4)
	w.mu.Lock()
	w.subs[ch] = struct{}{}
	cur := w.cur
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.subs, ch)
		w.mu.Unlock()
	}()
	send := func(f webFocus) bool {
		raw, _ := json.Marshal(f)
		if _, err := fmt.Fprintf(rw, "event: focus\ndata: %s\n\n", raw); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	fmt.Fprint(rw, "retry: 2000\n\n")
	if !send(cur) {
		return
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case f := <-ch:
			if !send(f) {
				return
			}
		case <-tick.C:
			if _, err := fmt.Fprint(rw, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// surfaceRoots is the set of workspace roots recto should show for a rig, one
// per repo in subdir order, the basedir that groups them, and the label the
// page should carry.
func surfaceRoots(slug string) (roots []string, group, label string, m manifest, err error) {
	rigs, err := listRigs()
	if err != nil {
		return nil, "", "", m, err
	}
	for _, r := range rigs {
		if r.Slug != slug {
			continue
		}
		m, err = readManifest(r.Path)
		if err != nil {
			return nil, "", "", m, err
		}
		dirs := make([]string, 0, len(m.Repos))
		for dir := range m.Repos {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		roots = make([]string, len(dirs))
		for i, d := range dirs {
			roots[i] = filepath.Join(r.Path, d)
		}
		label = r.ID
		if label == "" {
			label = r.Slug
		}
		return roots, r.Path, label, m, nil
	}
	return nil, "", "", m, fmt.Errorf("no rig %q on this host", slug)
}

// rigPeers is every other rig on this host whose asks a chief of staff's page
// gathers, as recto's `peer` values: slug, basedir, label. Other chiefs of
// staff are left out; they coordinate, they don't ask.
func rigPeers(self string) []string {
	rigs, err := listRigs()
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range rigs {
		if r.Path == self || r.Kind == "cos" || r.Building != "" {
			continue
		}
		// The tracker id and title when there is one, else the title alone;
		// a slug is only a last resort, since it's usually the title again.
		label := r.Title
		if r.ID != "" && r.ID != r.Slug {
			label = r.ID + " · " + r.Title
		}
		if strings.TrimSpace(strings.Trim(label, " ·")) == "" {
			label = r.Slug
		}
		out = append(out, r.Slug+"\t"+r.Path+"\t"+label)
	}
	return out
}

// postShowRig puts a rig's session on screen, for a link on a page that names
// another rig. The window follows the screen, so it lands there too. Only a
// session the board lists can be shown, the same as /v1/show.
func postShowRig(rw http.ResponseWriter, r *http.Request) {
	rig, err := rigBySlug(r.PathValue("rig"))
	if err != nil {
		jsonError(rw, http.StatusNotFound, err)
		return
	}
	s := rig.session()
	if err := show(showRequest{Kind: s.b.Name(), Target: s.name}); err != nil {
		if errors.Is(err, errNotListed) {
			err = fmt.Errorf("%s has no live session to show (wake it first)", rig.Slug)
		}
		jsonError(rw, http.StatusConflict, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]string{"shown": rig.Slug})
}

// rigBySlug finds a rig on this host by its basedir name.
func rigBySlug(slug string) (rigInfo, error) {
	rigs, err := listRigs()
	if err != nil {
		return rigInfo{}, err
	}
	for _, r := range rigs {
		if r.Slug == slug {
			return r, nil
		}
	}
	return rigInfo{}, fmt.Errorf("no rig %q on this host", slug)
}

// sameOrigin refuses a write a page on some other site sent from the
// person's own browser. The whois gate can't tell those apart, since they
// come from the same tailnet peer. Requiring JSON makes a browser ask first
// (and this server never says yes), and a stated Origin has to be this one.
func sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(ct) != "application/json" {
			http.Error(rw, "writes take application/json", http.StatusUnsupportedMediaType)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || u.Host != r.Host {
				http.Error(rw, "cross-origin write refused", http.StatusForbidden)
				return
			}
		}
		next(rw, r)
	}
}

func writeJSON(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(v)
}

func jsonError(rw http.ResponseWriter, code int, err error) {
	writeJSON(rw, code, map[string]string{"error": err.Error()})
}

func serveComments(rw http.ResponseWriter, r *http.Request) {
	rig, err := rigBySlug(r.PathValue("rig"))
	if err != nil {
		jsonError(rw, http.StatusNotFound, err)
		return
	}
	cs := readComments(rig.Path)
	if cs == nil {
		cs = []rigComment{}
	}
	writeJSON(rw, http.StatusOK, map[string]any{"comments": cs})
}

func postComment(rw http.ResponseWriter, r *http.Request) {
	rig, err := rigBySlug(r.PathValue("rig"))
	if err != nil {
		jsonError(rw, http.StatusNotFound, err)
		return
	}
	var in struct {
		Anchor commentAnchor `json:"anchor"`
		Quote  string        `json:"quote"`
		Body   string        `json:"body"`
		Client string        `json:"client"`
		Send   bool          `json:"send"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 64<<10)).Decode(&in); err != nil {
		jsonError(rw, http.StatusBadRequest, err)
		return
	}
	c, err := addComment(rig.Path, in.Anchor, in.Quote, in.Body, in.Client)
	if err != nil {
		jsonError(rw, http.StatusBadRequest, err)
		return
	}
	if in.Send {
		if _, err := sendComments(rig, []string{c.ID}); err != nil {
			// Kept and marked failed, so it can be sent again.
			jsonError(rw, http.StatusBadGateway, err)
			return
		}
	}
	writeJSON(rw, http.StatusOK, c)
}

func postSendComments(rw http.ResponseWriter, r *http.Request) {
	rig, err := rigBySlug(r.PathValue("rig"))
	if err != nil {
		jsonError(rw, http.StatusNotFound, err)
		return
	}
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 64<<10)).Decode(&in); err != nil {
		jsonError(rw, http.StatusBadRequest, err)
		return
	}
	sent, err := sendComments(rig, in.IDs)
	if err != nil {
		jsonError(rw, http.StatusBadGateway, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"sent": len(sent)})
}

func postDiscardComment(rw http.ResponseWriter, r *http.Request) {
	rig, err := rigBySlug(r.PathValue("rig"))
	if err != nil {
		jsonError(rw, http.StatusNotFound, err)
		return
	}
	if err := discardComment(rig.Path, r.PathValue("id")); err != nil {
		jsonError(rw, http.StatusBadRequest, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{})
}

// start is a rig repo's start, or "" when it can't be known.
func (w *serveWeb) start(basedir string, m manifest, subdir string) string {
	if c := m.Starts[subdir]; c != "" {
		return c
	}
	root := filepath.Join(basedir, subdir)
	w.startsMu.Lock()
	defer w.startsMu.Unlock()
	if c, ok := w.starts[root]; ok {
		return c
	}
	c, err := repoStart(basedir, m, subdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rig serve: no start for %s: %v\n", root, err)
	}
	w.starts[root] = c
	return c
}

func (w *serveWeb) rectoProxy() *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", w.rectoSock)
		},
		DisableCompression: true,
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "recto"
			pr.Out.Host = "recto"
		},
		Transport:      transport,
		FlushInterval:  -1, // the event stream has to arrive as it's written
		ModifyResponse: gzipHTML,
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, err error) {
			w.mu.Lock()
			why := w.rectoErr
			w.mu.Unlock()
			msg := fmt.Sprintf("recto serve isn't answering on %s: %v", w.rectoSock, err)
			if why != "" {
				msg += "\nlast exit: " + why
			}
			surfaceError(rw, http.StatusBadGateway, msg+"\n\nretrying…")
		},
	}
}

// gzipHTML compresses recto's pages on the way out. Syntax highlighting is
// inline styles on every token, which is bulky over the tailnet and shrinks
// by an order of magnitude.
func gzipHTML(resp *http.Response) error {
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(resp.Request.Header.Get("Accept-Encoding"), "gzip") {
		return nil
	}
	body := resp.Body
	pr, pw := io.Pipe()
	go func() {
		zw := gzip.NewWriter(pw)
		_, err := io.Copy(zw, body)
		if cerr := zw.Close(); err == nil {
			err = cerr
		}
		body.Close()
		pw.CloseWithError(err)
	}()
	resp.Body = pr
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	resp.Header.Set("Content-Encoding", "gzip")
	resp.Header.Add("Vary", "Accept-Encoding")
	return nil
}

// surfaceError is a page rather than a plain-text error because it lands in
// the frame's iframe, where a blank white box would read as "nothing here".
// A recto that's down is usually restarting, so that page retries itself;
// the frame keeps iframes, and one stuck on an old error would never recover.
func surfaceError(rw http.ResponseWriter, code int, msg string) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(code)
	retry := ""
	if code == http.StatusBadGateway {
		retry = `<meta http-equiv=refresh content=3>`
	}
	fmt.Fprintf(rw, `<!doctype html><meta charset=utf-8>%s<body style="background:#1e1e2e;color:#f38ba8;font:13px ui-monospace,Menlo,monospace;padding:20px;white-space:pre-wrap">%s</body>`, retry, html.EscapeString(msg))
}
