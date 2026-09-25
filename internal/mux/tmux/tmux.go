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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/phinze/rig/internal/mux"
)

const (
	paneRoleOption   = "@rig-pane-role"
	paneRepoOption   = "@rig-pane-repo"
	windowRoleOption = "@rig-window-role"
	windowRepoOption = "@rig-window-repo"
)

// Backend drives one tmux server. With Host empty that's the one the current
// environment points at; with it set, every invocation goes over ssh to that
// host's default server, which is how a board on the Mac lists the sessions on
// a devbox. Place names that host on the board and in its Surface.
type Backend struct {
	Host  string
	Place string
}

var _ mux.Backend = Backend{}

func (b Backend) Name() string { return "tmux" }

// Surface is the kind alone for the local server and tmux@place for another.
func (b Backend) Surface() string {
	if b.Place == "" {
		return b.Name()
	}
	return b.Name() + "@" + b.Place
}

func (b Backend) remote() bool { return b.Host != "" }

// sshOptions keep a remote call cheap and bounded. The radar lists every
// surface on each refresh, so the connection is shared and kept warm rather
// than handshaken per call, and a host that has dropped off costs a short
// connect timeout. BatchMode because nobody is there to answer a prompt.
//
// ControlPersist is long because the radar is a fresh process per popup: a
// master that expired between popups meant a new connection, and a new ssh
// agent approval, nearly every time you opened it. Thirty idle minutes is
// one approval per working stretch. It's a stopgap; PERS-20 is the non-ssh
// link that makes listing not need ssh at all.
var sshOptions = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=3",
	"-o", "ControlMaster=auto",
	"-o", "ControlPath=~/.ssh/rig-%C",
	"-o", "ControlPersist=30m",
}

// breaker is shared by every remote instance, keyed by host.
var breaker = &mux.Breaker{For: 30 * time.Second}

// errUnreachable is what a remote call returns while the breaker is open.
var errUnreachable = fmt.Errorf("host unreachable (retrying shortly)")

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
func (b Backend) command(args ...string) *exec.Cmd {
	args = append([]string{"-u"}, args...)
	if !b.remote() {
		return exec.Command("tmux", args...)
	}
	return exec.Command("ssh", append(sshOptions, b.Host, remoteLine("tmux", args...))...)
}

// remoteLine is one command line for the far side's shell. ssh joins its
// arguments with spaces and hands them to a shell, so every argument has to
// arrive quoted, -F formats with their tabs and braces included.
func remoteLine(name string, args ...string) string {
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, name)
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}
	return strings.Join(quoted, " ")
}

// shellQuote single-quotes s for whatever login shell the far side runs,
// which is not always POSIX: foxtrotbase's is fish. The two agree on single
// quotes except for backslashes, which fish still interprets inside them, so
// both a quote and a backslash are spelled outside the quotes, where \' and \\
// mean the same thing in either shell.
func shellQuote(s string) string {
	r := strings.NewReplacer(`'`, `'\''`, `\`, `'\\'`)
	return "'" + r.Replace(s) + "'"
}

// output runs a listing call. A remote one fails fast while its host is known
// to be down, and trips the breaker when ssh itself fails, which ssh reports
// as exit status 255; tmux's own errors (no server running, no such session)
// mean the host answered.
func (b Backend) output(args ...string) ([]byte, error) {
	if b.remote() && breaker.Down(b.Host) {
		return nil, errUnreachable
	}
	out, err := b.command(args...).Output()
	if b.remote() {
		var exit *exec.ExitError
		breaker.Note(b.Host, err != nil && (!errors.As(err, &exit) || exit.ExitCode() == 255))
	}
	return out, err
}

// Unreachable reports whether this is a remote instance whose host a recent
// call couldn't reach.
func (b Backend) Unreachable() bool { return b.remote() && breaker.Down(b.Host) }

func (b Backend) HasSession(name string) bool {
	_, err := b.output("has-session", "-t", name)
	return err == nil
}

// Endpoint returns the exact server socket used by the current tmux
// environment. Teardown persists it before crossing into a systemd service,
// whose environment may not carry TMUX or TMUX_TMPDIR. A remote instance names
// its host instead; nothing tears down across it.
func (b Backend) Endpoint() string {
	if b.remote() {
		return "ssh://" + b.Host
	}
	out, err := b.command("display-message", "-p", "#{socket_path}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Sessions lists every live session with the fields the radar needs, from a
// single list-sessions. Returns nil when tmux isn't running. A tab delimiter
// keeps paths with spaces intact (session names are dash-normalized, so they
// never carry a tab).
func (b Backend) Sessions() []mux.Session {
	out, err := b.output("list-sessions", "-F",
		"#{session_last_attached}\t#{session_path}\t#{session_name}")
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
			Surface:      b.Surface(),
			Name:         fields[2],
			Path:         fields[1],
			LastAttached: secs,
		})
	}
	return sessions
}

// CurrentSession returns the name of the session the current process is
// running inside, or "" if not inside tmux.
func (b Backend) CurrentSession() string {
	if os.Getenv("TMUX") == "" || b.remote() {
		return ""
	}
	out, err := b.command("display-message", "-p", "#S").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// CurrentPane is the pane id tmux hands every process it spawns.
func (b Backend) CurrentPane() string {
	if b.remote() {
		return ""
	}
	return os.Getenv("TMUX_PANE")
}

// Attach switches to the target if already inside tmux, otherwise attaches.
// A remote instance can only attach from a bare terminal, over ssh -t; from
// inside a multiplexer the client to move is somewhere else entirely, which
// is what rig's portal (portal.go) exists to reach.
func (b Backend) Attach(target string) error {
	if b.remote() {
		if os.Getenv("TMUX") != "" || os.Getenv("REX_SESSION") != "" {
			return mux.ErrNoClientSwitch
		}
		cmd := exec.Command("ssh", "-t", b.Host, remoteLine("tmux", "-u", "attach-session", "-t", target))
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	bin := "attach"
	if os.Getenv("TMUX") != "" {
		bin = "switch-client"
	}
	cmd := b.command(bin, "-t", target)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Popup is display-popup -E, sized in cells to a board's worth rather than
// a share of the client: 80% of a full-screen window on a large display is a
// popup most of which is empty.
func (b Backend) Popup(session, cmdline string) error {
	w, h := popupCols, popupRows
	if out, err := b.command("display-message", "-p", "-t", session, "#{client_width} #{client_height}").Output(); err == nil {
		var cw, ch int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d %d", &cw, &ch); err == nil && cw > 0 && ch > 0 {
			w = min(w, cw*9/10)
			h = min(h, ch*9/10)
		}
	}
	cmd := b.command("display-popup", "-E", "-t", session, "-w", strconv.Itoa(w), "-h", strconv.Itoa(h), cmdline)
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
func (b Backend) NewSession(name, windowName, cwd string) (string, string, error) {
	cmd := b.command("new-session", "-d",
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

func (b Backend) SplitCommand(target, cwd, cmdline string) (string, error) {
	cmd := b.command("split-window", "-d", "-h", "-l", "50%",
		"-t", target, "-c", cwd, "-P", "-F", "#{pane_id}", cmdline)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (b Backend) SplitShell(target, cwd string) (string, error) {
	cmd := b.command("split-window", "-d", "-h", "-l", "50%",
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
func (b Backend) NewCommandWindow(session, name, cwd, cmdline string) (string, string, error) {
	cmd := b.command("new-window", "-d",
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

func (b Backend) MarkPane(pane, role, repo string) error {
	if err := b.setOption("-p", pane, paneRoleOption, role); err != nil {
		return err
	}
	return b.setOption("-p", pane, paneRepoOption, repo)
}

func (b Backend) MarkWindow(window, role, repo string) error {
	if err := b.setOption("-w", window, windowRoleOption, role); err != nil {
		return err
	}
	return b.setOption("-w", window, windowRepoOption, repo)
}

func (b Backend) setOption(scope, target, name, value string) error {
	cmd := b.command("set-option", scope, "-t", target, name, value)
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

func (b Backend) Panes(session string) ([]mux.Pane, error) {
	out, err := b.output("list-panes", "-s", "-t", session, "-F", paneFormat)
	if err != nil {
		return nil, err
	}
	return parsePanes(string(out), b.Surface()), nil
}

// AllPanes sweeps the whole tree in one list-panes -a. Nil when tmux isn't
// running, since the radar draws an empty board rather than an error then.
func (b Backend) AllPanes() ([]mux.Pane, error) {
	out, err := b.output("list-panes", "-a", "-F", paneFormat)
	if err != nil {
		return nil, nil
	}
	return parsePanes(string(out), b.Surface()), nil
}

// parsePanes is the pure core of Panes and AllPanes: one paneFormat line per
// pane, in the order tmux listed them. A line with the wrong field count is
// dropped rather than misread, which is what a stray tab in a title would
// otherwise produce.
func parsePanes(out, surface string) []mux.Pane {
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
			Surface: surface,
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

func (b Backend) RenameWindow(window, name string) error {
	cmd := b.command("rename-window", "-t", window, name)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (b Backend) JoinPane(src, dst string) error {
	cmd := b.command("join-pane", "-d", "-f", "-h", "-l", "50%", "-s", src, "-t", dst)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (b Backend) BreakPane(src, name string) (string, error) {
	cmd := b.command("break-pane", "-d", "-s", src, "-n", name,
		"-P", "-F", "#{window_id}")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (b Backend) SelectPane(target string) error {
	cmd := b.command("select-pane", "-t", target)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// SendKeys types text into the target pane, then presses Enter.
func (b Backend) SendKeys(target, text string) error {
	cmd := b.command("send-keys", "-t", target, text, "Enter")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func (b Backend) KillSession(name string) error {
	if !b.HasSession(name) {
		return nil
	}
	cmd := b.command("kill-session", "-t", name)
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
	out, err := b.command("-S", socket, "list-sessions", "-F", "#{session_name}").CombinedOutput()
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
	cmd := b.command("-S", socket, "kill-session", "-t", "="+name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// portalOption is where a portal client records its own tty on the remote
// server, so rig can move exactly that client later without guessing which of
// the server's clients it is: several can be attached at once (another
// terminal, another device), and the portal is the one on the screen rig can
// hop to.
const portalOption = "@rig-portal-tty"

// PortalCommand is the command line, for this machine's shell, that a portal
// runs: a tmux client attached to target that stamps its own tty into
// portalOption as it arrives, over an interactive ssh when the server is on
// another host. The stamp rides in the same command sequence as the attach,
// so the client it names is the one that just attached.
func (b Backend) PortalCommand(target string) string {
	attach := remoteLine("tmux", "-u", "attach-session", "-t", target,
		";", "set-option", "-gF", portalOption, "#{client_tty}")
	if !b.remote() {
		return attach
	}
	return "ssh -t " + shellQuote(b.Host) + " " + shellQuote(attach)
}

// PortalTTY is the tty the portal stamped, provided a client on that tty is
// still attached. A portal whose ssh has exited leaves its stamp behind, so
// the stamp alone doesn't mean there's anything left to move.
func (b Backend) PortalTTY() (string, error) {
	out, err := b.output("show-options", "-gqv", portalOption)
	if err != nil {
		return "", err
	}
	tty := strings.TrimSpace(string(out))
	if tty == "" {
		return "", fmt.Errorf("no portal has attached to %s", b.Host)
	}
	clients, err := b.output("list-clients", "-F", "#{client_tty}")
	if err != nil {
		return "", err
	}
	for c := range strings.SplitSeq(strings.TrimSpace(string(clients)), "\n") {
		if c == tty {
			return tty, nil
		}
	}
	return "", fmt.Errorf("the portal client on %s (%s) is gone", b.Host, tty)
}

// ClientTTY is the tty of the client the current process is displayed
// through, from inside this machine's tmux; "" outside it or for a remote
// instance.
func (b Backend) ClientTTY() string {
	if b.remote() || os.Getenv("TMUX") == "" {
		return ""
	}
	out, err := b.output("display-message", "-p", "#{client_tty}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// SwitchClient moves the client on tty to target, from outside that client:
// tmux addresses a client by its tty, so this works over ssh from anywhere.
func (b Backend) SwitchClient(tty, target string) error {
	out, err := b.command("switch-client", "-c", tty, "-t", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("switching the portal on %s to %s: %w: %s", b.Host, target, err, strings.TrimSpace(string(out)))
	}
	return nil
}
