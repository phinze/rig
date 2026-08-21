package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQueryLinkedLinearTasks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "test-token" {
			t.Errorf("Authorization = %q, want test-token", got)
		}
		var request struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if got := request.Variables["url"]; got != "https://github.com/me/api/pull/42" {
			t.Errorf("url = %v", got)
		}
		_, _ = w.Write([]byte(`{
  "data": {
    "attachmentsForURL": {
      "nodes": [{
        "metadata": {"linkKind": "contributes"},
        "issue": {
          "identifier": "MIR-75",
          "title": "Add zig stack",
          "branchName": "phinze/mir-75-add-zig-stack"
        }
      }]
    }
  }
}`))
	}))
	defer server.Close()

	client := &linearClient{
		HTTP:     &http.Client{Timeout: time.Second},
		Endpoint: server.URL,
		Token:    "test-token",
	}
	linked, err := queryLinkedLinearTasks(client, "https://github.com/me/api/pull/42")
	if err != nil {
		t.Fatal(err)
	}
	if len(linked) != 1 || linked[0].Task.Identifier != "MIR-75" || linked[0].LinkKind != "contributes" {
		t.Fatalf("linked = %+v", linked)
	}
}

func TestPrimaryLinkedLinearTask(t *testing.T) {
	linked := []linkedLinearTask{
		{Task: task{Identifier: "MIR-1"}, LinkKind: "related"},
		{Task: task{Identifier: "MIR-2"}, LinkKind: "contributes"},
		{Task: task{Identifier: "MIR-3"}, LinkKind: "closes"},
	}
	got, ok := primaryLinkedLinearTask(linked)
	if !ok || got.Identifier != "MIR-3" {
		t.Fatalf("primaryLinkedLinearTask = (%+v, %v), want MIR-3", got, ok)
	}
}
