package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The live serve offers the hop only while an alternate is registered, alive
// and answering, and the alternate always offers the way back.
func TestSiblingOffersTheOtherServeOnlyWhileItIsUp(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	sibling := func(w *serveWeb) serveSibling {
		rec := httptest.NewRecorder()
		w.serveSibling(rec, httptest.NewRequest("GET", "/v1/sibling", nil))
		var s serveSibling
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	live := newServeWeb("")
	if got := sibling(live); got.Role != "live" || got.Port != "" {
		t.Fatalf("no alternate yet: got %+v", got)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	withdraw, err := registerServe(true, ln.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if got := sibling(live); got.Port != port {
		t.Fatalf("alternate up: got %+v, want port %s", got, port)
	}

	ln.Close()
	if got := sibling(live); got.Port != "" {
		t.Fatalf("an alternate that stopped answering isn't offered: got %+v", got)
	}

	withdraw()
	if _, ok := readAlt(mustRecordPath(t, true)); ok {
		t.Fatal("withdrawing removes the registration")
	}

	// The alternate points back at the live serve wherever it listens, not
	// at the default port.
	alt := newServeWeb("")
	alt.alt = true
	if got := sibling(alt); got.Role != "alt" || got.Port != "" {
		t.Fatalf("no live serve registered: got %+v", got)
	}
	liveLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer liveLn.Close()
	withdrawLive, err := registerServe(false, liveLn.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer withdrawLive()
	_, livePort, _ := net.SplitHostPort(liveLn.Addr().String())
	if got := sibling(alt); got.Port != livePort {
		t.Fatalf("the alternate points back at the live serve: got %+v, want %s", got, livePort)
	}
}

func mustRecordPath(t *testing.T, alt bool) string {
	t.Helper()
	p, err := serveRecordPath(alt)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The live serve shows the alternate's window under its own origin, so an
// installed app hops between them without leaving itself. The alternate's
// private socket answers only the window, whatever the proxy is asked for,
// and a write keeps its same-origin check against the page's own host.
func TestLiveServeProxiesTheAlternatesWindowUnderItsOrigin(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	live := webServer(t, newServeWeb(""))

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(live.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, _ := get("/alt/v1/sibling"); code != http.StatusBadGateway {
		t.Fatalf("no alternate yet: got %d", code)
	}

	alt := newServeWeb("")
	alt.alt = true
	h := http.NewServeMux()
	alt.mount(h, newServeGate([]string{"nobody"}, func(context.Context, string) (whoisIdentity, error) {
		return whoisIdentity{}, errors.New("the alternate's tailnet gate must not be reached through the proxy")
	}, false))
	h.HandleFunc("GET /v1/exec", func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusTeapot) })
	closeSock, sock, err := serveAltSocket(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSock()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	withdraw, err := registerServe(true, ln.Addr().String(), sock)
	if err != nil {
		t.Fatal(err)
	}
	defer withdraw()

	code, body := get("/alt/v1/sibling")
	if code != http.StatusOK || !strings.Contains(body, `"role":"alt"`) {
		t.Fatalf("the alternate answers under /alt/: %d %s", code, body)
	}
	if code, _ := get("/alt/v1/exec"); code != http.StatusNotFound {
		t.Fatalf("only the window's routes pass the private socket: got %d", code)
	}
	_, body = get("/v1/sibling")
	if !strings.Contains(body, `"path":"alt/"`) {
		t.Fatalf("the live window links to the alternate on its own origin: %s", body)
	}

	// A write from the page passes the alternate's same-origin check, since
	// the proxy keeps the page's Host.
	req, _ := http.NewRequest("POST", live.URL+"/alt/v1/comments/nope/send", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", live.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("a same-origin write through the proxy was refused")
	}
}

// A socket path has to fit in sun_path, which on macOS is 104 bytes. A deep
// runtime dir (a build sandbox's temp dir, say) won't, so the alternate's
// socket falls back somewhere short rather than failing to listen.
func TestAlternateSocketFitsUnderALongRuntimeDir(t *testing.T) {
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 60), strings.Repeat("e", 60))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", deep)
	closeSock, sock, err := serveAltSocket(context.Background(), http.NewServeMux())
	if err != nil {
		t.Fatalf("listening under a long runtime dir: %v", err)
	}
	if len(sock) >= maxSocketPath {
		t.Fatalf("socket path %q is %d bytes", sock, len(sock))
	}
	closeSock()
	if _, err := os.Stat(filepath.Dir(sock)); !os.IsNotExist(err) {
		t.Fatalf("closing removes the fallback dir: %v", err)
	}
}
