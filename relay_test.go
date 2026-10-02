package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelaySendsIssueDiscoveryToProjectRig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LINEAR_API_TOKEN", "test-token")
	workspaces := filepath.Join(home, "workspaces")
	if err := os.MkdirAll(workspaces, 0o755); err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(workspaces, "mir-1696-metadata")
	projectDir := filepath.Join(workspaces, "project-byoi")
	for _, dir := range []string{taskDir, projectDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeManifest(taskDir, manifest{ID: "mir-1696", Tracker: "linear", TrackerID: "MIR-1696"}); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(projectDir, manifest{ID: "project-byoi", Kind: "project", Tracker: "linear", TrackerID: "project-uuid"}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"issue":{"identifier":"MIR-1696","project":{"id":"project-uuid","name":"Bring Your Own Image","url":"https://linear/project/byoi"}}}}`))
	}))
	defer server.Close()
	t.Setenv("RIG_LINEAR_GRAPHQL_ENDPOINT", server.URL)
	t.Chdir(taskDir)

	type delivery struct {
		target, text string
		sender       rigSender
	}
	var got []delivery
	orig := relayDeliver
	relayDeliver = func(target rigInfo, text, replyTo string, sender rigSender) error {
		got = append(got, delivery{target.ID, text, sender})
		return nil
	}
	t.Cleanup(func() { relayDeliver = orig })

	if err := runRelay([]string{"MIR-1686", "must", "inherit", "the", "same", "metadata", "precedence"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("deliveries = %+v", got)
	}
	d := got[0]
	if d.target != "project-byoi" || d.sender.addr != "rig:mir-1696" ||
		d.text != "Discovery from MIR-1696 (via rig relay):\nMIR-1686 must inherit the same metadata precedence" {
		t.Fatalf("relay delivery = %+v", d)
	}
	if inbox := activeNotifications(); len(inbox) != 0 {
		t.Fatalf("relay should no longer post to the notify inbox: %+v", inbox)
	}

	// An unreachable project rig is the task agent's answer, not a note
	// filed somewhere: the send's own reason comes back, naming the rig.
	relayDeliver = func(rigInfo, string, string, rigSender) error {
		return errors.New("project-byoi has no live session (wake or dispatch it first)")
	}
	err := runRelay([]string{"anything"})
	if err == nil || !strings.Contains(err.Error(), "relaying to project-byoi") || !strings.Contains(err.Error(), "no live session") {
		t.Fatalf("relay to a down project rig: %v", err)
	}
}
