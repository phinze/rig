package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRelayRoutesIssueDiscoveryToProjectRigInbox(t *testing.T) {
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

	if err := runRelay([]string{"MIR-1686", "must", "inherit", "the", "same", "metadata", "precedence"}); err != nil {
		t.Fatal(err)
	}
	inbox := activeNotifications()
	if len(inbox) != 1 {
		t.Fatalf("inbox = %+v", inbox)
	}
	got := inbox[0]
	if got.Rig != "project-byoi" || got.Source != "rig:mir-1696" || got.Title != "Discovery from MIR-1696" || got.Body != "MIR-1686 must inherit the same metadata precedence" {
		t.Fatalf("relay notification = %+v", got)
	}
}
