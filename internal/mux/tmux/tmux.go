// Package tmux is the mux.Backend rig grew up on. Rig's marks ride as tmux
// user options on the pane or window they describe, which is the one place
// tmux offers free-form metadata that survives everything short of the pane
// dying.
//
// Every read asks tmux for data with an explicit -F format, never by scanning
// its human output: a format is a contract this package writes, so tmux is free
// to change how it renders and rig doesn't care.
package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/phinze/rig/internal/mux"
)

const (
	paneRoleOption   = "@rig-pane-role"
	paneRepoOption   = "@rig-pane-repo"
	windowRoleOption = "@rig-window-role"
	windowRepoOption = "@rig-window-repo"
)

// Backend drives the tmux server the current environment points at.
type Backend struct{}

var _ mux.Backend = Backend{}

func (Backend) Name() string { return "tmux" }

// Surface is the kind alone: this backend only ever drives the local server.
func (b Backend) Surface() string { return b.Name() }

// command builds a tmux invocation with -u forced on.
//
// tmux decides whether a client can handle UTF-8 from LANG/LC_ALL/LC_CTYPE at
// invocation time, and runs the output of any client it judges non-UTF-8
// through utf8_sanitize, which rewrites every non-printable byte to "_". That
// silently eats the tabs delimiting our -F format fields and the glyphs the
// radar reads out of pane titles, so rig sees one field where it expected
// eight and quietly drops the row. macOS hands GUI-launched processes no
// locale at all, so shells spawned by desktop apps and over ssh routinely
// arrive with none. Forcing the flag is cheaper and more honest than
// asserting a specific locale rig has no way to know is installed.
func command(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-u"}, args...)...)
}

func (Backend) HasSession(name string) bool {
	return command("has-session", "-t", name).Run() == nil
}

// Endpoint returns the exact server socket used by the current tmux
// environment. Teardown persists it before crossing into a systemd service,
// whose environment may not carry TMUX or TMUX_TMPDIR.
func (Backend) Endpoint() string {
	out, err := command("display-message", "-p", "#{socket_path}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Sessions lists every live session with the fields the radar needs, from a
// single list-sessions. Returns nil when tmux isn't running. A tab delimiter
// keeps paths with spaces intact (session names are dash-normalized, so they
// never carry a tab).
func (Backend) Sessions() []mux.Session {
	out, err := command("list-sessions", "-F",
		"#{session_last_attached}\t#{session_path}\t#{session_name}").Output()
	if err != nil {
		return nil
	}
	var sessions []mux.Session
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		secs, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		sessions = append(sessions, mux.Session{
			Surface:      "tmux",
			Name:         fields[2],
			Path:         fields[1],
			LastAttached: secs,
		})
	}
	return sessions
}

// CurrentSession returns the name of the session the current process is
// running inside, or "" if not inside tmux.
func (Backend) CurrentSession() string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	out, err := command("display-message", "-p", "#S").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// CurrentPane is the pane id tmux hands every process it spawns.
func (Backend) CurrentPane() string {
	return os.Getenv("TMUX_PANE")
}

// Attach switches to the target if already inside tmux, otherwise attaches.
func (Backend) Attach(target string) error {
	bin := "attach"
	if os.Getenv("TMUX") != "" {
		bin = "switch-client"
	}
	cmd := command(bin, "-t", target)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Popup is display-popup -E, sized in cells to a board's worth rather than
// a share of the client: 80% of a full-screen window on a large display is a
// popup most of which is empty.
func (Backend) Popup(session, cmdline string) error {
	w, h := popupCols, popupRows
	if out, err := command("display-message", "-p", "-t", session, "#{client_width} #{client_height}").Output(); err == nil {
		var cw, ch int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d %d", &cw, &ch); err == nil && cw > 0 && ch > 0 {
			w = min(w, cw*9/10)
			h = min(h, ch*9/10)
		}
	}
	cmd := command("display-popup", "-E", "-t", session, "-w", strconv.Itoa(w), "-h", strconv.Itoa(h), cmdline)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// popupCols and popupRows are the board's natural size; both backends aim at
// it and shrink only when the window can't seat it.
const (
	popupCols = 120
	popupRows = 32
)

// NewSession creates the first, task-level window for a rig and returns
// stable pane/window ids for the metadata and split operations that follow.
// A rig session has an explicit window name so tmux never replaces its identity
// with "claude" or "recto".
func (Backend) NewSession(name, windowName, cwd string) (string, string, error) {
	cmd := command("new-session", "-d",
		"-s", name, "-n", windowName, "-c", cwd,
		"-P", "-F", "#{pane_id}\t#{window_id}",
	)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	fields := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(fields) != 2 {
		return "", "", fmt.Errorf("unexpected tmux new-session output %q", out)
	}
	return fields[0], fields[1], nil
}

func (Backend) SplitCommand(target, cwd, cmdline string) (string, error) {
	cmd := command("split-window", "-d", "-h", "-l", "50%",
		"-t", target, "-c", cwd, "-P", "-F", "#{pane_id}", cmdline)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (Backend) SplitShell(target, cwd string) (string, error) {
	cmd := command("split-window", "-d", "-h", "-l", "50%",
		"-t", target, "-c", cwd, "-P", "-F", "#{pane_id}")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// NewCommandWindow creates a detached, explicitly named window whose only
// pane runs the command. Rig uses it to park one persistent Recto per
// repository.
func (Backend) NewCommandWindow(session, name, cwd, cmdline string) (string, string, error) {
	cmd := command("new-window", "-d",
		"-t", session, "-n", name, "-c", cwd,
		"-P", "-F", "#{pane_id}\t#{window_id}", cmdline,
	)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	fields := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(fields) != 2 {
		return "", "", fmt.Errorf("unexpected tmux new-window output %q", out)
	}
	return fields[0], fields[1], nil
}

func (Backend) MarkPane(pane, role, repo string) error {
	if err := setOption("-p", pane, paneRoleOption, role); err != nil {
		return err
	}
	return setOption("-p", pane, paneRepoOption, repo)
}

func (Backend) MarkWindow(window, role, repo string) error {
	if err := setOption("-w", window, windowRoleOption, role); err != nil {
		return err
	}
	return setOption("-w", window, windowRepoOption, repo)
}

func setOption(scope, target, name, value string) error {
	cmd := command("set-option", scope, "-t", target, name, value)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// paneFormat is the one list-panes contract, shared by Panes and AllPanes so
// the two can never disagree about a field. Tabs delimit because paths and
// titles carry spaces; session names and ids never carry a tab.
var paneFormat = strings.Join([]string{
	"#{session_name}", "#{pane_id}", "#{pane_index}",
	"#{window_id}", "#{window_index}", "#{window_name}",
	"#{" + paneRoleOption + "}", "#{" + paneRepoOption + "}",
	"#{" + windowRoleOption + "}", "#{" + windowRepoOption + "}",
	"#{pane_current_command}", "#{pane_current_path}", "#{window_activity}", "#{pane_title}",
}, "\t")

const paneFields = 14

func (Backend) Panes(session string) ([]mux.Pane, error) {
	out, err := command("list-panes", "-s", "-t", session, "-F", paneFormat).Output()
	if err != nil {
		return nil, err
	}
	return parsePanes(string(out)), nil
}

// AllPanes sweeps the whole tree in one list-panes -a. Nil when tmux isn't
// running, since the radar draws an empty board rather than an error then.
func (Backend) AllPanes() ([]mux.Pane, error) {
	out, err := command("list-panes", "-a", "-F", paneFormat).Output()
	if err != nil {
		return nil, nil
	}
	return parsePanes(string(out)), nil
}

// parsePanes is the pure core of Panes and AllPanes: one paneFormat line per
// pane, in the order tmux listed them. A line with the wrong field count is
// dropped rather than misread, which is what a stray tab in a title would
// otherwise produce.
func parsePanes(out string) []mux.Pane {
	var panes []mux.Pane
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", paneFields)
		if len(f) != paneFields {
			continue
		}
		activity, _ := strconv.ParseInt(f[12], 10, 64)
		panes = append(panes, mux.Pane{
			Surface: "tmux",
			Session: f[0], PaneID: f[1], PaneIdx: f[2],
			WindowID: f[3], WindowIdx: f[4], WindowName: f[5],
			Target:     f[0] + ":" + f[4] + "." + f[2],
			Role:       f[6],
			Repo:       f[7],
			WindowRole: f[8],
			WindowRepo: f[9],
			Command:    f[10],
			Path:       f[11],
			Activity:   activity,
			Title:      f[13],
		})
	}
	return panes
}

func (Backend) RenameWindow(window, name string) error {
	cmd := command("rename-window", "-t", window, name)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (Backend) JoinPane(src, dst string) error {
	cmd := command("join-pane", "-d", "-f", "-h", "-l", "50%", "-s", src, "-t", dst)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (Backend) BreakPane(src, name string) (string, error) {
	cmd := command("break-pane", "-d", "-s", src, "-n", name,
		"-P", "-F", "#{window_id}")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (Backend) SelectPane(target string) error {
	cmd := command("select-pane", "-t", target)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// SendKeys types text into the target pane, then presses Enter.
func (Backend) SendKeys(target, text string) error {
	cmd := command("send-keys", "-t", target, text, "Enter")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func (b Backend) KillSession(name string) error {
	if !b.HasSession(name) {
		return nil
	}
	cmd := command("kill-session", "-t", name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// KillSessionAt addresses the server recorded before teardown left the
// caller's pane. A missing socket means that whole server is gone, but a live
// socket that cannot be queried is an error: cleanup must retain its durable
// job instead of mistaking a connection failure for an absent session.
func (b Backend) KillSessionAt(name, socket string) error {
	if socket == "" {
		// Backward compatibility for teardown jobs written before tmux_socket was
		// recorded. Their retry still uses the caller's tmux environment.
		return b.KillSession(name)
	}
	if _, err := os.Stat(socket); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking tmux socket %s: %w", socket, err)
	}
	out, err := command("-S", socket, "list-sessions", "-F", "#{session_name}").CombinedOutput()
	if err != nil {
		return fmt.Errorf("listing sessions on %s: %w: %s", socket, err, strings.TrimSpace(string(out)))
	}
	found := false
	for session := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if session == name {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	cmd := command("-S", socket, "kill-session", "-t", "="+name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
