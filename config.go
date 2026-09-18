package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// Rig's persistent state — manifests, tombstones, the notify inbox — is all
// derived: something rig did, written down so a later command can read it back.
// Settings are the opposite kind of thing. They're statements you make about
// how rig should behave, they only change when you say so, and losing the file
// costs you a preference rather than a rig. That's the XDG split, so settings
// live under the config dir and not beside the state.
//
// The namespace exists ahead of its second key on purpose. Rig's top-level
// commands are the verbs of the workflow — up, park, sweep, down — and a bare
// `rig agent` would be the first noun among them, spending a name in the prefix
// namespace for each preference that ever gets one. One `config` covers all of
// them, and the shape (`rig config KEY [VALUE]`) is git's, which is already in
// everyone's fingers.
const configName = "config.toml"

// rigConfig is the whole settable surface. Add a field here, add its entry to
// configSettings, and both the reader and `rig config` pick it up.
type rigConfig struct {
	Agent   string
	Backend string
}

func rigConfigDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "rig"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "rig"), nil
}

func rigConfigPath() (string, error) {
	dir, err := rigConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configName), nil
}

// readRigConfig returns the stored settings. A missing file is the normal case,
// not an error: rig has to run for someone who has never set anything. A
// malformed line is skipped rather than fatal, for the same reason the manifest
// parser is forgiving — a hand-edited config should cost you the line you broke,
// not every command.
func readRigConfig() rigConfig {
	path, err := rigConfigPath()
	if err != nil {
		return rigConfig{}
	}
	f, err := os.Open(path)
	if err != nil {
		return rigConfig{}
	}
	defer f.Close()

	var c rigConfig
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "agent":
			c.Agent = parseTOMLString(val)
		case "backend":
			c.Backend = parseTOMLString(val)
		}
	}
	return c
}

func writeRigConfig(c rigConfig) error {
	path, err := rigConfigPath()
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# rig settings. `rig config` reads and writes this file.\n")
	if c.Agent != "" {
		fmt.Fprintf(&b, "agent = %q\n", c.Agent)
	}
	if c.Backend != "" {
		fmt.Fprintf(&b, "backend = %q\n", c.Backend)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(temporary, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

// agentSource names where a new rig's starting agent came from. It exists so
// every place that reports the default can say why, which is the whole answer
// to "wait, why is everything starting on codex" — a question that otherwise
// costs you a grep through your shell config.
type agentSource string

const (
	agentFromConfig  agentSource = "config"
	agentFromEnv     agentSource = "env"
	agentFromBuiltin agentSource = "built-in"
)

// defaultAgent is the agent a *new* rig starts on, and the only thing that
// consults a standing preference. It is deliberately separate from parseAgent,
// which stays a pure parser: an existing rig records an empty agent to mean
// Claude (see manifest.agentKind), so folding the default into the parser would
// make changing your preference retroactively relabel every rig and tombstone
// created before the field existed.
//
// Precedence is the ordinary one, narrowest scope first: --agent for this
// invocation, RIG_AGENT for this shell, the config file for this user, Claude
// beneath all three. Inverting the middle two was considered — RIG_AGENT is a
// standing preference in practice, since it's set once in home-manager — but
// that's a fact about one person's shell config rather than about the
// mechanism, and it would have made `RIG_AGENT=cdx rig new` a silent no-op.
// Leaving env on top gives that spelling a real job: the per-shell override
// between the flag and the file.
func defaultAgent() agentKind {
	kind, _, _ := defaultAgentWithSource()
	return kind
}

// defaultAgentWithSource resolves the default and reports where it came from,
// plus the raw spelling that supplied it (so a rejected value can be quoted
// back). An unparseable setting falls through to the next source rather than
// failing the command: a typo in a config file must not make rig unusable.
func defaultAgentWithSource() (agentKind, agentSource, string) {
	if raw := os.Getenv("RIG_AGENT"); raw != "" {
		if kind, err := parseAgent(raw); err == nil {
			return kind, agentFromEnv, raw
		}
	}
	if raw := readRigConfig().Agent; raw != "" {
		if kind, err := parseAgent(raw); err == nil {
			return kind, agentFromConfig, raw
		}
	}
	return agentClaude, agentFromBuiltin, ""
}

// configSetting is one settable key. The indirection is worth its weight at one
// entry because it's what makes the namespace an honest promise: the next
// setting is a struct field and one of these, not another arm of a switch that
// has to remember to teach itself to the lister.
type configSetting struct {
	name string
	// effective reports the value in force and the short label for where it
	// came from. The label goes in a table column, so it stays a name rather
	// than a sentence; anything that needs explaining goes in note.
	effective func() (value string, origin string)
	// note is the one thing worth saying beyond the value: an empty string
	// most of the time, and a warning when a setting is quietly shadowing
	// something you also set somewhere else.
	note func() string
	// set validates and stores; an empty value clears the setting.
	set func(c *rigConfig, value string) error
}

var configSettings = []configSetting{{
	name: "agent",
	effective: func() (string, string) {
		kind, src, _ := defaultAgentWithSource()
		switch src {
		case agentFromConfig:
			return string(kind), configOriginPath()
		case agentFromEnv:
			return string(kind), "RIG_AGENT"
		default:
			return string(kind), "built-in default"
		}
	},
	note: func() string {
		// The setting you just wrote losing to something you set months ago in
		// a shell config is the one confusing outcome this command can produce,
		// and it's silent by construction. Saying so, and naming the fix, is
		// the whole reason a note exists.
		_, src, raw := defaultAgentWithSource()
		stored := readRigConfig().Agent
		if src != agentFromEnv || stored == "" {
			return ""
		}
		return fmt.Sprintf("note: %s says %s, but RIG_AGENT=%s in your shell wins over it. Unset RIG_AGENT to use the file.",
			configOriginPath(), stored, raw)
	},
	set: func(c *rigConfig, value string) error {
		if value == "" {
			c.Agent = ""
			return nil
		}
		kind, err := parseAgent(value)
		if err != nil {
			return err
		}
		// Store the canonical long name rather than whatever shorthand was
		// typed, so the file reads the way the docs talk about agents.
		c.Agent = string(kind)
		return nil
	},
}, {
	name: "backend",
	effective: func() (string, string) {
		name, src, _ := defaultBackendWithSource()
		switch src {
		case agentFromConfig:
			return name, configOriginPath()
		case agentFromEnv:
			return name, "RIG_BACKEND"
		default:
			return name, "built-in default"
		}
	},
	note: func() string {
		_, src, raw := defaultBackendWithSource()
		stored := readRigConfig().Backend
		if src != agentFromEnv || stored == "" {
			return ""
		}
		return fmt.Sprintf("note: %s says %s, but RIG_BACKEND=%s in your shell wins over it. Unset RIG_BACKEND to use the file.",
			configOriginPath(), stored, raw)
	},
	set: func(c *rigConfig, value string) error {
		if value == "" {
			c.Backend = ""
			return nil
		}
		if _, err := backendByName(value); err != nil {
			return err
		}
		c.Backend = value
		return nil
	},
}}

// configOriginPath renders the config file's location for the source column,
// abbreviated under ~ because the full path is noise in a table whose job is to
// say "this came from the file, not your shell".
func configOriginPath() string {
	path, err := rigConfigPath()
	if err != nil {
		return "config file"
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return path
}

func findConfigSetting(name string) (configSetting, bool) {
	for _, s := range configSettings {
		if s.name == name {
			return s, true
		}
	}
	return configSetting{}, false
}

// runConfigCmd is `rig config [KEY [VALUE]]`, positional in git's shape: no
// argument lists everything, a key reads it, a key and a value writes it.
// --unset clears a key back to whatever the environment or the built-in default
// says, which is a different statement from setting it to that same value.
func runConfigCmd(args []string) error {
	unset := false
	var rest []string
	for _, a := range args {
		if a == "--unset" {
			unset = true
			continue
		}
		rest = append(rest, a)
	}

	switch {
	case len(rest) == 0 && !unset:
		return listConfig(os.Stdout)
	case len(rest) == 0:
		return fmt.Errorf("--unset needs a setting: %s", strings.Join(configNames(), ", "))
	case len(rest) > 2:
		return fmt.Errorf("usage: rig config [SETTING [VALUE]] | rig config SETTING --unset")
	}

	setting, ok := findConfigSetting(rest[0])
	if !ok {
		return fmt.Errorf("unknown setting %q (want %s)", rest[0], strings.Join(configNames(), ", "))
	}

	if len(rest) == 1 && !unset {
		value, origin := setting.effective()
		fmt.Printf("%s = %s (%s)\n", setting.name, value, origin)
		printNote(setting)
		return nil
	}

	value := ""
	if len(rest) == 2 {
		if unset {
			return fmt.Errorf("--unset takes no value")
		}
		value = rest[1]
	}

	// Report against the value that was in force before the write, so a set
	// that changes nothing visible still says so rather than looking like it
	// did something.
	before, _ := setting.effective()

	c := readRigConfig()
	if err := setting.set(&c, value); err != nil {
		return err
	}
	if err := writeRigConfig(c); err != nil {
		return err
	}

	// Always name the source, even when nothing visibly moved. A write that
	// changes no value still changes where the value comes from, and the two
	// cases where that matters are exactly the confusing ones: pinning the
	// built-in default into the file, and writing a file that an env var is
	// already shadowing.
	after, afterOrigin := setting.effective()
	if after == before {
		fmt.Printf("%s = %s (%s; unchanged)\n", setting.name, after, afterOrigin)
	} else {
		fmt.Printf("%s = %s (%s; was %s)\n", setting.name, after, afterOrigin, before)
	}
	printNote(setting)
	return nil
}

// printNote emits a setting's warning, if it has one, on its own line beneath
// the value.
func printNote(s configSetting) {
	if s.note == nil {
		return
	}
	if n := s.note(); n != "" {
		fmt.Println(n)
	}
}

func configNames() []string {
	out := make([]string, 0, len(configSettings))
	for _, s := range configSettings {
		out = append(out, s.name)
	}
	return out
}

func listConfig(out *os.File) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, s := range configSettings {
		value, origin := s.effective()
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.name, value, origin)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, s := range configSettings {
		printNote(s)
	}
	return nil
}
