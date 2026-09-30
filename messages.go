package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Rig-mediated cross-agent messaging. Rig owns addressing (a rig id), the
// message envelope, persistence (.rig/messages.jsonl), and the verbs;
// each agent vendor's mechanism is a transport underneath. The claude
// transport — posting one NDJSON line to the session's pid-named inbox
// socket — is the one this file implements, because it's the one verified
// end to end by hand (see the pers-22 lab notes). Delivery lands at the
// receiver's next turn boundary, attributed by sender name.
//
// At-most-once, fully logged: failure is loud, and both sides of a
// rig-to-rig send get a record. A message is an instruction with
// provenance, never consent — it cannot approve anything, and the whole
// trust boundary is "any process running as this user", same as send-keys.

// rigMessage is one line of a rig's .rig/messages.jsonl. Out-of-rig senders
// log to the receiver's side only; a send between two rigs logs both halves
// with the same id, so a thread can be reconstructed from either side.
type rigMessage struct {
	V         int       `json:"v"`
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Dir       string    `json:"dir"` // in | out
	From      string    `json:"from"`
	FromAddr  string    `json:"fromAddr,omitempty"` // rig:<id> | cli:<user>; routes replies
	To        string    `json:"to"`
	Text      string    `json:"text"`
	Transport string    `json:"transport"`         // claude-socket
	ReplyTo   string    `json:"replyTo,omitempty"` // correlating message id
	Delivered bool      `json:"delivered"`         // false rows carry Error
	Error     string    `json:"error,omitempty"`   // why delivery failed
}

// messagesPath is a rig's thread log, under the rig's own .rig so teardown
// and board reasoning stay per-rig. appendRigMessage takes the lock around
// its single line write because senders can run concurrently from any rig.
func messagesPath(basedir string) string {
	return filepath.Join(basedir, ".rig", "messages.jsonl")
}

func appendRigMessage(basedir string, msg rigMessage) error {
	if err := os.MkdirAll(filepath.Join(basedir, ".rig"), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(messagesPath(basedir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	_, err = f.Write(append(line, '\n'))
	return err
}

// readRigMessages parses a rig's thread log. A corrupt last line is dropped
// rather than failing the read: an appender killed mid-write leaves exactly
// that, and the thread up to it is still worth showing.
func readRigMessages(basedir string) []rigMessage {
	f, err := os.Open(messagesPath(basedir))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []rigMessage
	dec := json.NewDecoder(f)
	for {
		var m rigMessage
		if err := dec.Decode(&m); err != nil {
			break
		}
		out = append(out, m)
	}
	return out
}

// marshalWire renders one NDJSON line with HTML escaping off: the bytes
// claude's own emitter writes show literal `<cross-session-message>` tags,
// and matching them costs nothing while pre-empting any consumer that
// pattern-matches the wrapper instead of decoding first.
func marshalWire(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(b.String(), "\n")), nil
}

func uuidV4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// rigSender names the current sender the way the receiver should see it:
// inside a rig, the rig's id with a "rig:" address prefix ("rig:pers-22",
// the form the verified capture used); outside one, the local user on a
// "cli:" address, which nothing can route a reply to — `rig reply` says so
// plainly rather than pretending.
type rigSender struct {
	name string
	addr string
	rig  *rigInfo // non-nil when sent from inside a rig
}

func currentSender() rigSender {
	if cwd, err := os.Getwd(); err == nil {
		if basedir, err := findBasedir(cwd); err == nil {
			if m, err := readManifest(basedir); err == nil && m.ID != "" {
				return rigSender{
					name: m.ID,
					addr: "rig:" + m.ID,
					rig:  &rigInfo{ID: m.ID, Path: basedir},
				}
			}
		}
	}
	who := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		who = u.Username
	}
	if who == "" {
		who = "unknown"
	}
	return rigSender{name: who, addr: "cli:" + who}
}

// resolveRig answers "which rig is <query>": id, slug, or tracker id, what
// dispatch generalized from. Unlike dispatch's resolver it doesn't exclude
// project rigs — a project rig's agent is exactly who a CoS coordinates
// through.
func resolveRig(query string) (rigInfo, error) {
	rigs, err := listRigs()
	if err != nil {
		return rigInfo{}, err
	}
	var matches []rigInfo
	for _, r := range rigs {
		if strings.EqualFold(r.ID, query) || strings.EqualFold(r.Slug, query) ||
			(r.TrackerID != "" && strings.EqualFold(r.TrackerID, query)) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return rigInfo{}, fmt.Errorf("no rig matches %q", query)
	case 1:
		return matches[0], nil
	default:
		return rigInfo{}, fmt.Errorf("rig query %q is ambiguous", query)
	}
}

// --- claude socket transport ----------------------------------------------

// claudeEnvelope is one line of the inbox-socket wire format, field-for-field
// the shape captured from claude's own SendMessage traffic plus the sender
// attribution the lab's bare-poster experiment proved is honored.
type claudeEnvelope struct {
	MsgV    int    `json:"msgV"`
	MsgID   string `json:"msg_id"`
	Type    string `json:"type"`
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Priority string `json:"priority"`
	From     string `json:"from"`
}

func buildClaudeEnvelope(msgID, fromAddr, fromName, text string) claudeEnvelope {
	e := claudeEnvelope{
		MsgV:     1,
		MsgID:    msgID,
		Type:     "user",
		Priority: "next",
		From:     fromAddr,
	}
	e.Message.Role = "user"
	e.Message.Content = fmt.Sprintf(
		"<cross-session-message from=%q from-name=%q from-mode=\"bypass\">\n%s\n</cross-session-message>",
		fromAddr, fromName, text)
	return e
}

// claudeSocketPath is where a pid's claude session listens, mirroring the
// vendor's cc-socks convention. XDG_RUNTIME_DIR drives it; /run/user/<uid>
// is its systemd spelling. Elsewhere (notably darwin) the location is
// unverified, so the probe says it doesn't know rather than guessing a path.
func claudeSocketPath(pid int) (string, error) {
	root := os.Getenv("XDG_RUNTIME_DIR")
	if root == "" {
		if u, err := user.Current(); err == nil && u.Uid != "" {
			root = filepath.Join("/run/user", u.Uid)
		}
	}
	if root == "" {
		return "", fmt.Errorf("no XDG_RUNTIME_DIR; claude's socket root is unverified on this platform")
	}
	return filepath.Join(root, "cc-socks", fmt.Sprintf("%d.sock", pid)), nil
}

// procEntry is one row of the process table the walker reads.
type procEntry struct {
	pid, ppid int
	comm      string
	argv0     string // basename of the argv0, when ps reported one
}

// isCommand matches a process table row to an agent binary name. comm is the
// usual spelling, but a launcher can rewrite it — nix wraps claude, so on a
// NixOS box comm is ".claude-unwrapp" (15-char truncation and all) — and
// argv0's basename is the durable answer.
func (e procEntry) isCommand(name string) bool {
	return e.comm == name || e.argv0 == name
}

// parseProcessTable parses `ps -eo pid=,ppid=,comm=,args=` output: pid, ppid,
// comm, then the full command line, from which only argv0's basename matters.
func parseProcessTable(out string) []procEntry {
	var procs []procEntry
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		var e procEntry
		if _, err := fmt.Sscanf(fields[0], "%d", &e.pid); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(fields[1], "%d", &e.ppid); err != nil {
			continue
		}
		e.comm = fields[2]
		if len(fields) > 3 {
			e.argv0 = filepath.Base(fields[3])
		}
		procs = append(procs, e)
	}
	return procs
}

// descendantPID finds the shallowest process named comm at or under root —
// shallowest because the pane's own agent process always sits above any
// child it may have spawned, and the agent is the one holding the socket.
func descendantPID(procs []procEntry, root int, name string) (int, bool) {
	children := make(map[int][]int)
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p.pid)
	}
	byPID := make(map[int]procEntry, len(procs))
	for _, p := range procs {
		byPID[p.pid] = p
	}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if p, ok := byPID[pid]; ok && p.isCommand(name) {
			return pid, true
		}
		queue = append(queue, children[pid]...)
	}
	return 0, false
}

// agentPanePID answers the live question "which pid is this rig's <name>":
// the pane whose mark says agent, its root pid from tmux, and the shallowest
// <name> in that subtree. Each step's failure says its own reason, because
// these are the messages `rig send` surfaces when a rig is unreachable —
// the difference between "no session" and "session, but the agent exited".
func agentPanePID(rs rigSession, basedir string, m manifest, name string) (int, error) {
	backend, ok := rs.b.(interface{ PaneRootPID(string) (int, error) })
	if !ok {
		return 0, fmt.Errorf("can't probe a %s session (only local tmux for now)", rs.b.Name())
	}
	panes, err := adoptLegacyRigPanes(rs, basedir, m)
	if err != nil {
		return 0, err
	}
	var agentPanes []string
	for _, p := range panes {
		if p.Role == rigPaneAgent {
			agentPanes = append(agentPanes, p.PaneID)
		}
	}
	if len(agentPanes) == 0 {
		return 0, fmt.Errorf("no agent pane marked in session %s", rs.name)
	}
	out, err := exec.Command("ps", "-eo", "pid=,ppid=,comm=,args=").Output()
	if err != nil {
		return 0, fmt.Errorf("listing processes: %w", err)
	}
	procs := parseProcessTable(string(out))
	for _, pane := range agentPanes {
		root, err := backend.PaneRootPID(pane)
		if err != nil {
			continue
		}
		if pid, ok := descendantPID(procs, root, name); ok {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("no %s process under the agent pane (agent exited to its shell?)", name)
}

// claudeAgentPID is the claude spelling of agentPanePID, kept for the socket
// probe's callers.
func claudeAgentPID(rs rigSession, basedir string, m manifest) (int, error) {
	return agentPanePID(rs, basedir, m, "claude")
}

// probeClaudeReachable is the fail-loud reachability check every send runs
// first: session live, agent pid found, inbox socket present. The reasons
// are distinct on purpose — a CoS told "not reachable" needs to know which
// repair to propose.
func probeClaudeReachable(rs rigSession, basedir string, m manifest) (sockPath string, pid int, err error) {
	if !rs.live() {
		return "", 0, fmt.Errorf("%s has no live session (wake or dispatch it first)", m.ID)
	}
	pid, err = claudeAgentPID(rs, basedir, m)
	if err != nil {
		return "", 0, err
	}
	sockPath, err = claudeSocketPath(pid)
	if err != nil {
		return "", 0, err
	}
	info, err := os.Stat(sockPath)
	if err != nil {
		return "", 0, fmt.Errorf("no inbox socket for claude pid %d (agent predates messaging, or not claude): %w", pid, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", 0, fmt.Errorf("%s is not a socket", sockPath)
	}
	return sockPath, pid, nil
}

// postClaudeMessage writes one NDJSON line to the inbox socket: the whole
// verified interaction, no auth line needed on Linux. A dial to a stale
// socket file fails fast; a write gets a deadline so a wedged peer can't
// hang a batch send.
func postClaudeMessage(sockPath string, line []byte) error {
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", sockPath, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("posting to %s: %w", sockPath, err)
	}
	return nil
}

// --- codex queue transport ------------------------------------------------

// codexDaemonSock is the app-server control socket `codex queue` talks to.
// The lab found the hard part of this transport: when the daemon is down,
// `codex queue` still exits 0 and prints "Queued message", but nothing is
// delivered until the daemon comes back — and a stopped daemon's session
// shows a reconnect banner, not an error. So the socket is probed up front;
// without it a queue success is not a delivery.
func codexDaemonSock(home string) string {
	return filepath.Join(home, ".codex", "app-server-control", "app-server-control.sock")
}

// codexThreadFor resolves the thread UUID a rig's codex session answers to.
// Codex auto-labels sessions from the first prompt, but those labels are not
// reliably unique (the lab's lookup refused when more than one session
// shared a name), so the UUID from the rollout's session_meta is what the
// transport uses. Empty when the rig has no codex rollout recorded yet.
func codexThreadFor(home, basedir string) string {
	_, id := codexNewestSession(home, basedir)
	return id
}

// codexMessage prefixes sender attribution onto the text. Codex's queue has
// no cross-session envelope of its own, so without this the receiver sees a
// bare instruction with no provenance — the one property the rig envelope
// exists to guarantee on every transport.
func codexMessage(sender rigSender, text string) string {
	return fmt.Sprintf("[rig message from %s (%s)]\n%s", sender.name, sender.addr, text)
}

// probeCodexReachable is the fail-loud check every codex send runs: the
// session is live, a codex process holds the agent pane, the app-server
// daemon is up, and the rig's thread UUID resolves. Each failure names its
// own repair — the daemon one in particular, because a down daemon is the
// failure that otherwise looks like success.
func probeCodexReachable(rs rigSession, basedir string, m manifest) (string, error) {
	if !rs.live() {
		return "", fmt.Errorf("%s has no live session (wake or dispatch it first)", m.ID)
	}
	if _, err := agentPanePID(rs, basedir, m, "codex"); err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home for codex state: %w", err)
	}
	sockPath := codexDaemonSock(home)
	info, err := os.Stat(sockPath)
	if err != nil {
		return "", fmt.Errorf("codex app-server daemon isn't running (start it: `codex app-server daemon start`); without it queue accepts the message and delivers nothing: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", fmt.Errorf("%s is not a socket", sockPath)
	}
	thread := codexThreadFor(home, basedir)
	if thread == "" {
		return "", fmt.Errorf("no codex thread recorded for %s (nothing to queue into)", m.ID)
	}
	return thread, nil
}

// postCodexMessage runs `codex queue` against the rig's thread. The daemon
// probe ran first, so exit 0 here means the daemon accepted and persisted the
// message; delivery to the session is durable and flushes on reconnect.
func postCodexMessage(thread, text string) error {
	cmd := exec.Command("codex", "queue", "--thread", thread, "--message", text)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("codex queue: %w", err)
		}
		return fmt.Errorf("codex queue: %s", msg)
	}
	return nil
}

// --- verbs ------------------------------------------------------------------

// deliverRigMessage is the shared core of send and reply: probe, post, log
// both sides, report. Each agent kind routes to its transport; a kind with no
// transport yet fails loudly rather than silently pretending to coordinate.
func deliverRigMessage(target rigInfo, text, replyTo string, sender rigSender) error {
	m, err := readManifest(target.Path)
	if err != nil {
		return err
	}
	msgID, err := uuidV4()
	if err != nil {
		return err
	}
	record := rigMessage{
		V: 1, ID: msgID, At: time.Now(), Dir: "in",
		From: sender.name, FromAddr: sender.addr, To: m.ID, Text: text, ReplyTo: replyTo,
	}

	kind := m.agentKind()
	rs := sessionFor(target.Path, m)

	var postErr error
	switch kind {
	case agentClaude:
		record.Transport = "claude-socket"
		postErr = deliverClaudeMessage(rs, target, m, record, text, sender)
	case agentCodex:
		record.Transport = "codex-queue"
		postErr = deliverCodexMessage(rs, target, m, text, sender)
	default:
		postErr = fmt.Errorf("rig send speaks claude and codex for now; %s runs %s", m.ID, kind)
	}

	if postErr != nil {
		record.Delivered = false
		record.Error = postErr.Error()
		_ = appendRigMessage(target.Path, record)
		return postErr
	}

	record.Delivered = true
	if err := appendRigMessage(target.Path, record); err != nil {
		return fmt.Errorf("delivered but logging to %s failed: %w", target.Path, err)
	}
	if sender.rig != nil {
		record.Dir = "out"
		if err := appendRigMessage(sender.rig.Path, record); err != nil {
			fmt.Fprintf(os.Stderr, "rig: warning: logging to %s: %v\n", sender.rig.Path, err)
		}
	}
	return nil
}

// deliverClaudeMessage posts the verified NDJSON envelope to the session's
// inbox socket. A pre-messaging session accepts the post but holds it behind
// an approval prompt nobody sees — the lab's silent pile-up — so a missing
// settings file downgrades the success line to a warning.
func deliverClaudeMessage(rs rigSession, target rigInfo, m manifest, record rigMessage, text string, sender rigSender) error {
	sockPath, _, err := probeClaudeReachable(rs, target.Path, m)
	if err != nil {
		return err
	}
	env := buildClaudeEnvelope(record.ID, sender.addr, sender.name, text)
	line, err := marshalWire(env)
	if err != nil {
		return err
	}
	if err := postClaudeMessage(sockPath, line); err != nil {
		return err
	}
	if _, err := os.Stat(claudeRigSettingsPath(target.Path)); err != nil {
		fmt.Fprintf(os.Stderr, "rig: delivered, but %s predates messaging settings: resume it (`rig resume`) or the message may wait behind an approval prompt\n", m.ID)
	} else {
		fmt.Fprintf(os.Stderr, "rig: sent to %s\n", m.ID)
	}
	return nil
}

// deliverCodexMessage probes the daemon and thread, then hands the message to
// `codex queue`. The receiver sees the sender attribution prefix, so a codex
// message carries the same provenance the claude envelope does.
func deliverCodexMessage(rs rigSession, target rigInfo, m manifest, text string, sender rigSender) error {
	thread, err := probeCodexReachable(rs, target.Path, m)
	if err != nil {
		return err
	}
	if err := postCodexMessage(thread, codexMessage(sender, text)); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rig: sent to %s\n", m.ID)
	return nil
}

func runSend(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: rig send RIG MESSAGE...")
	}
	target, err := resolveRig(args[0])
	if err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(args[1:], " "))
	if text == "" {
		return fmt.Errorf("rig send: empty message")
	}
	return deliverRigMessage(target, text, "", currentSender())
}

// runReply answers the most recent delivered inbound message in the current
// rig's log, routing by its From address. Replies are sends with a
// correlation id; the verb exists so an agent's easy path is the right one
// rather than a convention in message text.
func runReply(args []string) error {
	text := strings.TrimSpace(strings.Join(args, " "))
	if text == "" {
		return fmt.Errorf("usage: rig reply MESSAGE...")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	basedir, err := findBasedir(cwd)
	if err != nil {
		return err
	}
	thread := readRigMessages(basedir)
	for i := len(thread) - 1; i >= 0; i-- {
		m := thread[i]
		if m.Dir != "in" || !m.Delivered {
			continue
		}
		rigID, ok := strings.CutPrefix(m.FromAddr, "rig:")
		if !ok || rigID == "" {
			return fmt.Errorf("last message came from %s, which isn't a rig address I can deliver a reply to", m.From)
		}
		target, err := resolveRig(rigID)
		if err != nil {
			return fmt.Errorf("the rig that messaged you (%s) is gone: %v", rigID, err)
		}
		return deliverRigMessage(target, text, m.ID, currentSender())
	}
	return fmt.Errorf("no inbound message to reply to")
}

func runMessages(args []string) error {
	jsonOut := false
	var query string
	for _, a := range args {
		switch a {
		case "--format=json":
			jsonOut = true
		case "--format=table":
			jsonOut = false
		default:
			if query != "" {
				return fmt.Errorf("usage: rig messages [RIG] [--format=json|table]")
			}
			query = a
		}
	}

	var r rigInfo
	if query != "" {
		var err error
		r, err = resolveRig(query)
		if err != nil {
			return err
		}
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		basedir, err := findBasedir(cwd)
		if err != nil {
			return fmt.Errorf("not in a rig; usage: rig messages RIG")
		}
		m, err := readManifest(basedir)
		if err != nil {
			return err
		}
		r = rigInfo{ID: m.ID, Path: basedir}
	}

	thread := readRigMessages(r.Path)
	if jsonOut {
		if thread == nil {
			thread = []rigMessage{}
		}
		blob, err := json.MarshalIndent(thread, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(blob))
		return nil
	}
	if len(thread) == 0 {
		fmt.Fprintf(os.Stderr, "rig: no messages for %s\n", r.ID)
		return nil
	}
	for _, m := range thread {
		mark := " "
		if !m.Delivered {
			mark = "✗"
		}
		fmt.Printf("%-12s %s %s → %s  %s\n", age(m.At)+" ago", mark, m.From, m.To, m.Text)
		if !m.Delivered {
			fmt.Printf("%-12s      %s\n", "", m.Error)
		}
	}
	return nil
}
