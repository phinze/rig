package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// An alternate serve runs beside the live one on the same host, usually a
// dev build on another port: `rig serve --alt --listen ADDR:7745`. While one
// is up, each frame offers a one-tap hop to the other, so trying a build
// doesn't mean retyping a URL into a phone.
//
// Each serve registers itself in rig's state directory by role, where the
// other looks for it, and the alternate keeps its own runtime sockets so the
// two never fight over a path.

// altRecord is what a serve leaves for its sibling to find.
type altRecord struct {
	Addr string `json:"addr"`
	PID  int    `json:"pid"`
}

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
func registerServe(alt bool, addr string) (func(), error) {
	path, err := serveRecordPath(alt)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(altRecord{Addr: addr, PID: os.Getpid()})
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

// siblingPort is the port of the serve registered in the given role, while
// its process is alive and its port answers. A record left by a serve that
// was killed hard is simply ignored, so a pill never offers a dead server.
func siblingPort(alt bool) string {
	path, err := serveRecordPath(alt)
	if err != nil {
		return ""
	}
	rec, ok := readAlt(path)
	if !ok || syscall.Kill(rec.PID, 0) != nil {
		return ""
	}
	conn, err := net.DialTimeout("tcp", rec.Addr, 300*time.Millisecond)
	if err != nil {
		return ""
	}
	_ = conn.Close()
	_, port, err := net.SplitHostPort(rec.Addr)
	if err != nil {
		return ""
	}
	return port
}

// serveSibling tells a frame which serve it came from and where the other
// one is. The frame builds the link from its own hostname, since that's the
// name the person's device already reaches this host by.
type serveSibling struct {
	Role string `json:"role"`           // "live" or "alt"
	Port string `json:"port,omitempty"` // the other serve's port, when it's up
}

func (w *serveWeb) serveSibling(rw http.ResponseWriter, _ *http.Request) {
	s := serveSibling{Role: "live", Port: siblingPort(true)}
	if w.alt {
		s = serveSibling{Role: "alt", Port: siblingPort(false)}
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(rw).Encode(s)
}
