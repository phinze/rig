package main

import (
	"fmt"
	"sort"
	"strings"
)

// commandNames are the commands you can type, in usage order. Resolution walks
// this list for both prefix shorthand and typo suggestions, which is exactly
// why the __-prefixed internals aren't in it: they're exact-match only, and
// listing them would let `rig _` resolve to one and let a typo suggest a
// command that isn't meant to be typed.
var commandNames = []string{
	"up", "new", "project", "dispatch", "relay", "review", "pr", "track",
	"add", "recto", "ls", "notify", "switch", "radar", "park", "wake",
	"resume", "waiting", "sweep", "down", "reap", "history", "resurrect",
	"env", "info",
}

// commandAliases are retained spellings that stand in for a canonical command.
// They join the prefix namespace rather than sitting beside it, so `rig c`
// reaches switch by way of cd.
var commandAliases = map[string]string{"cd": "switch"}

// hiddenCommands are the internals other processes invoke: the per-rig gh shim,
// the pickers' state round-trips, and the durable teardown worker. Exact spelling
// only, on both sides — they never resolve from a prefix, and they're never
// offered as a suggestion.
var hiddenCommands = []string{"__gh", "__agent", "__issues", "__teardown"}

// helpCommands print usage. They're resolved before the prefix pass rather than
// through it, which is what keeps `rig h` meaning history: help is reachable by
// its full name (and by a typo suggestion), and it doesn't spend the letter.
var helpCommands = []string{"-h", "--help", "help"}

// resolveCommand maps what you typed to a canonical command name. Exact wins
// over prefix, so `rig pr` is always pr and never project even though it spells
// a prefix of it. A unique prefix resolves; an ambiguous one names its
// candidates instead of guessing; anything else comes back with the nearest
// spellings. Every error it returns is already phrased for the terminal.
func resolveCommand(input string) (string, error) {
	if input == "" {
		return "", fmt.Errorf("no command given")
	}
	for _, name := range helpCommands {
		if name == input {
			return "help", nil
		}
	}
	for _, name := range hiddenCommands {
		if name == input {
			return name, nil
		}
	}
	for _, name := range commandNames {
		if name == input {
			return name, nil
		}
	}
	if canon, ok := commandAliases[input]; ok {
		return canon, nil
	}

	matches := prefixMatches(input)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		// fall through to suggestions
	default:
		return "", fmt.Errorf("%q is ambiguous: %s", input, strings.Join(matches, ", "))
	}
	if near := nearestCommands(input); len(near) > 0 {
		return "", fmt.Errorf("unknown command %q; did you mean %s?", input, orList(near))
	}
	return "", fmt.Errorf("unknown command %q", input)
}

// prefixMatches collects the canonical commands input abbreviates, sorted and
// deduplicated. An alias contributes its target, so `rig c` yields one match
// (switch) rather than reading as a second candidate alongside it.
func prefixMatches(input string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(canon string) {
		if !seen[canon] {
			seen[canon] = true
			out = append(out, canon)
		}
	}
	for _, name := range commandNames {
		if strings.HasPrefix(name, input) {
			add(name)
		}
	}
	for alias, canon := range commandAliases {
		if strings.HasPrefix(alias, input) {
			add(canon)
		}
	}
	sort.Strings(out)
	return out
}

// nearestCommands returns the spellings closest to a mistyped word, best first.
// The budget scales with length: a short command tolerates one slip, a longer
// one two. That's what keeps `rig revew` pointing at review without letting
// `rig x` offer half the vocabulary. At most three, because a longer list stops
// being a suggestion and starts being usage.
func nearestCommands(input string) []string {
	budget := 1
	if len(input) >= 5 {
		budget = 2
	}
	type scored struct {
		name string
		dist int
	}
	var xs []scored
	consider := func(spelling, canon string) {
		if d := editDistance(input, spelling); d <= budget {
			xs = append(xs, scored{canon, d})
		}
	}
	for _, name := range commandNames {
		consider(name, name)
	}
	for alias, canon := range commandAliases {
		consider(alias, canon)
	}
	consider("help", "help")

	sort.SliceStable(xs, func(i, j int) bool {
		if xs[i].dist != xs[j].dist {
			return xs[i].dist < xs[j].dist
		}
		return xs[i].name < xs[j].name
	})
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if seen[x.name] {
			continue
		}
		seen[x.name] = true
		out = append(out, x.name)
		if len(out) == 3 {
			break
		}
	}
	return out
}

// editDistance is Damerau-Levenshtein restricted to adjacent transpositions
// (the optimal string alignment variant). Transposition counts as one edit
// rather than two on purpose: swapping two letters is the typo people actually
// make, so "hlep" has to land one step from help and not two. Command names
// are ASCII, so bytes and characters agree and the table stays the algorithm.
func editDistance(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := 0; j <= len(b); j++ {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(min(d[i-1][j]+1, d[i][j-1]+1), d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}

// orList joins suggestions the way a sentence would.
func orList(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("%q", names[0])
	case 2:
		return fmt.Sprintf("%q or %q", names[0], names[1])
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + ", or " + quoted[len(quoted)-1]
}
