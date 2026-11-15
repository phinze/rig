package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommentsHoldSendAndAck(t *testing.T) {
	basedir := webRig(t)
	rig := rigInfo{ID: "ALPHA-1", Slug: "alpha", Path: basedir}
	var delivered []string
	deliverComments = func(_ rigInfo, text string) error { delivered = append(delivered, text); return nil }
	t.Cleanup(func() {
		deliverComments = func(target rigInfo, text string) error { return deliverRigMessage(target, text, "", webSender) }
	})

	a, err := addComment(basedir, commentAnchor{Kind: "diff", Repo: "one", Path: "x.go", Start: 3, End: 5, Label: "one/x.go:3-5"}, "line three\nline four", "why this?", "mac")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := addComment(basedir, commentAnchor{Kind: "tour", Section: "Going down", Label: "Tour › Going down"}, "", "say more", "mac")
	c, _ := addComment(basedir, commentAnchor{Kind: "text", Label: "something"}, "", "never mind", "mac")
	if err := discardComment(basedir, c.ID); err != nil {
		t.Fatal(err)
	}

	// Held comments go out together, as one message.
	if _, err := sendComments(rig, nil); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 {
		t.Fatalf("sent %d messages, want one", len(delivered))
	}
	for _, want := range []string{"[" + a.ID + "] one/x.go:3-5", "> line four", "why this?", "[" + b.ID + "] Tour › Going down", "rig ack"} {
		if !strings.Contains(delivered[0], want) {
			t.Fatalf("message lacks %q:\n%s", want, delivered[0])
		}
	}
	if strings.Contains(delivered[0], "never mind") {
		t.Fatal("a discarded comment went out")
	}
	if err := discardComment(basedir, a.ID); err == nil {
		t.Fatal("a sent comment shouldn't be discardable")
	}

	t.Chdir(filepath.Join(basedir))
	if err := runAck([]string{a.ID, "done"}); err != nil {
		t.Fatal(err)
	}
	if err := runAck([]string{b.ID, "maybe"}); err == nil {
		t.Fatal("an unknown state should be refused")
	}
	states := map[string]string{}
	for _, c := range readComments(basedir) {
		states[c.ID] = c.State
	}
	if states[a.ID] != "done" || states[b.ID] != "sent" || len(states) != 2 {
		t.Fatalf("states = %v", states)
	}
}

func TestFailedSendStaysVisibleAndRetries(t *testing.T) {
	basedir := webRig(t)
	rig := rigInfo{ID: "ALPHA-1", Slug: "alpha", Path: basedir}
	fail := true
	deliverComments = func(rigInfo, string) error {
		if fail {
			return errors.New("agent not running")
		}
		return nil
	}
	t.Cleanup(func() {
		deliverComments = func(target rigInfo, text string) error { return deliverRigMessage(target, text, "", webSender) }
	})

	c, _ := addComment(basedir, commentAnchor{Kind: "text", Label: "x"}, "", "hello", "")
	if _, err := sendComments(rig, nil); err == nil {
		t.Fatal("a failed delivery should say so")
	}
	got := readComments(basedir)[0]
	if got.State != commentFailed || !strings.Contains(got.Error, "not running") {
		t.Fatalf("after a failed send: %+v", got)
	}
	fail = false
	if _, err := sendComments(rig, []string{c.ID}); err != nil {
		t.Fatal(err)
	}
	if got := readComments(basedir)[0]; got.State != commentSent || got.Error != "" {
		t.Fatalf("after retry: %+v", got)
	}
}

func TestCommentWritesRefuseOtherSites(t *testing.T) {
	webRig(t)
	srv := webServer(t, newServeWeb(""))
	post := func(ctype, origin string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/comments/alpha", strings.NewReader(`{"anchor":{"kind":"text","label":"x"},"body":"hi"}`))
		req.Header.Set("Content-Type", ctype)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("text/plain", ""); code != http.StatusUnsupportedMediaType {
		t.Fatalf("a form-style post got %d", code)
	}
	if code := post("application/json", "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("a cross-origin post got %d", code)
	}
	if code := post("application/json", srv.URL); code != http.StatusOK {
		t.Fatalf("a same-origin post got %d", code)
	}
	if _, err := os.Stat(commentsPath(filepath.Join(os.Getenv("HOME"), "workspaces", "alpha"))); err != nil {
		t.Fatalf("the accepted comment wasn't logged: %v", err)
	}
}

// A diff comment is stamped when it's made, so the stamp rides with it to
// the page (which marks the lines wherever they've moved) and the agent's
// message names the change the lines belong to.
func TestADiffCommentIsStampedWhenMade(t *testing.T) {
	basedir := t.TempDir()
	was := stampDiffAnchor
	t.Cleanup(func() { stampDiffAnchor = was })
	stampDiffAnchor = func(_ string, a *commentAnchor, _ string) {
		if a.Kind == "diff" {
			a.Stamp = json.RawMessage(`{"change":"kxyzabcdefghijkl","commit":"abc","start":3,"end":4}`)
		}
	}
	c, err := addComment(basedir, commentAnchor{Kind: "diff", Repo: "r", Path: "a.go", Start: 3, End: 4, Label: "r/a.go:3-4"}, "x", "why?", "")
	if err != nil {
		t.Fatal(err)
	}
	if stampChange(c.Anchor.Stamp) != "kxyzabcd" {
		t.Fatalf("stamp: %s", c.Anchor.Stamp)
	}
	saved := readComments(basedir)
	if len(saved) != 1 || stampChange(saved[0].Anchor.Stamp) != "kxyzabcd" {
		t.Fatalf("the stamp is kept with the comment: %+v", saved)
	}
	if msg := commentMessage(saved); !strings.Contains(msg, "(lines in change kxyzabcd)") {
		t.Fatalf("the agent hears which change: %s", msg)
	}
	tour, _ := addComment(basedir, commentAnchor{Kind: "tour", Section: "Why", Label: "tour"}, "", "hm", "")
	if len(tour.Anchor.Stamp) != 0 {
		t.Fatal("only diff anchors are stamped")
	}
}

// A stamp is kept only when its lines hold what was selected; the code
// moving while the comment was written would otherwise point it elsewhere.
func TestAStampMustHoldTheSelection(t *testing.T) {
	stamp := []byte(`{"snippet":["func retry() {","\treturn nil"]}`)
	if !stampMatchesQuote(stamp, "12 13 + func retry() {\n14 + \treturn nil") {
		t.Fatal("the selected lines match")
	}
	if !stampMatchesQuote(stamp, "retry() {\n\treturn nil") {
		t.Fatal("a selection may start mid-line")
	}
	if stampMatchesQuote(stamp, "something else entirely") {
		t.Fatal("other code doesn't match")
	}
	if !stampMatchesQuote(stamp, "") {
		t.Fatal("with nothing selected there's nothing to contradict")
	}
}
