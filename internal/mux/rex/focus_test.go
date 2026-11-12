package rex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phinze/rig/internal/mux"
)

// The stream below is shaped like what `rex events --all --json` printed on
// a real server while the Mac app hopped between sessions: CLI clients come
// and go constantly, a session's blocks re-send sizes when it comes on
// screen, and a block split into a background session is sized by the app
// that owns it the moment it's created.
const focusStream = `{"type":"client_joined","payload":{"client":{"client_id":"client:cli","info":{"kind":"cli","name":"rex"}}}}
{"type":"session_event","payload":{"session_id":"session:1","event":{"type":"block_event","payload":{"block_id":"block:a","name":"size_changed","payload":{"owner":"client:cli"}}}}}
{"type":"session_event","payload":{"session_id":"session:1","event":{"type":"block_event","payload":{"block_id":"block:a","name":"size_changed","payload":{"owner":"client:mac"}}}}}
{"type":"session_event","payload":{"session_id":"session:1","event":{"type":"block_event","payload":{"block_id":"block:b","name":"size_changed","payload":{"owner":"client:mac"}}}}}
{"type":"session_event","payload":{"session_id":"session:2","event":{"type":"block_created","payload":{"block_id":"block:new"}}}}
{"type":"session_event","payload":{"session_id":"session:2","event":{"type":"block_event","payload":{"block_id":"block:new","name":"size_changed","payload":{"owner":"client:mac"}}}}}
{"type":"client_joined","payload":{"client":{"client_id":"client:phone","info":{"kind":"app","name":"Rex","label":"iPhone"}}}}
{"type":"session_event","payload":{"session_id":"session:2","event":{"type":"block_event","payload":{"block_id":"block:c","name":"size_changed","payload":{"owner":"client:phone"}}}}}
{"type":"session_event","payload":{"session_id":"session:9","event":{"type":"block_event","payload":{"block_id":"block:d","name":"size_changed","payload":{"owner":"client:mac"}}}}}
not json at all
{"type":"client_left","payload":{"client_id":"client:mac"}}
{"type":"session_event","payload":{"session_id":"session:1","event":{"type":"block_event","payload":{"block_id":"block:a","name":"size_changed","payload":{"owner":"client:mac"}}}}}
`

func fakeFocusRex(t *testing.T) { t.Helper(); fakeFocusRexWith(t, focusStream) }

func fakeFocusRexWith(t *testing.T, events string) {
	t.Helper()
	bin := t.TempDir()
	stream := filepath.Join(bin, "events.jsonl")
	if err := os.WriteFile(stream, []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$*" in
*"events --all --json"*) cat "` + stream + `"; exit 0 ;;
*client.list*) echo '{"clients":[{"client_id":"client:mac","info":{"kind":"app","name":"Rex Beta","label":"mac"}},{"client_id":"client:cli","info":{"kind":"cli"}}]}' ;;
*session.list*) echo '{"sessions":[{"session_id":"session:1","label":"alpha"},{"session_id":"session:2","label":"beta"}]}' ;;
*) echo '{}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "rex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

func TestWatchFocusFollowsAppSizing(t *testing.T) {
	fakeFocusRex(t)
	var got []string
	err := Backend{}.WatchFocus(context.Background(), func(f mux.Focus) {
		got = append(got, f.Client+"@"+f.Session)
	})
	if err == nil || !strings.Contains(err.Error(), "ended") {
		t.Fatalf("a stream that ends should say so, got %v", err)
	}
	// CLI sizing is ignored, a session's several blocks report once, a fresh
	// block's sizing doesn't count, each app is followed on its own, an
	// unknown session falls back to its id, and a client that left is no
	// longer followed.
	want := []string{"mac@alpha", "iPhone@beta", "mac@session:9"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("focus reports = %v, want %v", got, want)
	}
}

func TestWatchFocusNeverAutostarts(t *testing.T) {
	for _, cmd := range [][]string{{"events", "--all", "--json"}, {"api", "call", "client.list"}} {
		args := strings.Join(Backend{}.watchCommand(cmd...).Args, " ")
		if !strings.Contains(args, "--autostart=false") {
			t.Fatalf("local %q would start a server", args)
		}
	}
}

// A brand-new session's only block is brand new too, so switching to it has
// to count even though a fresh block's sizing otherwise doesn't.
func TestWatchFocusReportsANewSession(t *testing.T) {
	stream := `{"type":"session_created","payload":{"session":{"session_id":"session:2","label":"beta"}}}
{"type":"session_event","payload":{"session_id":"session:2","event":{"type":"block_created","payload":{"block_id":"block:only"}}}}
{"type":"session_event","payload":{"session_id":"session:2","event":{"type":"block_event","payload":{"block_id":"block:only","name":"size_changed","payload":{"owner":"client:mac"}}}}}
`
	fakeFocusRexWith(t, stream)
	var got []string
	_ = Backend{}.WatchFocus(context.Background(), func(f mux.Focus) { got = append(got, f.Session) })
	if strings.Join(got, " ") != "beta" {
		t.Fatalf("focus reports = %v, want [beta]", got)
	}
}
