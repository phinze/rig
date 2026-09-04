package main

import (
	"strings"
	"testing"
)

func TestResolveCommandExactBeatsPrefix(t *testing.T) {
	// "pr" spells a prefix of "project" as well as a command of its own. The
	// exact spelling has to win, or adding a longer command would silently
	// steal a name that already works.
	got, err := resolveCommand("pr")
	if err != nil || got != "pr" {
		t.Fatalf("resolveCommand(pr) = %q, %v; want pr", got, err)
	}
}

func TestResolveCommandUniquePrefix(t *testing.T) {
	for input, want := range map[string]string{
		"u":     "up",
		"swe":   "sweep",
		"swi":   "switch",
		"resum": "resume",
		"resur": "resurrect",
		"rev":   "review",
		"rec":   "recto",
		"rel":   "relay",
		"h":     "history",
		"c":     "switch", // through the cd alias
		"cd":    "switch",
	} {
		got, err := resolveCommand(input)
		if err != nil {
			t.Errorf("resolveCommand(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("resolveCommand(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResolveCommandAmbiguousNamesCandidates(t *testing.T) {
	_, err := resolveCommand("sw")
	if err == nil {
		t.Fatal("resolveCommand(sw) resolved; want ambiguity")
	}
	for _, want := range []string{"sweep", "switch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error %q does not name %q", err, want)
		}
	}
}

func TestResolveCommandSuggestsOnTypo(t *testing.T) {
	for input, want := range map[string]string{
		"revew":   "review",
		"swep":    "sweep",
		"downn":   "down",
		"raadar":  "radar",
		"reusme":  "resume",
		"hlep":    "help",
		"noitfy":  "notify",
		"projekt": "project",
	} {
		_, err := resolveCommand(input)
		if err == nil {
			t.Errorf("resolveCommand(%q) resolved; want a suggestion", input)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("resolveCommand(%q) error %q does not suggest %q", input, err, want)
		}
	}
}

func TestResolveCommandUnknownWithNoNeighbour(t *testing.T) {
	_, err := resolveCommand("zzzzzzzz")
	if err == nil {
		t.Fatal("resolveCommand(zzzzzzzz) resolved; want unknown")
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("error %q offers a suggestion for a word nothing is near", err)
	}
}

func TestResolveCommandHiddenIsExactOnly(t *testing.T) {
	// The internals have to keep working when invoked by name...
	if got, err := resolveCommand("__gh"); err != nil || got != "__gh" {
		t.Fatalf("resolveCommand(__gh) = %q, %v; want __gh", got, err)
	}
	// ...but must not be reachable by abbreviation, and must never be offered
	// as the fix for a typo.
	if _, err := resolveCommand("__g"); err == nil {
		t.Error("resolveCommand(__g) resolved; hidden commands must be exact")
	}
	for _, input := range []string{"__gg", "__agen"} {
		if _, err := resolveCommand(input); err == nil {
			t.Errorf("resolveCommand(%q) resolved", input)
		} else if strings.Contains(err.Error(), "did you mean") {
			t.Errorf("resolveCommand(%q) suggested a hidden command: %v", input, err)
		}
	}
}

func TestResolveCommandHelpDoesNotSpendItsLetter(t *testing.T) {
	for _, spelling := range []string{"help", "-h", "--help"} {
		if got, err := resolveCommand(spelling); err != nil || got != "help" {
			t.Errorf("resolveCommand(%q) = %q, %v; want help", spelling, got, err)
		}
	}
	// help resolves by its full name only, so "h" stays history.
	if got, err := resolveCommand("h"); err != nil || got != "history" {
		t.Errorf("resolveCommand(h) = %q, %v; want history", got, err)
	}
}

func TestEveryCommandNameResolvesToItself(t *testing.T) {
	for _, name := range commandNames {
		got, err := resolveCommand(name)
		if err != nil || got != name {
			t.Errorf("resolveCommand(%q) = %q, %v; want itself", name, got, err)
		}
	}
}
