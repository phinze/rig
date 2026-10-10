package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/phinze/rig/internal/mux"
)

// Cross-host verbs work by forwarding, not by reimplementation. A rig lives
// where its multiplexer runs, so the host that owns it is the only one that
// can probe its agent, park its session, or tear down its basedir; the verb
// that does each of those already exists there. So `rig park foo@foxtrotbase`
// asks foxtrotbase's serve to run `rig park foo` and streams back what it
// said, exit code and all, and every verb keeps exactly one implementation.
//
// The trust boundary this opens is the serve gate's allow list, which is the
// same human on their own nodes; see CLAUDE.md. What narrows it is the verb
// list below, and the fact that a forwarded command resolves locally only
// (localOnly), so a miss on the far side can never bounce back through that
// host's own surfaces.

// loopbackPlace is the identity a loopback listener stamps, since there's no
// tailnet to ask who a local test is.
const loopbackPlace = "loopback"

// forwardableVerbs is every command another host may run here. Anything that
// configures this machine (config, serve, reap), draws on its screen (radar,
// switch, recto), or exists for other processes (env, info, the __ internals)
// is absent on purpose: those answer for the host you're sitting at.
var forwardableVerbs = map[string]bool{
	"send": true, "messages": true,
	"ls": true, "waiting": true, "history": true, "sweep": true,
	"dispatch": true, "wake": true, "resume": true, "park": true,
	"down": true, "resurrect": true,
	"up": true, "new": true, "review": true, "project": true, "cos": true,
	"notify": true,
}

func forwardable(argv []string) error {
	if len(argv) == 0 {
		return errors.New("no command")
	}
	if !forwardableVerbs[argv[0]] {
		return fmt.Errorf("%q can't be run from another host", argv[0])
	}
	// The inbox is read-only from elsewhere: posting belongs to the tools on
	// this machine that watch it.
	if argv[0] == "notify" && (len(argv) < 2 || argv[1] != "list") {
		return errors.New("only `notify list` can be run from another host")
	}
	return nil
}

// The forwarded command learns it was forwarded, and on whose behalf, from its
// environment. captureRemoteEnv moves those variables into these globals and
// unsets them before anything runs, because a forwarded `wake` or `up` may
// start a tmux server, and a tmux server hands its own environment to every
// session it ever makes: a leaked RIG_LOCAL_ONLY would cut every agent on the
// host off from the others, and a leaked sender would sign their messages.
var (
	// localOnly says this process was forwarded here: resolve rigs on this
	// host and nowhere else.
	localOnly bool
	// forwardedSender is who a forwarded send speaks for, already stamped
	// with the host the tailnet says it came from. Nil when not forwarded.
	forwardedSender *rigSender
	// forwardedAttach is where a forwarded command reports the session it
	// would have attached, for the asking host to enter instead.
	forwardedAttach *os.File
	// forwardedMessage carries the id the sending host minted, so both halves
	// of a cross-host send log the same one, and the id a reply answers.
	forwardedMessage struct{ id, replyTo string }
)

const (
	envLocalOnly    = "RIG_LOCAL_ONLY"
	envSenderAddr   = "RIG_FORWARDED_SENDER"
	envSenderName   = "RIG_FORWARDED_SENDER_NAME"
	envMessageID    = "RIG_FORWARDED_MESSAGE_ID"
	envReplyTo      = "RIG_FORWARDED_REPLY_TO"
	envAttachFD     = "RIG_FORWARDED_ATTACH_FD"
	forwardedPrefix = "RIG_FORWARDED_"
)

func captureRemoteEnv() {
	localOnly = os.Getenv(envLocalOnly) == "1"
	if addr := os.Getenv(envSenderAddr); addr != "" {
		forwardedSender = &rigSender{name: os.Getenv(envSenderName), addr: addr}
	}
	forwardedMessage.id = os.Getenv(envMessageID)
	forwardedMessage.replyTo = os.Getenv(envReplyTo)
	if fd, err := strconv.Atoi(os.Getenv(envAttachFD)); err == nil && fd > 2 {
		forwardedAttach = os.NewFile(uintptr(fd), "attach")
		syscall.CloseOnExec(fd) // nothing this command starts should inherit it
	}
	for _, k := range []string{envLocalOnly, envSenderAddr, envSenderName, envMessageID, envReplyTo, envAttachFD} {
		_ = os.Unsetenv(k)
	}
}

// execRequest asks a host to run one rig command. From and FromName are the
// caller's claim about who's speaking; the host keeps the rig id and replaces
// the place with the tailnet's answer.
type execRequest struct {
	Argv      []string `json:"argv"`
	Stdin     []byte   `json:"stdin,omitempty"`
	From      string   `json:"from,omitempty"`
	FromName  string   `json:"fromName,omitempty"`
	MessageID string   `json:"messageId,omitempty"`
	ReplyTo   string   `json:"replyTo,omitempty"`
}

// attachFrame names a session a forwarded command made ready: its backend
// kind on that host and the target Attach takes.
type attachFrame struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

// reportAttach tells the asking host which session to enter, when this
// command was forwarded. It reports whether it did.
func reportAttach(kind, target string) bool {
	if forwardedAttach == nil {
		return false
	}
	line, _ := json.Marshal(attachFrame{Kind: kind, Target: target})
	_, err := forwardedAttach.Write(append(line, '\n'))
	return err == nil
}

// execFrame is one NDJSON line of the answer: a chunk of stdout or stderr, or
// the closing frame with the exit code. A stream that ends without one means
// the connection dropped, not that the command finished.
type execFrame struct {
	Out    string       `json:"out,omitempty"`
	Err    string       `json:"err,omitempty"`
	Exit   *int         `json:"exit,omitempty"`
	Attach *attachFrame `json:"attach,omitempty"`
	Error  string       `json:"error,omitempty"` // the command couldn't be started
}

// stampSender turns a caller's claimed address into the one this host
// records. The rig id is the caller's to say; the place is the tailnet's, so
// whatever place the claim carried is dropped. A caller that claimed nothing
// is a person at a shell over there.
func stampSender(claimAddr, claimName, node string) rigSender {
	addr, _, _ := strings.Cut(claimAddr, "@")
	name, _, _ := strings.Cut(claimName, "@")
	if addr == "" {
		addr, name = "cli:unknown", "unknown"
	}
	if name == "" {
		_, name, _ = strings.Cut(addr, ":")
	}
	return rigSender{name: name + "@" + node, addr: addr + "@" + node}
}

// rigExecutable is the binary a forwarded command runs as: this one, behind a
// variable so a test can point it at a stand-in.
var rigExecutable = os.Executable

func serveExec(w http.ResponseWriter, r *http.Request, peer whoisIdentity) {
	var req execRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "unreadable request", http.StatusBadRequest)
		return
	}
	if err := forwardable(req.Argv); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	bin, err := rigExecutable()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sender := stampSender(req.From, req.FromName, peer.Node)

	// From home, like a shell would start, so no verb resolves "the rig I'm
	// in" from wherever serve happened to be launched. A service whose home
	// doesn't exist gets / rather than a command that can't start.
	cmd := exec.Command(bin, req.Argv...)
	cmd.Dir = "/"
	if home, err := os.UserHomeDir(); err == nil && dirExists(home) {
		cmd.Dir = home
	}
	cmd.Stdin = bytes.NewReader(req.Stdin)
	cmd.Env = append(scrubForwardedEnv(os.Environ()),
		envLocalOnly+"=1",
		envSenderAddr+"="+sender.addr,
		envSenderName+"="+sender.name,
		envMessageID+"="+req.MessageID,
		envReplyTo+"="+req.ReplyTo,
	)

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fw := &frameWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		fw.flush = f.Flush
	}
	cmd.Stdout = fw.stream(func(s string) execFrame { return execFrame{Out: s} })
	cmd.Stderr = fw.stream(func(s string) execFrame { return execFrame{Err: s} })

	// fd 3 is the command's side channel for the session it would have
	// attached (reportAttach). It's a pipe rather than a marker on stdout so
	// no output a command prints can be mistaken for one.
	attachR, attachW, err := os.Pipe()
	if err != nil {
		fw.send(execFrame{Error: err.Error()})
		return
	}
	cmd.ExtraFiles = []*os.File{attachW}
	cmd.Env = append(cmd.Env, envAttachFD+"=3")
	attachDone := make(chan struct{})
	go func() {
		defer close(attachDone)
		sc := bufio.NewScanner(attachR)
		for sc.Scan() {
			var a attachFrame
			if json.Unmarshal(sc.Bytes(), &a) == nil && a.Target != "" {
				fw.send(execFrame{Attach: &a})
			}
		}
	}()

	// Verb and target only: the rest is often a message body, which has no
	// business in a service log.
	fmt.Fprintf(os.Stderr, "rig serve: %s runs %s\n", sender.addr, strings.Join(req.Argv[:min(2, len(req.Argv))], " "))
	// The command runs to the end even if the caller goes away: a lid closing
	// on the laptop shouldn't strand a teardown halfway. Its output just stops
	// having anywhere to go.
	code := 0
	err = cmd.Start()
	_ = attachW.Close() // the child holds its own copy
	if err == nil {
		err = cmd.Wait()
	}
	<-attachDone
	_ = attachR.Close()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			fw.send(execFrame{Error: err.Error()})
			code = 127
		} else {
			code = exitErr.ExitCode()
		}
	}
	fw.send(execFrame{Exit: &code})
}

func scrubForwardedEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, forwardedPrefix) || strings.HasPrefix(kv, envLocalOnly+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// frameWriter serializes the child's two streams onto one response. Write
// errors are swallowed on purpose: a caller that hung up must not turn into
// an EPIPE that kills the command it asked for.
type frameWriter struct {
	mu    sync.Mutex
	w     io.Writer
	flush func()
	dead  bool
}

func (fw *frameWriter) send(f execFrame) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.dead {
		return
	}
	line, _ := json.Marshal(f)
	if _, err := fw.w.Write(append(line, '\n')); err != nil {
		fw.dead = true
		return
	}
	if fw.flush != nil {
		fw.flush()
	}
}

type frameStream struct {
	fw   *frameWriter
	wrap func(string) execFrame
}

func (fw *frameWriter) stream(wrap func(string) execFrame) io.Writer {
	return frameStream{fw, wrap}
}

func (s frameStream) Write(p []byte) (int, error) {
	s.fw.send(s.wrap(string(p)))
	return len(p), nil
}

// remoteExit is a forwarded command that ran and failed. The far side already
// said why on its stderr, which streamed here, so main exits with its code and
// adds nothing.
type remoteExit struct {
	place string
	code  int
}

func (e remoteExit) Error() string {
	return fmt.Sprintf("exited %d on %s", e.code, e.place)
}

// execClient has no overall deadline because an `up` fetches repos and a
// `down` deletes trees; it bounds only the parts that say a host is gone.
var execClient = &http.Client{Transport: &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	ResponseHeaderTimeout: 15 * time.Second,
}}

// exec runs argv on this surface's host, streaming its output to stdout and
// stderr. A non-zero exit comes back as remoteExit. It doesn't consult the
// listing breaker: someone asked for this by name, and a failed dial says so
// in its own words.
func (b servedBackend) exec(req execRequest, stdout, stderr io.Writer) error {
	_, err := b.execAttach(req, stdout, stderr)
	return err
}

// execAttach is exec that also returns the last session the command made
// ready, which a caller that would have attached locally now enters.
func (b servedBackend) execAttach(req execRequest, stdout, stderr io.Writer) (*attachFrame, error) {
	place := surfacePlace(b.Surface())
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := execClient.Post(b.URL+"/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("reaching %s: %w", place, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s's rig serve predates forwarding; update rig there and restart it", place)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("%s refused: %s", place, strings.TrimSpace(string(msg)))
	}
	var attach *attachFrame
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		var f execFrame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			return nil, fmt.Errorf("%s: unreadable answer: %w", place, err)
		}
		switch {
		case f.Out != "":
			_, _ = io.WriteString(stdout, f.Out)
		case f.Err != "":
			_, _ = io.WriteString(stderr, f.Err)
		case f.Error != "":
			_, _ = fmt.Fprintf(stderr, "rig: %s couldn't run it: %s\n", place, f.Error)
		case f.Attach != nil:
			attach = f.Attach
		case f.Exit != nil:
			if *f.Exit != 0 {
				return attach, remoteExit{place: place, code: *f.Exit}
			}
			return attach, nil
		}
	}
	return nil, fmt.Errorf("lost %s before it finished; the command may still be running there", place)
}

// execCapture runs argv on the host and returns its stdout, for verbs that
// merge another host's answer into their own (ls, waiting, history). Its
// stderr still streams, since that's where a far-side failure explains itself.
func (b servedBackend) execCapture(argv ...string) ([]byte, error) {
	var out bytes.Buffer
	err := b.exec(execRequest{Argv: argv}, &out, os.Stderr)
	return out.Bytes(), err
}

// forward runs a verb on the host that owns a remote target, as the person or
// rig here, and passes its output straight through.
// When the verb would have attached a session there, this side enters it
// instead, through the surface that lists it; see reportAttach.
func forward(t remoteTarget, argv ...string) error {
	return forwardTo(t.host, nil, argv...)
}

func forwardTo(host servedBackend, stdin []byte, argv ...string) error {
	sender := currentSender()
	attach, err := host.execAttach(execRequest{Argv: argv, Stdin: stdin, From: sender.addr, FromName: sender.name}, os.Stdout, os.Stderr)
	if err != nil || attach == nil {
		return err
	}
	var b mux.Backend
	switch attach.Kind {
	case "tmux":
		b = host
	case "rex":
		b = host.rex()
	default:
		return fmt.Errorf("%s made %s ready in %s, which nothing here can enter", surfacePlace(host.Surface()), attach.Target, attach.Kind)
	}
	return attachOrReport(b, attach.Target)
}

// runOn makes a rig on another host: `rig up MIR-12 --on foxtrotbase` runs
// `rig up MIR-12` there and then enters what it built from here, through the
// session it reports. The far side has no terminal, so it takes its scriptable
// path: what a picker or the kickoff wizard would have asked has to be in the
// arguments (an exact id, --repo, a kickoff), and the far side says which
// one's missing. Piped context travels as the far command's stdin, read here
// only when nothing on the command line already supplied it, for the same
// never-closed-pipe reason resolveUpContext gives.
func runOn(verb, place string, args []string) error {
	if localOnly {
		return fmt.Errorf("--on can't be forwarded again")
	}
	if place == thisPlace() {
		return fmt.Errorf("--on %s is this host; drop the flag", place)
	}
	host, ok := servedAt(place)
	if !ok {
		return fmt.Errorf("no rig surface for %s (add it to [surfaces] as rig+http://%s)", place, place)
	}
	var stdin []byte
	if ctx, _ := extractValueFlag(args, "--context"); ctx == "" && !stdinIsTTY() {
		blob, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading context from stdin: %w", err)
		}
		stdin = blob
	}
	return forwardTo(host, stdin, append([]string{verb}, args...)...)
}
