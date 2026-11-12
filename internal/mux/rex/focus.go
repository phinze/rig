package rex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/phinze/rig/internal/mux"
)

// Rex doesn't say which session an app is showing: an app attaches to every
// session in its sidebar, and every session keeps its own active window and
// focused block. What it does do is re-send the visible blocks' sizes
// whenever it puts a session on screen, so a size_changed event whose owner
// is an app client marks the session that app just showed. Background
// sessions get none. That's a side effect of terminal sizing rather than a
// promise, so it lives here alone; when Rex grows a real focus event, this
// file is what changes.

// freshBlock is how long after a block's creation its own sizing is ignored.
// A block an agent splits into a background session is sized by the app that
// owns it, which would otherwise read as that session coming on screen. A
// session that is itself that new is exempt: its first block is all it has,
// and an app switching to it is exactly what focus should report.
const freshBlock = 2 * time.Second

type focusEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type clientInfo struct {
	ClientID string `json:"client_id"`
	Info     struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		Label string `json:"label"`
	} `json:"info"`
}

func (c clientInfo) label() string {
	if c.Info.Label != "" {
		return c.Info.Label
	}
	return c.Info.Name
}

// WatchFocus follows `rex events --all` and reports each session an app
// client puts on screen. Consecutive reports of the same session by the same
// app are folded into one.
func (b Backend) WatchFocus(ctx context.Context, seen func(mux.Focus)) error {
	apps := map[string]string{} // client id -> label, app clients only
	var list struct {
		Clients []clientInfo `json:"clients"`
	}
	raw, err := b.run(b.watchCommand("api", "call", "client.list"))
	if err != nil {
		return fmt.Errorf("rex client.list: %w", err)
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("rex client.list: %w", err)
	}
	for _, c := range list.Clients {
		if c.Info.Kind == "app" {
			apps[c.ClientID] = c.label()
		}
	}

	var namesMu sync.Mutex
	names := map[string]string{}
	nameOf := func(id string) string {
		namesMu.Lock()
		defer namesMu.Unlock()
		if n, ok := names[id]; ok {
			return n
		}
		if sessions, err := b.listSessions(); err == nil {
			for _, s := range sessions {
				names[s.ID] = s.Name
			}
		}
		return names[id]
	}

	// Cancelling before Wait matters on an early return (a scanner error): with
	// nothing reading its pipe, rex events would otherwise never exit.
	ctx, cancel := context.WithCancel(ctx)
	cmd := b.watchCommand("events", "--all", "--json")
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	born := map[string]time.Time{}        // block id -> created
	newSessions := map[string]time.Time{} // session id -> created
	last := map[string]string{}           // app client -> session last reported
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev focusEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "client_joined":
			var p struct {
				Client clientInfo `json:"client"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil && p.Client.Info.Kind == "app" {
				apps[p.Client.ClientID] = p.Client.label()
			}
		case "client_left":
			var p struct {
				ClientID string `json:"client_id"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil {
				delete(apps, p.ClientID)
				delete(last, p.ClientID)
			}
		case "session_created", "session_removed":
			var p struct {
				Session struct {
					ID string `json:"session_id"`
				} `json:"session"`
			}
			if ev.Type == "session_created" && json.Unmarshal(ev.Payload, &p) == nil && p.Session.ID != "" {
				newSessions[p.Session.ID] = time.Now()
			}
			namesMu.Lock()
			names = map[string]string{}
			namesMu.Unlock()
		case "session_event":
			var p struct {
				SessionID string `json:"session_id"`
				Event     struct {
					Type    string `json:"type"`
					Payload struct {
						BlockID string `json:"block_id"`
						Name    string `json:"name"`
						Payload struct {
							Owner string `json:"owner"`
						} `json:"payload"`
					} `json:"payload"`
				} `json:"event"`
			}
			if json.Unmarshal(ev.Payload, &p) != nil {
				continue
			}
			e := p.Event
			if e.Type == "block_created" {
				now := time.Now()
				// Entries only matter for freshBlock; drop the old ones
				// here so blocks that are never sized don't pile up.
				for id, t := range born {
					if now.Sub(t) > freshBlock {
						delete(born, id)
					}
				}
				for id, t := range newSessions {
					if now.Sub(t) > freshBlock {
						delete(newSessions, id)
					}
				}
				born[e.Payload.BlockID] = now
				continue
			}
			if e.Type != "block_event" || e.Payload.Name != "size_changed" {
				continue
			}
			label, isApp := apps[e.Payload.Payload.Owner]
			if !isApp {
				continue
			}
			if t, ok := born[e.Payload.BlockID]; ok {
				_, sessionIsNew := newSessions[p.SessionID]
				if time.Since(t) < freshBlock && !sessionIsNew {
					continue
				}
				delete(born, e.Payload.BlockID)
			}
			if last[e.Payload.Payload.Owner] == p.SessionID {
				continue
			}
			last[e.Payload.Payload.Owner] = p.SessionID
			name := nameOf(p.SessionID)
			if name == "" {
				name = p.SessionID
			}
			seen(mux.Focus{Session: name, Client: label})
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("rex events ended")
}

// watchCommand never autostarts: watching a server that isn't there should
// fail loudly and retry, not conjure an empty one to watch.
func (b Backend) watchCommand(args ...string) *exec.Cmd {
	if !b.remote() {
		args = append([]string{"--autostart=false"}, args...)
	}
	return b.command(args...)
}
