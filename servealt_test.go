package main

import (
	"encoding/json"
	"net"
	"net/http/httptest"
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
	withdraw, err := registerServe(true, ln.Addr().String())
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
	withdrawLive, err := registerServe(false, liveLn.Addr().String())
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
