package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeDocker puts a docker on PATH that serves `container ls` and `network ls`
// from state files of "id<TAB>project-dir" lines and removes exactly the ids it
// is asked to. A network refuses removal while any container is left, the way
// docker refuses a network with active endpoints. Filters are ignored: the
// state stands in for what docker's label filter already returned.
func fakeDocker(t *testing.T, containers, networks []string) (state string) {
	t.Helper()
	bin, state := t.TempDir(), t.TempDir()
	write := func(name string, lines []string) {
		body := strings.Join(lines, "\n")
		if body != "" {
			body += "\n"
		}
		if err := os.WriteFile(filepath.Join(state, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("containers", containers)
	write("networks", networks)
	script := `#!/bin/sh
S="` + state + `"
drop() { f="$S/$1"; shift; for id in "$@"; do grep -v "^$id	" "$f" > "$f.tmp"; mv "$f.tmp" "$f"; done; }
case "$1 $2" in
  "container ls") cat "$S/containers" ;;
  "network ls") cat "$S/networks" ;;
  "rm -f") shift 2; drop containers "$@" ;;
  "network rm")
    shift 2
    if [ -s "$S/stuck" ] || [ -s "$S/containers" ]; then echo "error: network has active endpoints" >&2; exit 1; fi
    drop networks "$@" ;;
  *) echo "fake docker: unexpected $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Prepended, not replacing: the script itself needs cat, grep, and mv.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return state
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(strings.ReplaceAll(string(b), "\t", " "))
}

// The mir-1979 shape: iso stop has run, the services are gone, and the peers
// network is still there. Teardown finishes it. A resource another workspace
// made under the same session name is not this rig's and stays.
func TestRemoveISOLeftoversFinishesWhatStopLeft(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "mir-1979", "runtime")
	other := filepath.Join(root, "elsewhere", "runtime")
	state := fakeDocker(t,
		[]string{"c1\t" + ws},
		[]string{"n1\t" + ws, "n2\t" + other},
	)

	if err := removeISOLeftovers(isoCleanup{Workspace: ws, Session: "dev-mir-1979-runtime"}); err != nil {
		t.Fatalf("leftovers should be removed: %v", err)
	}
	if got := readLines(t, filepath.Join(state, "containers")); len(got) != 0 {
		t.Errorf("containers left: %v", got)
	}
	if got, want := readLines(t, filepath.Join(state, "networks")), []string{"n2", other}; !reflect.DeepEqual(got, want) {
		t.Errorf("networks = %v, want only the other workspace's %v", got, want)
	}
}

// A network that won't go away keeps the teardown job on disk for reap to
// retry, and the error names what's left.
func TestRemoveISOLeftoversFailsClosed(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "runtime")
	state := fakeDocker(t, nil, []string{"n1\t" + ws})
	if err := os.WriteFile(filepath.Join(state, "stuck"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := removeISOLeftovers(isoCleanup{Workspace: ws, Session: "dev-x-runtime"})
	if err == nil {
		t.Fatal("a surviving network should fail teardown")
	}
	if !strings.Contains(err.Error(), "active endpoints") {
		t.Errorf("error %q should carry docker's reason", err)
	}
}

func TestRemoveISOLeftoversNothingToDo(t *testing.T) {
	fakeDocker(t, nil, nil)
	if err := removeISOLeftovers(isoCleanup{Workspace: t.TempDir(), Session: "dev-x-runtime"}); err != nil {
		t.Fatalf("a clean stop should be a no-op: %v", err)
	}
}

func TestParseISOPeersAndPeersUnder(t *testing.T) {
	root := t.TempDir()
	rig := filepath.Join(root, "mir-1858")
	out := strings.Join([]string{
		filepath.Join(rig, "runtime") + "\trunner1\texited",
		filepath.Join(rig, "runtime") + "\tcoordinator\trunning",
		filepath.Join(root, "other", "runtime") + "\tcoordinator\trunning",
		"\torphan\trunning", // no project dir: not attributable
		"garbage",
	}, "\n")

	all := parseISOPeers(out)
	got := all[resolvePath(filepath.Join(rig, "runtime"))]
	want := []isoPeer{{"coordinator", "running"}, {"runner1", "exited"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("peers = %v, want %v", got, want)
	}
	if len(all) != 2 {
		t.Errorf("unattributable lines should be dropped: %v", all)
	}

	under := peersUnder(all, rig)
	if len(under) != 2 || runningPeers(under) != 1 {
		t.Errorf("peersUnder(%s) = %v, want the rig's two peers with one running", rig, under)
	}
	if got := peersUnder(all, filepath.Join(root, "nope")); got != nil {
		t.Errorf("a rig with no peers should get none: %v", got)
	}
}
