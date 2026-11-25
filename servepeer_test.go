package main

import (
	"bufio"
	"encoding/json"
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
)

// peerAt lists place as a served surface at endpoint, the way surfaces.toml
// names the Mac on foxtrotbase.
func peerAt(t *testing.T, place, endpoint string) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[surfaces]\n" + place + " = \"rig+" + endpoint + "\"\n"
	if err := os.WriteFile(filepath.Join(cfg, "rig", "surfaces.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// deadEndpoint is an address nothing listens on, so dialing it is refused at
// once rather than timing out.
func deadEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

type seenRequest struct {
	method, path, query, origin, body string
}

// farServe stands in for another host's serve and records what reached it.
func farServe(t *testing.T) (*httptest.Server, func() []seenRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, seenRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Origin"), string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenRequest(nil), seen...)
	}
}

func TestPeerRigRoutesGoToItsHost(t *testing.T) {
	webRig(t)
	far, seen := farServe(t)
	peerAt(t, "fx", far.URL)
	srv := webServer(t, newServeWeb(filepath.Join(t.TempDir(), "unused.sock")))

	resp, err := http.Get(srv.URL + "/v1/surface/robbie@fx/fragment?only=2")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("surface status %d", resp.StatusCode)
	}

	// A write that passed this host's origin check arrives without the
	// origin the far host would refuse as foreign.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/comments/robbie@fx/send", strings.NewReader(`{"ids":["a"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("send status %d", resp.StatusCode)
	}

	got := seen()
	if len(got) != 2 {
		t.Fatalf("far host saw %+v", got)
	}
	if got[0].path != "/v1/surface/robbie/fragment" || got[0].query != "only=2" {
		t.Errorf("surface reached the far host as %+v", got[0])
	}
	if got[1].method != "POST" || got[1].path != "/v1/comments/robbie/send" || got[1].origin != "" || got[1].body != `{"ids":["a"]}` {
		t.Errorf("send reached the far host as %+v", got[1])
	}
}

func TestPeerRouteRefusesAPlaceItDoesntKnow(t *testing.T) {
	webRig(t)
	isolateRigConfig(t)
	srv := webServer(t, newServeWeb(""))
	resp, err := http.Get(srv.URL + "/v1/comments/robbie@elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || !strings.Contains(string(body), "no surface") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

// A laptop that's asleep gets a page saying so, which retries itself, and
// the frame's own calls get the same news as JSON.
func TestPeerRouteSaysWhenTheHostIsGone(t *testing.T) {
	webRig(t)
	peerAt(t, "fx", deadEndpoint(t))
	srv := webServer(t, newServeWeb(""))

	for path, want := range map[string]string{
		"/v1/surface/robbie@fx/": "text/html",
		"/v1/comments/robbie@fx": "application/json",
	} {
		start := time.Now()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 502 || !strings.HasPrefix(resp.Header.Get("Content-Type"), want) || !strings.Contains(string(body), "asleep or off the tailnet") {
			t.Errorf("%s: %d %s %q", path, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("%s took %s; a gone host should answer fast", path, time.Since(start))
		}
	}
	html, _ := http.Get(srv.URL + "/v1/surface/robbie@fx/")
	body, _ := io.ReadAll(html.Body)
	html.Body.Close()
	if !strings.Contains(string(body), "http-equiv=refresh") {
		t.Errorf("the unreachable page should retry itself: %s", body)
	}
}

func focusReader(t *testing.T, url string) func() webFocus {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	ch := make(chan webFocus, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var f webFocus
				if json.Unmarshal([]byte(data), &f) == nil {
					ch <- f
				}
			}
		}
		close(ch)
	}()
	return func() webFocus {
		select {
		case f, ok := <-ch:
			if !ok {
				t.Fatal("focus stream ended")
			}
			return f
		case <-time.After(5 * time.Second):
			t.Fatal("no focus within 5s")
		}
		return webFocus{}
	}
}

// awaitFocus reads until a focus satisfies ok.
func awaitFocus(t *testing.T, next func() webFocus, what string, ok func(webFocus) bool) webFocus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f := next(); ok(f) {
			return f
		}
	}
	t.Fatalf("never saw %s", what)
	return webFocus{}
}

func TestFrameFollowsWhicheverHostSwitchedLast(t *testing.T) {
	webRig(t)
	old := peerLinger
	peerLinger = 0
	t.Cleanup(func() { peerLinger = old })

	far := newServeWeb("")
	farSrv := httptest.NewUnstartedServer(nil)
	farMux := http.NewServeMux()
	far.mount(farMux, newServeGate([]string{"nobody"}, nil, true))
	farSrv.Config.Handler = farMux
	farSrv.Start()
	t.Cleanup(farSrv.Close)
	peerAt(t, "fx", farSrv.URL)

	here := newServeWeb("")
	srv := webServer(t, here)
	now := time.Now().UnixMilli()
	here.publish(webFocus{Rig: "alpha", ID: "ALPHA-1", Session: "~/workspaces/alpha", Since: now - 1000})
	far.publish(webFocus{Rig: "robbie", ID: "robbie", Session: "~/workspaces/robbie", Since: now})

	next := focusReader(t, srv.URL+"/v1/focus")
	got := awaitFocus(t, next, "the far rig", func(f webFocus) bool { return f.Rig == "robbie@fx" })
	if got.ID != "robbie@fx" || got.Error != "" {
		t.Fatalf("far focus came through as %+v", got)
	}

	// A bare session on the far host doesn't take the screen from a rig.
	far.publish(webFocus{Session: "scratch", Since: now + 500})
	here.publish(webFocus{Rig: "alpha", ID: "ALPHA-1", Session: "~/workspaces/alpha", Since: now + 1000})
	awaitFocus(t, next, "the local rig again", func(f webFocus) bool { return f.Rig == "alpha" })

	// Another serve asking for this host's own focus never hears the far
	// host's, or two serves would hand each other's back forever.
	far.publish(webFocus{Rig: "robbie", ID: "robbie", Since: now + 2000})
	awaitFocus(t, next, "the far rig, newest", func(f webFocus) bool { return f.Rig == "robbie@fx" })
	if f := focusReader(t, srv.URL+"/v1/focus?local=1")(); f.Rig != "alpha" {
		t.Fatalf("local focus = %+v, want this host's own", f)
	}

	// The laptop goes away while it holds the screen: the frame says so.
	farSrv.CloseClientConnections()
	farSrv.Close()
	awaitFocus(t, next, "an error naming fx", func(f webFocus) bool {
		return f.Rig == "robbie@fx" && strings.Contains(f.Error, "fx")
	})
}

func TestSwitcherListsPeerRigsByRecency(t *testing.T) {
	webRig(t)
	far := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rigs" || r.URL.Query().Get("local") != "1" {
			http.NotFound(w, r)
			return
		}
		future := time.Now().Add(time.Hour).UnixMilli()
		json.NewEncoder(w).Encode(map[string]any{"rigs": []switchRow{
			{Slug: "robbie", ID: "robbie", Title: "Robbie", State: "idle", Touched: future},
			{Slug: "old", ID: "old", Title: "Old"},
		}})
	}))
	t.Cleanup(far.Close)
	peerAt(t, "fx", far.URL)
	srv := webServer(t, newServeWeb(""))

	var doc struct {
		Rigs []switchRow `json:"rigs"`
		Down []string    `json:"down"`
	}
	resp, err := http.Get(srv.URL + "/v1/rigs")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	var slugs []string
	for _, r := range doc.Rigs {
		slugs = append(slugs, r.Slug)
	}
	if strings.Join(slugs, ",") != "robbie@fx,alpha,old@fx" {
		t.Fatalf("switcher rows = %v", slugs)
	}
	if doc.Rigs[0].ID != "robbie@fx" || len(doc.Down) != 0 {
		t.Fatalf("doc = %+v", doc)
	}

	// Asked for its own rigs, a serve doesn't ask anyone else.
	resp, _ = http.Get(srv.URL + "/v1/rigs?local=1")
	doc.Rigs = nil
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if len(doc.Rigs) != 1 || doc.Rigs[0].Slug != "alpha" {
		t.Fatalf("local rows = %+v", doc.Rigs)
	}

	// A host that can't answer is named, not silently missing.
	peerAt(t, "fx", deadEndpoint(t))
	resp, _ = http.Get(srv.URL + "/v1/rigs")
	doc.Rigs, doc.Down = nil, nil
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if len(doc.Down) != 1 || doc.Down[0] != "fx" {
		t.Fatalf("down = %v", doc.Down)
	}
}
