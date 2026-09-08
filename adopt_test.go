package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Adoption is a manifest promotion, so the fields it writes have to survive the
// round-trip and the fields that identify the rig on disk have to not move.
func TestAdoptedManifestRoundTrips(t *testing.T) {
	dir := t.TempDir()
	m := manifest{
		ID:    "post-hoc-rig-type-change",
		Title: "post-hoc rig type change",
		Repos: map[string]string{"rig": "phinze/rig"},
	}
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}

	// What runAdopt does to the manifest, without the Linear round-trip.
	m.Tracker, m.TrackerID, m.Title = "linear", "MIR-123", "Allow post-hoc rig type change"
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest after adopt: %v", err)
	}

	got, err := readManifest(dir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if got.Tracker != "linear" || got.TrackerID != "MIR-123" {
		t.Errorf("tracker identity not round-tripped: %q/%q", got.Tracker, got.TrackerID)
	}
	if got.ID != "post-hoc-rig-type-change" {
		t.Errorf("adopt must not move the local id: got %q", got.ID)
	}
	if got.Title != "Allow post-hoc rig type change" {
		t.Errorf("title not adopted: got %q", got.Title)
	}
	if got.Repos["rig"] != "phinze/rig" {
		t.Errorf("repos disturbed by adoption: %v", got.Repos)
	}
}

// The point of adoption is that the boards stop reading the rig as loose. Kind
// is derived from the tracker field alone, so this is the whole promotion.
func TestAdoptedRigReadsAsTicketKind(t *testing.T) {
	loose := rigStatus{ID: "post-hoc-rig-type-change", Title: "post-hoc rig type change"}
	if got := rigKindOf(loose); got != rigKindLoose {
		t.Fatalf("rigKindOf(loose) = %v, want loose", got)
	}
	adopted := loose
	adopted.Tracker, adopted.TrackerID = "linear", "MIR-123"
	if got := rigKindOf(adopted); got != rigKindTicket {
		t.Errorf("rigKindOf(adopted) = %v, want ticket", got)
	}
}

// An adopted rig keeps its kickoff slug as its id, so a lookup by the ticket
// identifier has to reach it through TrackerID or `rig up MIR-123` builds a
// second, empty rig for work that already exists.
func TestPickRigStatusFindsAdoptedRigByTrackerID(t *testing.T) {
	statuses := []rigStatus{
		{ID: "post-hoc-rig-type-change", Slug: "post-hoc-rig-type-change",
			Title: "Allow post-hoc rig type change", Tracker: "linear", TrackerID: "MIR-123"},
		{ID: "mir-999-unrelated", Slug: "mir-999-unrelated", Title: "something else"},
	}
	got, err := pickRigStatus(statuses, []string{"MIR-123"}, "")
	if err != nil {
		t.Fatalf("pickRigStatus: %v", err)
	}
	if got == nil || got.ID != "post-hoc-rig-type-change" {
		t.Fatalf("pickRigStatus by tracker id = %v, want the adopted rig", got)
	}
}

// A tracker line appended to the end of the manifest lands under whatever table
// header came last, because the parser's section never resets. Recording it as
// a repo would flip every len(m.Repos) == 1 check and be persisted by the next
// write, so a value with no owner/repo shape is dropped instead.
func TestReadManifestDropsMisplacedScalarFromReposTable(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "id    = \"loose\"\ntitle = \"loose\"\n\n[repos]\nrig = \"phinze/rig\"\ntracker = \"linear\"\ntracker_id = \"MIR-123\"\n"
	if err := os.WriteFile(filepath.Join(dir, manifestName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := readManifest(dir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if len(m.Repos) != 1 || m.Repos["rig"] != "phinze/rig" {
		t.Errorf("misplaced scalars became phantom repos: %v", m.Repos)
	}
}

// `a` and `ad` both abbreviated add before adopt existed. Adopt is the
// newcomer, so it starts at `ado` and the working spellings keep working.
func TestAdoptDoesNotStealAddsAbbreviations(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"a", "add"}, {"ad", "add"}, {"add", "add"},
		{"ado", "adopt"}, {"adopt", "adopt"},
	} {
		got, err := resolveCommand(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("resolveCommand(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
}

// Adopt promotes an untracked rig; it never re-points one that already has an
// identity, because the rig's branch, PR and conversation all accumulated under
// the first one.
func TestAdoptRefusesRigThatAlreadyTracks(t *testing.T) {
	// The mutation lock consults the pending-teardown jobs in the state dir, so
	// pin it rather than letting the check reach the developer's real one.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	if err := writeManifest(dir, manifest{ID: "mir-1", Title: "x", Tracker: "linear", TrackerID: "MIR-1"}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	err := runAdopt([]string{"MIR-2"})
	if err == nil || !strings.Contains(err.Error(), "already tracks MIR-1") {
		t.Fatalf("runAdopt over an existing tracker = %v; want a refusal naming MIR-1", err)
	}
	// Re-adopting the same identifier is a no-op rather than an error, so a
	// retried command doesn't look like a failure.
	if err := runAdopt([]string{"mir-1"}); err != nil {
		t.Errorf("re-adopting the same id = %v; want nil", err)
	}
}
