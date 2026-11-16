package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// An alternate serve runs beside the live one on the same host, usually a
// dev build on another port: `rig serve --alt --listen ADDR:7745`. While one
// is up, each frame offers a one-tap hop to the other, so trying a build
// doesn't mean retyping a URL into a phone.
//
// The live serve shows the alternate's window under its own origin, at
// /alt/, because an installed web app only keeps pages from its own origin
// inside itself: a link to another port opens a browser instead. So the
// alternate also listens on a private unix socket, the live serve proxies
// /alt/ there, and the alternate trusts what arrives on that socket as
// already admitted. The socket answers only the window's own routes, never
// /v1/exec or the radar's.
//
// Each serve registers itself in rig's state directory by role, where the
// other looks for it, and the alternate keeps its own runtime sockets so the
// two never fight over a path.

// altRecord is what a serve leaves for its sibling to find. Sock is the
// alternate's private socket for the live serve's proxy.
type altRecord struct {
	Addr string `json:"addr"`
	PID  int    `json:"pid"`
	Sock string `json:"sock,omitempty"`
}

// trustedPeerKey marks a request that arrived on the alternate's private
// socket, carrying the identity the gate should take as admitted.
type trustedPeerKey struct{}

// altPrefix is where the live serve shows the alternate's window.
const altPrefix = "/alt"

func serveRecordPath(alt bool) (string, error) {
	dir, err := rigStateDir()
	if err != nil {
		return "", err
	}
	if alt {
		return filepath.Join(dir, "serve-alt.json"), nil
	}
	return filepath.Join(dir, "serve-live.json"), nil
}

// registerServe announces this process as the live or alternate serve at
// addr. The returned func withdraws it, unless another serve in the same role
// has since taken over.
func registerServe(alt bool, addr, sock string) (func(), error) {
	path, err := serveRecordPath(alt)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(altRecord{Addr: addr, PID: os.Getpid(), Sock: sock})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, raw, 0o644); err != nil {
		return nil, err
	}
	return func() {
		if rec, ok := readAlt(path); ok && rec.PID == os.Getpid() {
			_ = os.Remove(path)
		}
	}, nil
}

func readAlt(path string) (altRecord, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return altRecord{}, false
	}
	var rec altRecord
	if json.Unmarshal(raw, &rec) != nil || rec.Addr == "" || rec.PID <= 0 {
		return altRecord{}, false
	}
	return rec, true
}

// sibling is the serve registered in the given role, while its process is
// alive and it answers. A record left by a serve that was killed hard is
// simply ignored, so a pill never offers a dead server.
func sibling(alt bool) (altRecord, bool) {
	path, err := serveRecordPath(alt)
	if err != nil {
		return altRecord{}, false
	}
	rec, ok := readAlt(path)
	if !ok || syscall.Kill(rec.PID, 0) != nil {
		return altRecord{}, false
	}
	network, target := "tcp", rec.Addr
	if rec.Sock != "" {
		network, target = "unix", rec.Sock
	}
	conn, err := net.DialTimeout(network, target, 300*time.Millisecond)
	if err != nil {
		return altRecord{}, false
	}
	_ = conn.Close()
	return rec, true
}

// serveAltSocket listens on the alternate's private socket and answers the
// window's routes there for the live serve's proxy. The socket sits in
// rig's runtime directory and is the owner's alone.
func serveAltSocket(ctx context.Context, h http.Handler) (func(), string, error) {
	dir, err := rigRuntimeDir()
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "serve-alt-web.sock")
	// A socket path has to fit sun_path: 104 bytes on macOS, 108 on Linux.
	// A long runtime dir (a deep $HOME, a build sandbox) won't, so fall back
	// to a private directory under /tmp, which always does.
	var tmpDir string
	if len(path) >= maxSocketPath {
		tmpDir, err = os.MkdirTemp("/tmp", "rig-alt-")
		if err != nil {
			return nil, "", err
		}
		path = filepath.Join(tmpDir, "web.sock")
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
		return nil, "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, "", err
	}
	srv := &http.Server{
		Handler:           windowRoutesOnly(h),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return context.WithValue(ctx, trustedPeerKey{}, whoisIdentity{Node: "live serve"})
		},
	}
	go func() { _ = srv.Serve(ln) }()
	return func() {
		_ = srv.Close()
		_ = os.Remove(path)
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
	}, path, nil
}

// maxSocketPath is the longest unix socket path every platform rig runs on
// accepts, with room for the terminating NUL: macOS's sun_path is 104 bytes.
const maxSocketPath = 103

// windowRoutesOnly lets the proxied socket answer the web window and nothing
// else. Everything else a serve answers (the radar's document, /v1/exec)
// stays behind the tailnet gate on the TCP listener.
func windowRoutesOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		ok := p == "/" || p == "/v1/focus" || p == "/v1/sibling" || p == "/v1/rigs"
		for _, prefix := range []string{"/v1/comments/", "/v1/show/", "/v1/surface/"} {
			ok = ok || strings.HasPrefix(p, prefix)
		}
		if !ok {
			http.NotFound(rw, r)
			return
		}
		h.ServeHTTP(rw, r)
	})
}

// altProxy shows the alternate's window under /alt/ on the live serve's
// origin, for as long as an alternate is up.
func altProxy() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "alt"
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, altPrefix)
			pr.Out.URL.RawPath = ""
			// Keep the page's own Host, so the alternate's same-origin check
			// on writes compares against the origin the page came from.
			pr.Out.Host = pr.In.Host
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				rec, ok := sibling(true)
				if !ok || rec.Sock == "" {
					return nil, fmt.Errorf("no alternate serve is running")
				}
				var d net.Dialer
				return d.DialContext(ctx, "unix", rec.Sock)
			},
			DisableCompression: true,
		},
		FlushInterval: -1, // the focus and page streams arrive as they're written
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, err error) {
			surfaceError(rw, http.StatusBadGateway, "the dev window isn't running: "+err.Error())
		},
	}
	return proxy
}

// serveSibling tells a frame which serve it came from and where the other
// one is: a path on this origin when the live serve can proxy it, else a
// port the frame pairs with its own hostname.
type serveSibling struct {
	Role string `json:"role"`           // "live" or "alt"
	Port string `json:"port,omitempty"` // the other serve's port, when it's up
	Path string `json:"path,omitempty"` // the other serve's window on this origin
}

func (w *serveWeb) serveSibling(rw http.ResponseWriter, _ *http.Request) {
	var s serveSibling
	if w.alt {
		s.Role = "alt"
		if rec, ok := sibling(false); ok {
			s.Port = portOf(rec.Addr)
		}
	} else {
		s.Role = "live"
		if rec, ok := sibling(true); ok {
			s.Port = portOf(rec.Addr)
			if rec.Sock != "" {
				s.Path = strings.TrimPrefix(altPrefix, "/") + "/"
			}
		}
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(rw).Encode(s)
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}
