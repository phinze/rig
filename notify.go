package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/sys/unix"
)

// Rig's whole data model is rig-scoped, but the tools that most want to tell you
// something — an hourly nix-config-sync, a cron, a watcher — aren't rigs and
// never will be. The inbox is the seam: one ambient list any tool can post to,
// surfaced in the boards you already read (ls, sweep, radar) so a stalled
// background job can't go two days without anyone noticing. An entry may name a
// rig, which pins it to that rig's row; most won't, and those render as a banner
// above the table.
//
// Identity is (source, key), not a generated id, because the archetypal poster
// is a cron saying the same thing every hour. Re-posting the same key updates
// one entry and bumps Count — "stalled, 37 runs" is the useful message, and a
// hundred identical rows is not.
type notification struct {
	Source  string    `json:"source"`
	Key     string    `json:"key"`
	Level   string    `json:"level"` // info | warn | error
	Title   string    `json:"title"`
	Body    string    `json:"body,omitempty"`
	Rig     string    `json:"rig,omitempty"` // optional rig id this belongs to
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Count   int       `json:"count"`
}

// ref is the "source/key" handle dismiss accepts.
func (n notification) ref() string { return n.Source + "/" + n.Key }

// notifyMax bounds the file so a misbehaving poster that varies its key can't
// grow it without limit. Eviction is oldest-Updated first, which keeps whatever
// is currently recurring and drops what has gone quiet.
const notifyMax = 200

var notifyLevels = map[string]int{"info": 0, "warn": 1, "error": 2}

func notifyPath() (string, error) {
	dir, err := rigStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "notifications.json"), nil
}

// withNotifications runs fn against the stored list under an exclusive flock and
// writes back whatever it returns. Posts arrive from cron and from terminals at
// the same time, so the read-modify-write has to be atomic or an hourly job can
// silently clobber a dismissal.
func withNotifications(fn func([]notification) ([]notification, error)) error {
	path, err := notifyPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}

	list, err := readNotifications(path)
	if err != nil {
		return err
	}
	next, err := fn(list)
	if err != nil {
		return err
	}
	if next == nil {
		return nil // fn declined to change anything
	}
	return writeNotifications(path, next)
}

func readNotifications(path string) ([]notification, error) {
	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []notification
	if err := json.Unmarshal(blob, &list); err != nil {
		// A corrupt inbox is not worth failing a post over — the inbox is
		// advisory, and refusing to record anything until someone hand-repairs
		// a JSON file is the wrong trade for a notification system.
		return nil, nil
	}
	return list, nil
}

func writeNotifications(path string, list []notification) error {
	sort.Slice(list, func(i, j int) bool { return list[i].Updated.After(list[j].Updated) })
	if len(list) > notifyMax {
		list = list[:notifyMax]
	}
	blob, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(blob, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// activeNotifications is the read path the boards use. It never returns an error
// worth surfacing: a board that can't read the inbox should render the rigs it
// does know about, not refuse to draw.
func activeNotifications() []notification {
	path, err := notifyPath()
	if err != nil {
		return nil
	}
	list, err := readNotifications(path)
	if err != nil {
		return nil
	}
	sort.Slice(list, func(i, j int) bool {
		if notifyLevels[list[i].Level] != notifyLevels[list[j].Level] {
			return notifyLevels[list[i].Level] > notifyLevels[list[j].Level]
		}
		return list[i].Updated.After(list[j].Updated)
	})
	return list
}

// notificationsForRig splits the inbox into the entries pinned to a given rig id
// and the loose ones. Boards render the first on that rig's row and the second
// as a banner.
func notificationsForRig(list []notification, id string) []notification {
	var out []notification
	for _, n := range list {
		if n.Rig != "" && n.Rig == id {
			out = append(out, n)
		}
	}
	return out
}

func looseNotifications(list []notification) []notification {
	var out []notification
	for _, n := range list {
		if n.Rig == "" {
			out = append(out, n)
		}
	}
	return out
}

func notifyLevelMark(level string) string {
	switch level {
	case "error":
		return "✗"
	case "warn":
		return "!"
	default:
		return "·"
	}
}

// notifyBanner is the one-line-per-entry block the boards print above their
// table. Count is only shown once something has actually repeated, so a
// first-time notice reads as a sentence rather than a metric.
func notifyBanner(list []notification) []string {
	var lines []string
	for _, n := range list {
		line := fmt.Sprintf("%s %s: %s", notifyLevelMark(n.Level), n.Source, n.Title)
		if n.Count > 1 {
			line += fmt.Sprintf(" (%d runs, %s)", n.Count, age(n.Created))
		}
		lines = append(lines, line)
	}
	return lines
}

func runNotify(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: rig notify post|list|dismiss [...]")
	}
	switch args[0] {
	case "post":
		return runNotifyPost(args[1:])
	case "list":
		return runNotifyList(args[1:])
	case "dismiss":
		return runNotifyDismiss(args[1:])
	default:
		return fmt.Errorf("rig notify: unknown subcommand %q", args[0])
	}
}

func runNotifyPost(args []string) error {
	n := notification{Level: "info"}
	for i := 0; i < len(args); i++ {
		key, val, inline := strings.Cut(args[i], "=")
		if !inline {
			if i+1 >= len(args) {
				return fmt.Errorf("rig notify post: %s needs a value", key)
			}
			i++
			val = args[i]
		}
		switch key {
		case "--source":
			n.Source = val
		case "--key":
			n.Key = val
		case "--level":
			n.Level = val
		case "--title":
			n.Title = val
		case "--body":
			n.Body = val
		case "--rig":
			n.Rig = val
		default:
			return fmt.Errorf("rig notify post: unknown flag %q", key)
		}
	}
	if n.Source == "" || n.Key == "" || n.Title == "" {
		return fmt.Errorf("rig notify post: --source, --key and --title are required")
	}
	if _, ok := notifyLevels[n.Level]; !ok {
		return fmt.Errorf("rig notify post: level %q is not info, warn or error", n.Level)
	}

	now := time.Now()
	return withNotifications(func(list []notification) ([]notification, error) {
		for i, existing := range list {
			if existing.Source != n.Source || existing.Key != n.Key {
				continue
			}
			// Same story, still happening: keep when it started and how many
			// times, refresh everything that describes the current state.
			n.Created = existing.Created
			n.Count = existing.Count + 1
			n.Updated = now
			list[i] = n
			return list, nil
		}
		n.Created, n.Updated, n.Count = now, now, 1
		return append(list, n), nil
	})
}

func runNotifyList(args []string) error {
	jsonOut := false
	for _, a := range args {
		switch a {
		case "--format=json":
			jsonOut = true
		case "--format=table":
			jsonOut = false
		default:
			return fmt.Errorf("usage: rig notify list [--format=json|table]")
		}
	}
	list := activeNotifications()
	if jsonOut {
		blob, err := json.MarshalIndent(list, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(blob))
		return nil
	}
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "rig: inbox is empty")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, n := range list {
		count := ""
		if n.Count > 1 {
			count = fmt.Sprintf("x%d", n.Count)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			notifyLevelMark(n.Level), n.ref(), age(n.Updated), count, n.Title)
		if n.Body != "" {
			fmt.Fprintf(w, "\t\t\t\t%s\n", n.Body)
		}
	}
	return w.Flush()
}

func runNotifyDismiss(args []string) error {
	all := false
	var refs []string
	for _, a := range args {
		if a == "--all" {
			all = true
			continue
		}
		refs = append(refs, a)
	}
	if !all && len(refs) == 0 {
		return fmt.Errorf("usage: rig notify dismiss <source/key>... | --all")
	}
	return withNotifications(func(list []notification) ([]notification, error) {
		if all {
			return []notification{}, nil
		}
		for _, ref := range refs {
			match := -1
			for i, n := range list {
				// A bare key is accepted when it's unambiguous; typing the
				// source every time is friction for the common one-source case.
				if n.ref() != ref && n.Key != ref {
					continue
				}
				if match >= 0 && n.ref() != ref {
					return nil, fmt.Errorf("rig notify dismiss: %q is ambiguous, use source/key", ref)
				}
				if n.ref() == ref {
					match = i
					break
				}
				match = i
			}
			if match < 0 {
				return nil, fmt.Errorf("rig notify dismiss: no notification %q", ref)
			}
			list = append(list[:match], list[match+1:]...)
		}
		return list, nil
	})
}
