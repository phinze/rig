package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Comments are what the person sends the agent from the web window: a
// selection on the page (a diff span, a tour passage, any text) plus what
// they want done about it. Recto renders the page and says what a selection
// is anchored to; rig delivers the comment and tracks where it stands, the
// agent marking each one with `rig ack`.
//
// Comments scatter and answers gather. A comment is held until sent, so a
// reading pass can leave several and send them as one message. The agent
// doesn't answer each in kind: an ack only says "on it" or "handled", and
// anything worth reading goes in the rig's briefing, the one page recto
// renders to organize the person's attention.
//
// The log is .rig/comments.jsonl, append-only events folded into each
// comment's current state. Serve and `rig ack` write it from different
// processes, so each append takes a lock, the same as the message log.

// commentStates, in the order a request moves through them.
const (
	commentHeld    = "held"
	commentSent    = "sent"
	commentFailed  = "failed" // delivery failed; still sendable
	commentStarted = "start"
	commentDone    = "done"
)

// commentAnchor is what a comment is about, as the page described it. Label
// is the human-readable form shown to both sides; the rest lets a page find
// the spot again.
type commentAnchor struct {
	Kind    string `json:"kind"`              // diff | tour | text
	Repo    string `json:"repo,omitempty"`    // the repo's dir in the rig; empty for the rig as a whole
	Path    string `json:"path,omitempty"`    // file, for a diff anchor
	Start   int    `json:"start,omitempty"`   // new-side lines, for a diff anchor
	End     int    `json:"end,omitempty"`     //
	Section string `json:"section,omitempty"` // heading, for a tour anchor
	Label   string `json:"label"`
}

type commentEvent struct {
	V      int            `json:"v"`
	At     time.Time      `json:"at"`
	Type   string         `json:"type"` // comment | send | ack | discard
	ID     string         `json:"id,omitempty"`
	IDs    []string       `json:"ids,omitempty"` // send
	Anchor *commentAnchor `json:"anchor,omitempty"`
	Quote  string         `json:"quote,omitempty"`
	Body   string         `json:"body,omitempty"`
	Client string         `json:"client,omitempty"`
	State  string         `json:"state,omitempty"` // ack
	Error  string         `json:"error,omitempty"` // send that failed
}

// rigComment is one comment folded to where it stands now.
type rigComment struct {
	ID      string        `json:"id"`
	State   string        `json:"state"`
	Anchor  commentAnchor `json:"anchor"`
	Quote   string        `json:"quote,omitempty"`
	Body    string        `json:"body"`
	Error   string        `json:"error,omitempty"` // why the last send failed
	At      time.Time     `json:"at"`
	Updated time.Time     `json:"updated"`
}

func commentsPath(basedir string) string {
	return filepath.Join(basedir, ".rig", "comments.jsonl")
}

func appendCommentEvent(basedir string, ev commentEvent) error {
	if err := os.MkdirAll(filepath.Join(basedir, ".rig"), 0o755); err != nil {
		return err
	}
	ev.V = 1
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(commentsPath(basedir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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

// readComments folds the log, oldest first. A line that doesn't parse is
// skipped: an appender killed mid-write leaves one, and the rest still count.
func readComments(basedir string) []rigComment {
	raw, err := os.ReadFile(commentsPath(basedir))
	if err != nil {
		return nil
	}
	var out []rigComment
	index := map[string]int{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		var ev commentEvent
		if line == "" || json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "comment":
			if ev.Anchor == nil || ev.ID == "" {
				continue
			}
			index[ev.ID] = len(out)
			out = append(out, rigComment{
				ID: ev.ID, State: commentHeld, Anchor: *ev.Anchor,
				Quote: ev.Quote, Body: ev.Body, At: ev.At, Updated: ev.At,
			})
		case "send":
			for _, id := range ev.IDs {
				if i, ok := index[id]; ok {
					c := &out[i]
					if ev.Error != "" {
						c.State, c.Error = commentFailed, ev.Error
					} else {
						c.State, c.Error = commentSent, ""
					}
					c.Updated = ev.At
				}
			}
		case "ack":
			if i, ok := index[ev.ID]; ok {
				out[i].State, out[i].Updated = ev.State, ev.At
			}
		case "discard":
			if i, ok := index[ev.ID]; ok {
				out = slices.Delete(out, i, i+1)
				delete(index, ev.ID)
				for id, j := range index {
					if j > i {
						index[id] = j - 1
					}
				}
			}
		}
	}
	return out
}

// newCommentID is short enough to type in `rig ack`, and unique in its rig.
func newCommentID(existing []rigComment) (string, error) {
	for range 20 {
		b := make([]byte, 3)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id := "c" + hex.EncodeToString(b)[:5]
		if !slices.ContainsFunc(existing, func(c rigComment) bool { return c.ID == id }) {
			return id, nil
		}
	}
	return "", fmt.Errorf("couldn't find a free comment id")
}

// addComment records a held comment.
func addComment(basedir string, anchor commentAnchor, quote, body, client string) (rigComment, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return rigComment{}, fmt.Errorf("a comment needs something to say")
	}
	if strings.TrimSpace(anchor.Label) == "" {
		return rigComment{}, fmt.Errorf("a comment needs an anchor")
	}
	id, err := newCommentID(readComments(basedir))
	if err != nil {
		return rigComment{}, err
	}
	ev := commentEvent{Type: "comment", ID: id, Anchor: &anchor, Quote: quote, Body: body, Client: client, At: time.Now()}
	if err := appendCommentEvent(basedir, ev); err != nil {
		return rigComment{}, err
	}
	return rigComment{ID: id, State: commentHeld, Anchor: anchor, Quote: quote, Body: body, At: ev.At, Updated: ev.At}, nil
}

// maxQuote keeps one long selection from drowning the comments around it.
// The anchor still says where the rest is.
const maxQuote = 1500

// commentMessage is what the agent reads: what to do with these, then each
// comment with its id, anchor, the quoted selection, and what was said.
func commentMessage(cs []rigComment) string {
	var b strings.Builder
	if len(cs) == 1 {
		b.WriteString("Paul sent a comment from the web window, on something he selected there.\n")
	} else {
		fmt.Fprintf(&b, "Paul sent %d comments from the web window, each on something he selected there.\n", len(cs))
	}
	b.WriteString("Mark each by id so he can see where it stands: `rig ack ID start` when you\n")
	b.WriteString("take it up, `rig ack ID done` when it's handled. Don't answer each one in a\n")
	b.WriteString("message. What he should read (what each turned into, questions, decisions)\n")
	b.WriteString("goes in the briefing: `recto brief --group \"$RIG_BASEDIR\"`.\n")
	for _, c := range cs {
		fmt.Fprintf(&b, "\n[%s] %s\n", c.ID, c.Anchor.Label)
		if q := strings.TrimSpace(c.Quote); q != "" {
			if len(q) > maxQuote {
				q = strings.TrimSpace(q[:maxQuote]) + " …"
			}
			for line := range strings.SplitSeq(q, "\n") {
				b.WriteString("> " + line + "\n")
			}
		}
		b.WriteString(c.Body + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// webSender is who a comment comes from: the person, by way of the window.
// Its address isn't a rig, so `rig reply` can say to answer with ack instead.
var webSender = rigSender{name: "Paul (web window)", addr: "web"}

// deliverComments is the transport, swapped out in tests.
var deliverComments = func(target rigInfo, text string) error {
	return deliverRigMessage(target, text, "", webSender)
}

// sendComments delivers the given comments as one message, or every sendable
// one when ids is empty, and records the outcome either way: a failed send
// stays visible and can be retried.
func sendComments(target rigInfo, ids []string) ([]rigComment, error) {
	var picked []rigComment
	for _, c := range readComments(target.Path) {
		sendable := c.State == commentHeld || c.State == commentFailed
		if sendable && (len(ids) == 0 || slices.Contains(ids, c.ID)) {
			picked = append(picked, c)
		}
	}
	if len(picked) == 0 {
		return nil, fmt.Errorf("nothing waiting to send")
	}
	sent := make([]string, len(picked))
	for i, c := range picked {
		sent[i] = c.ID
	}
	err := deliverComments(target, commentMessage(picked))
	ev := commentEvent{Type: "send", IDs: sent}
	if err != nil {
		ev.Error = err.Error()
	}
	if logErr := appendCommentEvent(target.Path, ev); logErr != nil && err == nil {
		err = fmt.Errorf("delivered, but recording it failed: %w", logErr)
	}
	return picked, err
}

// discardComment drops a comment that hasn't gone out.
func discardComment(basedir, id string) error {
	for _, c := range readComments(basedir) {
		if c.ID != id {
			continue
		}
		if c.State != commentHeld && c.State != commentFailed {
			return fmt.Errorf("%s has already been sent", id)
		}
		return appendCommentEvent(basedir, commentEvent{Type: "discard", ID: id})
	}
	return fmt.Errorf("no comment %s in this rig", id)
}

func runAck(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: rig ack ID start|done (what to say goes in the briefing: recto brief)")
	}
	id, state := args[0], args[1]
	switch state {
	case commentStarted, commentDone:
	default:
		return fmt.Errorf("rig ack: state is start or done, not %q", state)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	basedir, err := findBasedir(cwd)
	if err != nil {
		return fmt.Errorf("rig ack runs inside the rig whose comment it answers: %w", err)
	}
	for _, c := range readComments(basedir) {
		if c.ID != id {
			continue
		}
		if c.State == commentHeld || c.State == commentFailed {
			return fmt.Errorf("%s was never delivered, so there's nothing to answer", id)
		}
		if err := appendCommentEvent(basedir, commentEvent{Type: "ack", ID: id, State: state}); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "rig: %s is %s\n", id, state)
		return nil
	}
	return fmt.Errorf("no comment %s in this rig", id)
}
