package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phinze/rig/internal/mux"
)

// webRig makes ~/workspaces/alpha under a fake HOME holding two repos, and
// returns its basedir.
func webRig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	basedir := filepath.Join(home, "workspaces", "alpha")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := manifest{
		ID: "ALPHA-1", Title: "Alpha things",
		Repos:  map[string]string{"two": "o/two", "one": "o/one"},
		Starts: map[string]string{"one": "abc123"},
	}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}
	return basedir
}

// fakeRecto answers on a unix socket the way recto serve does and records
// what it was asked for.
func fakeRecto(t *testing.T) (sock string, asked func() *http.Request) {
	t.Helper()
	// Unix socket paths are short-limited, and t.TempDir can be long.
	dir, err := os.MkdirTemp("", "rw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var last *http.Request
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<p>"+strings.Repeat("rendered ", 50)+"</p>")
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock, func() *http.Request {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func webServer(t *testing.T, w *serveWeb) *httptest.Server {
	t.Helper()
	h := http.NewServeMux()
	w.mount(h, newServeGate([]string{"nobody"}, nil, true))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestSurfaceProxiesTheRigsReposToRecto(t *testing.T) {
	basedir := webRig(t)
	sock, asked := fakeRecto(t)
	srv := webServer(t, newServeWeb(sock))

	// A caller's own root, group, and start are dropped: the rig decides the
	// page.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/surface/alpha/fragment?keep=1&root=/etc&group=/&start=/etc:feed", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("html should arrive gzipped, headers %v", resp.Header)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if !strings.Contains(string(body), "rendered") {
		t.Fatalf("body %q", body)
	}

	got := asked()
	if got.URL.Path != "/fragment" {
		t.Fatalf("recto asked for %q", got.URL.Path)
	}
	q := got.URL.Query()
	want := []string{filepath.Join(basedir, "one"), filepath.Join(basedir, "two")}
	if strings.Join(q["root"], ",") != strings.Join(want, ",") {
		t.Fatalf("roots = %v, want %v in subdir order", q["root"], want)
	}
	if q.Get("group") != basedir {
		t.Fatalf("group = %q, want the basedir %q", q.Get("group"), basedir)
	}
	// "two" isn't a real workspace, so its start can't be recovered and the
	// page falls back to its branch point.
	if got := strings.Join(q["start"], ","); got != filepath.Join(basedir, "one")+":abc123" {
		t.Fatalf("starts = %q", got)
	}
	if q.Get("label") != "ALPHA-1" || q.Get("keep") != "1" {
		t.Fatalf("query = %v", q)
	}
}

func TestSurfaceSaysSoWhenItCantAnswer(t *testing.T) {
	webRig(t)
	srv := webServer(t, newServeWeb(filepath.Join(t.TempDir(), "nobody-home.sock")))

	for path, want := range map[string]struct {
		code int
		text string
	}{
		"/v1/surface/nope/":  {404, "no rig"},
		"/v1/surface/alpha/": {502, "isn&#39;t answering"},
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want.code || !strings.Contains(string(body), want.text) {
			t.Errorf("%s: %d %q, want %d containing %q", path, resp.StatusCode, body, want.code, want.text)
		}
	}
}

func TestFocusStreamNamesTheRigOnScreen(t *testing.T) {
	basedir := webRig(t)
	w := newServeWeb("")
	srv := webServer(t, w)

	resp, err := http.Get(srv.URL + "/v1/focus")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := bufio.NewScanner(resp.Body)
	next := func() string {
		for lines.Scan() {
			if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok {
				return data
			}
		}
		t.Fatal("stream ended")
		return ""
	}
	if first := next(); first != "{}" {
		t.Fatalf("before any focus the stream should say nothing, got %s", first)
	}

	w.publish(resolveFocus(mux.Focus{Session: rigSessionName(basedir), Client: "mac"}))
	got := next()
	for _, want := range []string{`"rig":"alpha"`, `"id":"ALPHA-1"`, `"client":"mac"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("focus %s lacks %s", got, want)
		}
	}

	w.publish(resolveFocus(mux.Focus{Session: "scratch", Client: "mac"}))
	if got := next(); strings.Contains(got, `"rig"`) || !strings.Contains(got, `"session":"scratch"`) {
		t.Fatalf("a non-rig session should come through bare, got %s", got)
	}

	// The last focus survives a restart.
	again := newServeWeb("")
	again.restoreFocus()
	if again.cur.Session != "scratch" {
		t.Fatalf("restored focus = %+v", again.cur)
	}
}

func TestSlowFocusReaderEndsOnTheNewest(t *testing.T) {
	w := newServeWeb("")
	ch := make(chan webFocus, 4)
	w.subs[ch] = struct{}{}
	for _, s := range []string{"a", "b", "c", "d", "e", "f"} {
		w.publish(webFocus{Session: s})
	}
	var last string
	for len(ch) > 0 {
		last = (<-ch).Session
	}
	if last != "f" {
		t.Fatalf("a reader that fell behind ended on %q, want the newest", last)
	}
}

func TestNoFocusSourceSaysSo(t *testing.T) {
	w := newServeWeb("")
	w.follow(t.Context(), nil)
	if !strings.Contains(w.cur.Error, "no multiplexer") {
		t.Fatalf("with nothing to watch, focus should say so; got %+v", w.cur)
	}
}

// A chief of staff's page gathers every other working rig's asks; any other
// rig's page gathers none, and a caller can't add peers of its own.
func TestCoSPageGathersTheOtherRigs(t *testing.T) {
	alpha := webRig(t)
	home := filepath.Dir(filepath.Dir(alpha))
	mk := func(slug string, m manifest) string {
		dir := filepath.Join(home, "workspaces", slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeManifest(dir, m); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	mk("cos-today", manifest{Kind: "cos", Title: "Chief of staff"})
	mk("cos-yesterday", manifest{Kind: "cos", Title: "Chief of staff"})
	untracked := mk("loose-idea", manifest{Title: "Loose idea"})
	sock, asked := fakeRecto(t)
	srv := webServer(t, newServeWeb(sock))

	get := func(slug string) []string {
		resp, err := http.Get(srv.URL + "/v1/surface/" + slug + "/fragment?peer=x%09%2Fetc%09evil")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return asked().URL.Query()["peer"]
	}
	got := get("cos-today")
	want := []string{
		"alpha\t" + alpha + "\tALPHA-1 · Alpha things",
		"loose-idea\t" + untracked + "\tLoose idea",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("peers = %q, want %q", got, want)
	}
	if got := get("alpha"); len(got) != 0 {
		t.Fatalf("a working rig's page got peers %q", got)
	}
}

// A rig link puts that rig's session on screen through the same guarded
// path as the board's, and says so when the rig has nothing to show.
func TestShowRigPutsItsSessionOnScreen(t *testing.T) {
	basedir := webRig(t)
	srv := webServer(t, newServeWeb(""))
	var got showRequest
	old := show
	t.Cleanup(func() { show = old })
	show = func(req showRequest) error { got = req; return nil }

	post := func(slug, origin string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/show/"+slug, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("alpha", "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("a cross-origin show got %d", code)
	}
	if code := post("alpha", srv.URL); code != http.StatusOK {
		t.Fatalf("show got %d", code)
	}
	if got.Target != rigSessionName(basedir) || got.Kind != localTmux().Name() {
		t.Fatalf("showed %+v", got)
	}
	if code := post("nope", srv.URL); code != http.StatusNotFound {
		t.Fatalf("an unknown rig got %d", code)
	}
	show = func(showRequest) error { return errNotListed }
	if code := post("alpha", srv.URL); code != http.StatusConflict {
		t.Fatalf("a rig with no session got %d", code)
	}
}

// A sync that switches the profile to a new recto repoints the PATH entry at
// a new store path. The running recto is stopped and started again from the
// new binary, without waiting for rig serve itself to restart.
func TestRunRectoRestartsOntoAReplacedBinary(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "starts.log")
	for _, v := range []string{"v1", "v2"} {
		script := "#!/bin/sh\necho " + v + " >> " + log + "\nexec sleep 30\n"
		if err := os.WriteFile(filepath.Join(dir, v), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "recto")
	if err := os.Symlink(filepath.Join(dir, "v1"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := rectoCheckEvery
	rectoCheckEvery = 50 * time.Millisecond
	t.Cleanup(func() { rectoCheckEvery = old })

	w := newServeWeb(filepath.Join(dir, "run", "recto.sock"))
	ctx, cancel := context.WithCancel(context.Background())
	go w.runRecto(ctx)
	t.Cleanup(func() { cancel(); <-w.rectoDone })

	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if raw, _ := os.ReadFile(log); strings.TrimSpace(string(raw)) == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		raw, _ := os.ReadFile(log)
		t.Fatalf("starts: %q, want %q", raw, want)
	}
	waitFor("v1")

	tmp := filepath.Join(bin, "recto.new")
	if err := os.Symlink(filepath.Join(dir, "v2"), tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Fatal(err)
	}
	waitFor("v1\nv2")
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rectoErr != "" {
		t.Fatalf("a replaced binary isn't a failure: %q", w.rectoErr)
	}
}
