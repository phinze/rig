package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/phinze/rig/internal/mux"
	"github.com/phinze/rig/internal/mux/rex"
	"golang.org/x/term"
)

// preferredBackend is the multiplexer a *new* rig is built on, picked once in
// main from the preference ladder. It decides nothing about existing rigs:
// each records the backend that hosts it, and sessionFor reads that back. The
// commands that list across the whole machine (radar, switch, ls) ask every
// known backend rather than this one, because a Mac in the middle of the Rex
// trial has tmux rigs and Rex rigs side by side and the board has to show both.
var preferredBackend mux.Backend = localTmux()

// knownBackends is every backend this binary can drive from this machine, in
// the order a listing unions them: this machine's own, then the configured
// surfaces elsewhere (surface.go). Anything that joins what they list keys on
// Surface, since two of them can share a kind.
func knownBackends() []mux.Backend {
	return append(localBackends(), surfaceBackends()...)
}

// localBackends is one backend per kind on this machine, in the order `rig
// config` and a typo's suggestion print them. tmux is first because it's the
// default an empty name resolves to. Rex is included only where its CLI
// exists: a backend whose server is down lists nothing, which is the cheap and
// correct answer for a board, but a backend that isn't installed shouldn't
// cost a failed exec per listing on every Linux host. It's computed per call
// rather than at init so the Rex marks dir follows XDG_STATE_HOME, which tests
// pin after init.
func localBackends() []mux.Backend {
	list := []mux.Backend{localTmux()}
	if rex.Installed() {
		list = append(list, rex.Backend{MarksDir: rexMarksDir()})
	}
	return append(list, extraBackends...)
}

// extraBackends is where a test registers a fake, since one real multiplexer
// can't exercise the routing between two.
var extraBackends []mux.Backend

// rexMarksDir is where the Rex backend keeps its per-session mark sidecars:
// derived state, so under the state dir beside everything else rig writes
// down.
func rexMarksDir() string {
	dir, err := rigStateDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "rex-marks")
}

func backendNames() []string {
	backends := localBackends()
	names := make([]string, 0, len(backends))
	for _, b := range backends {
		names = append(names, b.Name())
	}
	return names
}

// backendByName resolves a preference to a backend. Unknown names are an
// error rather than a fallback, because a stale or misspelled preference that
// silently landed you in tmux would be indistinguishable from the setting
// never having taken. A name is a kind, and a kind always means this
// machine's own instance: a rig is built, recorded, and torn down where it
// lives, never on a surface elsewhere.
func backendByName(name string) (mux.Backend, error) {
	if name == "" {
		return localTmux(), nil
	}
	for _, b := range localBackends() {
		if b.Name() == name {
			return b, nil
		}
	}
	return nil, fmt.Errorf("unknown backend %q (expected one of %s)", name, strings.Join(backendNames(), ", "))
}

// backendNamed is backendByName for a name rig itself recorded (a manifest, a
// tombstone, a teardown job, a listed session) rather than one the user typed.
// A name this binary no longer knows falls back to tmux, since a rig with no
// multiplexer at all is worse than one looked for in the wrong place: that's
// the downgraded-binary case, not the typo case, which the config command
// refuses at write time.
func backendNamed(name string) mux.Backend {
	b, err := backendByName(name)
	if err != nil {
		return localTmux()
	}
	return b
}

// surfaceNamed is the backend a listed row came from, by the Surface it was
// tagged with. A surface no longer known falls back to the kind's local
// backend, the same downgrade backendNamed makes; for a local surface that is
// the right answer, and a remote one can only vanish between a scan and a
// keypress, where the attach that follows fails on its own.
func surfaceNamed(surface string) mux.Backend {
	for _, b := range knownBackends() {
		if b.Surface() == surface {
			return b
		}
		if sb, ok := b.(servedBackend); ok && sb.rex().Surface() == surface {
			return sb.rex()
		}
	}
	kind, _, _ := strings.Cut(surface, "@")
	return backendNamed(kind)
}

// defaultBackend applies the same ladder defaultAgent does, narrowest scope
// first: RIG_BACKEND for this shell, `rig config backend` for this user, then
// tmux. The env var sits above the file for the reason the agent one does —
// `RIG_BACKEND=rex rig new` has to mean something. Like the agent, it decides
// only for a new rig; the manifest records the answer and sessionFor reads it
// back.
func defaultBackend() (mux.Backend, error) {
	name, _, _ := defaultBackendWithSource()
	return backendByName(name)
}

// defaultBackendWithSource is defaultBackend plus where the answer came from,
// for `rig config` to name. The source labels are the agent's, since the
// ladder is the same and the table prints them the same way.
func defaultBackendWithSource() (name string, src agentSource, raw string) {
	if raw := os.Getenv("RIG_BACKEND"); raw != "" {
		return raw, agentFromEnv, raw
	}
	if raw := readRigConfig().Backend; raw != "" {
		return raw, agentFromConfig, raw
	}
	return "tmux", agentFromBuiltin, ""
}

// rigSession is one rig's session together with the multiplexer hosting it.
// The two travel as a pair because a session name alone is ambiguous on a
// machine with two multiplexers, so commands hold this rather than the bare
// string.
type rigSession struct {
	name string
	b    mux.Backend
	// show is how the session presents on a backend that keeps a display
	// label apart from the name (mux.SessionPresenter). It's empty for a
	// session built from a board row rather than a manifest, which never
	// relabels anything.
	show rigShow
}

// rigShow is what a rig's session displays where the backend lets it: the
// session labelled with the rig's kind glyph and id, the agent's window with
// the rig's title, and a chief of staff at the top of the list.
type rigShow struct {
	label string
	title string
	first bool
}

func showFor(m manifest) rigShow {
	kind := manifestKind(m)
	label := m.ID
	if g := rigKindGlyph(kind); g != "" {
		label = g + " " + label
	}
	return rigShow{label: label, title: m.Title, first: kind == rigKindCoS}
}

// sessionFor is the session a rig's basedir maps to, on the backend its
// manifest records. Empty means tmux, as it does for every rig made before the
// field existed.
func sessionFor(basedir string, m manifest) rigSession {
	rs := rigSession{name: rigSessionName(basedir), b: backendNamed(m.Backend)}
	if _, ok := rs.b.(mux.SessionPresenter); ok && m.ID != "" {
		rs.show = showFor(m)
	}
	return rs
}

// mainWindowLabel names the agent's window. A presenting backend lists
// windows under the session, so the rig's title says what the rig is for
// there. Elsewhere the name is main/<repo>, the repo whose Recto sits beside
// the agent.
func (rs rigSession) mainWindowLabel(repo string) string {
	if rs.show.title != "" {
		return rs.show.title
	}
	return mainWindowName(repo)
}

// present applies rs.show. It's cosmetic, so a failure is dropped rather than
// failing whatever built or repaired the session.
func (rs rigSession) present() {
	p, ok := rs.b.(mux.SessionPresenter)
	if !ok || rs.show.label == "" {
		return
	}
	_ = p.PresentSession(rs.name, rs.show.label, rs.show.first)
}

// sessionKey is a session's identity on a board that lists more than one
// surface. The name alone isn't one: a rig at ~/workspaces/foo slugs the same
// on every host, so a remote session could otherwise hide a local rig's row
// or lend it its agents.
type sessionKey struct {
	surface string
	name    string
}

func (rs rigSession) key() sessionKey { return sessionKey{rs.b.Surface(), rs.name} }

// rigKey is the key a rig's own session has. A rig is always on this machine,
// so its surface is the local instance of the kind its manifest records.
func rigKey(path, backend string) sessionKey {
	return sessionKey{backendNamed(backend).Surface(), rigSessionName(path)}
}

func (rs rigSession) live() bool { return rs.b.HasSession(rs.name) }

func (rs rigSession) attach() error { return attachOrReport(rs.b, rs.name) }

func (rs rigSession) panes() ([]mux.Pane, error) { return rs.b.Panes(rs.name) }

// rigSessionName is the session a rig's basedir maps to. It's mux.SessionName
// under a name that says what rig uses it for.
func rigSessionName(path string) string {
	return mux.SessionName(path)
}

// currentSession is the session the current process runs inside and the
// backend it belongs to, or nil and "" from a bare terminal. Each backend
// answers from its own environment, so at most one says anything.
func currentSession() (mux.Backend, string) {
	for _, b := range knownBackends() {
		if name := b.CurrentSession(); name != "" {
			return b, name
		}
	}
	return nil, ""
}

// currentKey is currentSession as a sessionKey, zero from a bare terminal.
func currentKey() sessionKey {
	b, name := currentSession()
	if b == nil {
		return sessionKey{}
	}
	return sessionKey{b.Surface(), name}
}

// insideSession reports whether the current process is running inside the
// given rig session. False when not inside any multiplexer.
func insideSession(rs rigSession) bool {
	b, name := currentSession()
	return b != nil && b.Surface() == rs.b.Surface() && name == rs.name
}

// allSessions lists every live session across the given backends, each
// tagged with the backend that listed it.
func allSessions(backends []mux.Backend) []mux.Session {
	return slices.Concat(eachBackend(backends, func(b mux.Backend) []mux.Session { return b.Sessions() })...)
}

// boardListing is allSessions and every pane together, one pass per backend,
// for the boards that draw both. A backend that can answer the two at once
// does (mux.Snapshotter); the rest run the two listings side by side, as the
// separate passes did.
func boardListing(backends []mux.Backend) ([]mux.Session, []mux.Pane) {
	type listing struct {
		sessions []mux.Session
		panes    []mux.Pane
	}
	per := eachBackend(backends, func(b mux.Backend) listing {
		if s, ok := b.(mux.Snapshotter); ok {
			sessions, panes := s.Snapshot()
			return listing{sessions, panes}
		}
		var l listing
		var wg sync.WaitGroup
		wg.Go(func() { l.sessions = b.Sessions() })
		wg.Go(func() { l.panes, _ = b.AllPanes() })
		wg.Wait()
		return l
	})
	var sessions []mux.Session
	var panes []mux.Pane
	for _, l := range per {
		sessions = append(sessions, l.sessions...)
		panes = append(panes, l.panes...)
	}
	return sessions, panes
}

// liveness answers rigSession.live for a batch of rigs. A backend that can
// list its session names (mux.SessionNamer) is listed once on first ask;
// anything else is asked per rig, which for tmux is a cheap has-session.
type liveness map[string]map[string]bool // surface → live session names

func (l liveness) live(rs rigSession) bool {
	n, ok := rs.b.(mux.SessionNamer)
	if !ok {
		return rs.live()
	}
	set, seen := l[rs.b.Surface()]
	if !seen {
		set = map[string]bool{}
		names, _ := n.SessionNames()
		for _, name := range names {
			set[name] = true
		}
		l[rs.b.Surface()] = set
	}
	return set[rs.name]
}

// eachBackend runs one listing against every given backend at once and
// returns the answers in backend order. Concurrently because a surface on
// another host costs a network round trip per call and one that has dropped
// off costs its whole timeout; in sequence those add up, and the local rows a
// board refreshes every two seconds would wait on all of them.
func eachBackend[T any](backends []mux.Backend, list func(mux.Backend) T) []T {
	out := make([]T, len(backends))
	var wg sync.WaitGroup
	for i, b := range backends {
		wg.Go(func() { out[i] = list(b) })
	}
	wg.Wait()
	return out
}

// lastAttached maps each live local session to the unix time it was last
// attached (0 if never). It's how `rig switch` sorts most-recently-touched
// first, the same signal session-wizard's `t` sorts on. Local only, because
// switch lists rigs and a rig is always local: asking the surfaces elsewhere
// would only add their latency.
func lastAttached() map[sessionKey]int64 {
	return attachedTimes(allSessions(localBackends()))
}

func attachedTimes(sessions []mux.Session) map[sessionKey]int64 {
	m := make(map[sessionKey]int64, len(sessions))
	for _, s := range sessions {
		m[sessionKey{s.Surface, s.Name}] = s.LastAttached
	}
	return m
}

// attachOrReport attaches to the target on the given backend when stdin is a
// tty, otherwise prints how to attach manually (e.g. when invoked from a
// script or test). A backend that can't switch the client says so and
// succeeds: the session is ready, and the last step is yours.
func attachOrReport(b mux.Backend, target string) error {
	// Forwarded from another host, there's no screen here to attach: hand the
	// session back to the host that asked, which enters it from its side.
	if reportAttach(b.Name(), target) {
		return nil
	}
	if !stdinIsTTY() {
		fmt.Fprintf(os.Stderr, "rig: not a tty — session ready as %q, attach manually\n", target)
		return nil
	}
	if err := b.Attach(target); err != nil {
		if errors.Is(err, mux.ErrNoClientSwitch) {
			fmt.Fprintf(os.Stderr, "rig: session %q is ready, but %v — switch to it by hand\n", target, err)
			return nil
		}
		return err
	}
	return nil
}

// agentChild is one agent-bearing window: what the radar dangles under a rig
// or session so the board reads as a HUD of every agent in flight. Window is
// the window's name (a repo, for a rig), Target is what a jump hands to
// Attach, and Context is the task the agent named for itself (empty when it's
// still on the "Claude Code" placeholder).
type agentChild struct {
	Surface string
	Session string
	Window  string
	Target  string
	Context string
	Working bool // see paneWorking
	// Status is the agent's own report of its state, zero where the backend
	// carries none (tmux) or the agent never reported.
	Status mux.ProgramStatus
}

// liveAgentChildren is the radar's one sweep of every pane on the given
// backends, filtered down to the agents. A backend that can't be listed
// contributes nothing rather than failing the sweep.
func liveAgentChildren(backends []mux.Backend) map[sessionKey][]agentChild {
	panes := slices.Concat(eachBackend(backends, func(b mux.Backend) []mux.Pane {
		ps, _ := b.AllPanes()
		return ps
	})...)
	return agentChildren(panes, time.Now().Unix())
}

// agentChildren maps each session to its agent panes, in pane order. Each agent
// pane becomes a child, except that panes sharing a window and the exact same
// context collapse to one — that kills the artifact of the same agent mirrored
// across a split without hiding two genuinely different agents side by side. A
// pane is an agent when its command is recognized or its title still wears
// Claude Code's state glyph (so a wrapped agent is still caught). Some agents
// set the title to cwd's basename instead of naming the task; that repeats the
// repo and is treated like a placeholder so a rig can fall back to its durable
// title. The working signal is the agent's own status where it reports one,
// and otherwise when the window last produced output, which idles off after a
// few quiet minutes. The animating title glyph is never the signal, which is
// why it carries only the task text. now is the current unix time.
func agentChildren(panes []mux.Pane, now int64) map[sessionKey][]agentChild {
	activeWithin := int64(agentActiveWindow / time.Second)
	children := map[sessionKey][]agentChild{}
	seen := map[string]bool{} // surface\tsession\twindow\tcontext — collapse exact dups
	for _, p := range panes {
		if !isAgentPane(p) {
			continue
		}
		ctx := stripAgentGlyph(p.Title)
		if isAgentPlaceholder(ctx) || (p.Path != "" && ctx == filepath.Base(p.Path)) {
			ctx = ""
		}
		key := p.Surface + "\t" + p.Session + "\t" + p.WindowIdx + "\t" + ctx
		if seen[key] {
			continue
		}
		seen[key] = true
		sk := sessionKey{p.Surface, p.Session}
		children[sk] = append(children[sk], agentChild{
			Surface: p.Surface,
			Session: p.Session,
			Window:  p.WindowName,
			Target:  p.Target,
			Context: ctx,
			Working: paneWorking(p, now, activeWithin),
			Status:  p.Status,
		})
	}
	return children
}

// paneWorking trusts an agent's own report when it made one, since only the
// agent can tell a finished turn from a quiet one. Without one, it's whether
// the window produced output within the last activeWithin seconds.
func paneWorking(p mux.Pane, now, activeWithin int64) bool {
	if p.Status.State != "" {
		return p.Status.State == "working"
	}
	return p.Activity > 0 && now-p.Activity < activeWithin
}

// isAgentPane is agentChildren's test for whether a pane holds an agent: a
// recognized command, or a title still wearing Claude Code's state glyph (so a
// wrapped agent is still caught).
func isAgentPane(p mux.Pane) bool {
	return isAgentCommand(p.Command) || stripAgentGlyph(p.Title) != p.Title
}

// isAgentCommand recognises an agent by its foreground process name. Nix
// wraps binaries as `.<name>-wrapped` around `.<name>-unwrapped`, and Rex
// reports the real process where tmux reports the wrapper, so both spellings
// are folded back to the name.
func isAgentCommand(cmd string) bool {
	cmd = strings.TrimPrefix(cmd, ".")
	cmd = strings.TrimSuffix(strings.TrimSuffix(cmd, "-unwrapped"), "-wrapped")
	return cmd == "claude" || strings.HasPrefix(cmd, "codex") || cmd == "agy" || strings.HasPrefix(cmd, "antigravity") || cmd == "pi"
}

// shellQuote wraps s for safe inclusion as a single shell argument when
// typed via SendKeys (where the receiving shell will reparse the line).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// stdinIsTTY reports whether stdin is connected to a real terminal.
// Note: a char-device check alone returns true for /dev/null, so use the
// proper termios probe via x/term.
func stdinIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
