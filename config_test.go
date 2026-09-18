package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRigConfigRoundTrip(t *testing.T) {
	isolateRigConfig(t)

	if c := readRigConfig(); c.Agent != "" {
		t.Errorf("fresh config = %+v, want empty", c)
	}
	if err := writeRigConfig(rigConfig{Agent: "codex"}); err != nil {
		t.Fatal(err)
	}
	if c := readRigConfig(); c.Agent != "codex" {
		t.Errorf("agent = %q, want codex", c.Agent)
	}

	// A hand-edited file should cost you the line you broke, not the command.
	path, err := rigConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# note\nnonsense\nagent = \"agy\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := readRigConfig(); c.Agent != "agy" {
		t.Errorf("agent = %q after a junk line, want agy", c.Agent)
	}
}

// Ordinary precedence, narrowest scope first: --agent for this invocation,
// RIG_AGENT for this shell, the config file for this user, claude beneath all
// three. Env stays above the file so `RIG_AGENT=cdx rig new` is a real one-off
// rather than a silent no-op.
func TestDefaultAgentPrecedence(t *testing.T) {
	isolateRigConfig(t)

	if err := writeRigConfig(rigConfig{Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	kind, src, _ := defaultAgentWithSource()
	if kind != agentClaude || src != agentFromConfig {
		t.Errorf("with only the file: %q from %q, want claude from config", kind, src)
	}

	t.Setenv("RIG_AGENT", "codex")
	kind, src, _ = defaultAgentWithSource()
	if kind != agentCodex || src != agentFromEnv {
		t.Errorf("with both set: %q from %q, want codex from env", kind, src)
	}

	pick, _, err := extractAgentFlag([]string{"--agent", "agy"})
	if err != nil {
		t.Fatal(err)
	}
	if pick.kind != agentAntigravity {
		t.Errorf("--agent = %q, want antigravity", pick.kind)
	}

	t.Setenv("RIG_AGENT", "")
	if err := writeRigConfig(rigConfig{}); err != nil {
		t.Fatal(err)
	}
	kind, src, _ = defaultAgentWithSource()
	if kind != agentClaude || src != agentFromBuiltin {
		t.Errorf("with nothing set: %q from %q, want claude from built-in", kind, src)
	}
}

// A typo in a file you edited by hand must not make rig unusable, so an
// unparseable setting falls through to the next source instead of erroring.
func TestDefaultAgentIgnoresGarbage(t *testing.T) {
	isolateRigConfig(t)

	if err := writeRigConfig(rigConfig{Agent: "cdx"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_AGENT", "gemini")
	if kind, src, _ := defaultAgentWithSource(); kind != agentCodex || src != agentFromConfig {
		t.Errorf("garbage env: %q from %q, want codex from the file", kind, src)
	}

	if err := writeRigConfig(rigConfig{Agent: "nonsense"}); err != nil {
		t.Fatal(err)
	}
	if kind, src, _ := defaultAgentWithSource(); kind != agentClaude || src != agentFromBuiltin {
		t.Errorf("garbage everywhere: %q from %q, want claude from built-in", kind, src)
	}
}

// The whole point of separating parseAgent from defaultAgent: an existing rig
// records an empty agent to mean Claude, so a standing preference must never
// reach back and relabel one.
func TestStandingPreferenceDoesNotRelabelExistingRigs(t *testing.T) {
	isolateRigConfig(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // this test records a real tombstone
	if err := writeRigConfig(rigConfig{Agent: "codex"}); err != nil {
		t.Fatal(err)
	}

	if got, err := parseAgent(""); got != agentClaude || err != nil {
		t.Errorf("parseAgent(\"\") = %q, %v; want claude", got, err)
	}

	dir := t.TempDir()
	if err := writeManifest(dir, manifest{ID: "mir-1"}); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.agentKind() != agentClaude {
		t.Errorf("existing rig = %q, want claude", m.agentKind())
	}

	// Tombstones take the same path, and getting it wrong would resurrect a
	// Claude rig into the wrong agent a week after it was torn down.
	if err := recordTombstone(dir, m, nil); err != nil {
		t.Fatal(err)
	}
	stone, err := readTombstoneFor(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if stone.Agent != string(agentClaude) {
		t.Errorf("tombstone agent = %q, want claude", stone.Agent)
	}
}

func readTombstoneFor(t *testing.T, basedir string) (tombstone, error) {
	t.Helper()
	dir, err := tombstoneDir()
	if err != nil {
		return tombstone{}, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return tombstone{}, err
	}
	for _, e := range entries {
		st, err := readTombstone(filepath.Join(dir, e.Name()))
		if err == nil && resolvePath(st.Basedir) == resolvePath(basedir) {
			return *st, nil
		}
	}
	t.Fatalf("no tombstone recorded for %s", basedir)
	return tombstone{}, nil
}

func TestConfigCmdSetsReadsAndUnsets(t *testing.T) {
	isolateRigConfig(t)

	if err := runConfigCmd([]string{"agent", "cdx"}); err != nil {
		t.Fatal(err)
	}
	if got := readRigConfig().Agent; got != "codex" {
		// The canonical long name is stored, not whatever shorthand was typed.
		t.Errorf("stored agent = %q, want codex", got)
	}
	if defaultAgent() != agentCodex {
		t.Errorf("default = %q, want codex", defaultAgent())
	}

	if err := runConfigCmd([]string{"agent", "--unset"}); err != nil {
		t.Fatal(err)
	}
	if got := readRigConfig().Agent; got != "" {
		t.Errorf("agent = %q after unset, want empty", got)
	}
	if defaultAgent() != agentClaude {
		t.Errorf("default after unset = %q, want claude", defaultAgent())
	}

	if err := runConfigCmd([]string{"agent", "gemini"}); err == nil {
		t.Error("an unknown agent should be rejected at write time")
	}
	if err := runConfigCmd([]string{"nonsense"}); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Errorf("unknown setting error = %v, want it to name the real settings", err)
	}
	if err := runConfigCmd([]string{"agent", "cld", "--unset"}); err == nil {
		t.Error("--unset with a value should be rejected")
	}
}

func TestConfigListNamesItsSource(t *testing.T) {
	isolateRigConfig(t)

	agent := configSettings[0]
	if err := writeRigConfig(rigConfig{Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	value, origin := agent.effective()
	if value != "claude" || !strings.Contains(origin, "config.toml") {
		t.Errorf("effective = %q from %q, want claude from the config file", value, origin)
	}
	if note := agent.note(); note != "" {
		t.Errorf("note = %q; nothing is being shadowed yet", note)
	}

	// The setting you just wrote losing to an env var you set months ago is the
	// one confusing outcome here, so it gets said out loud along with the fix.
	t.Setenv("RIG_AGENT", "codex")
	value, origin = agent.effective()
	if value != "codex" || origin != "RIG_AGENT" {
		t.Errorf("effective = %q from %q, want codex from RIG_AGENT", value, origin)
	}
	note := agent.note()
	if !strings.Contains(note, "claude") || !strings.Contains(note, "RIG_AGENT=codex") {
		t.Errorf("note = %q, want it to name both the shadowed file value and the winner", note)
	}
	if !strings.Contains(note, "Unset RIG_AGENT") {
		t.Errorf("note = %q, want it to name the fix", note)
	}
}

func TestConfigBackendFollowsTheAgentLadder(t *testing.T) {
	isolateRigConfig(t)

	if err := runConfigCmd([]string{"backend", "tmux"}); err != nil {
		t.Fatal(err)
	}
	if got := readRigConfig().Backend; got != "tmux" {
		t.Errorf("stored backend = %q, want tmux", got)
	}
	// Both settings share the file, so writing one must not drop the other.
	if err := runConfigCmd([]string{"agent", "cdx"}); err != nil {
		t.Fatal(err)
	}
	if c := readRigConfig(); c.Backend != "tmux" || c.Agent != "codex" {
		t.Errorf("config = %+v, want both settings kept", c)
	}

	// An unknown backend is refused at write time, and refused again at
	// startup if it arrives through the environment, because a preference
	// that silently fell back to tmux would be indistinguishable from one
	// that never took.
	if err := runConfigCmd([]string{"backend", "screen"}); err == nil || !strings.Contains(err.Error(), "tmux") {
		t.Errorf("unknown backend error = %v, want it to name the known ones", err)
	}
	t.Setenv("RIG_BACKEND", "screen")
	if _, err := defaultBackend(); err == nil {
		t.Error("RIG_BACKEND=screen should fail to resolve rather than fall back")
	}
	name, src, _ := defaultBackendWithSource()
	if name != "screen" || src != agentFromEnv {
		t.Errorf("source = %q from %q, want the env var to win over the file", name, src)
	}

	t.Setenv("RIG_BACKEND", "")
	if err := runConfigCmd([]string{"backend", "--unset"}); err != nil {
		t.Fatal(err)
	}
	name, src, _ = defaultBackendWithSource()
	if name != "tmux" || src != agentFromBuiltin {
		t.Errorf("after unset = %q from %q, want the built-in tmux", name, src)
	}
	if _, err := defaultBackend(); err != nil {
		t.Errorf("built-in default should resolve: %v", err)
	}
}
