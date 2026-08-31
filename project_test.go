package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func projectTestClient(t *testing.T, handler http.HandlerFunc) *linearClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &linearClient{HTTP: &http.Client{Timeout: time.Second}, Endpoint: server.URL, Token: "test-token"}
}

func TestResolveLinearProjectUsesUUIDAndExactName(t *testing.T) {
	client := projectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(req.Query, "query Project(") {
			_, _ = w.Write([]byte(`{"data":{"project":{"id":"2371b91f-53ba-4c7d-aa0b-7a1174451379","name":"Bring Your Own Image","url":"https://linear.app/miren/project/byoi","status":{"name":"In Progress","type":"started"}}}}`))
			return
		}
		if req.Variables["term"] != "Bring Your Own Image" {
			t.Errorf("term = %v", req.Variables["term"])
		}
		_, _ = w.Write([]byte(`{"data":{"searchProjects":{"nodes":[{"id":"2371b91f-53ba-4c7d-aa0b-7a1174451379","name":"Bring Your Own Image","url":"https://linear.app/miren/project/byoi","status":{"name":"In Progress","type":"started"}}]}}}`))
	})

	pick := &agentPick{kind: agentCodex, explicit: true}
	byID, err := resolveLinearProject(client, "2371b91f-53ba-4c7d-aa0b-7a1174451379", pick)
	if err != nil || byID == nil || byID.Name != "Bring Your Own Image" {
		t.Fatalf("UUID resolution = %+v, %v", byID, err)
	}
	byName, err := resolveLinearProject(client, "Bring Your Own Image", pick)
	if err != nil || byName == nil || byName.ID != byID.ID {
		t.Fatalf("name resolution = %+v, %v", byName, err)
	}
}

func TestQueryLinearProjectDetailPaginatesIssues(t *testing.T) {
	calls := 0
	client := projectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			if req.Variables["after"] != nil {
				t.Errorf("first after = %v, want null", req.Variables["after"])
			}
			_, _ = w.Write([]byte(`{"data":{"project":{"id":"project-uuid","name":"P","issues":{"nodes":[{"identifier":"MIR-1","title":"one","updatedAt":"2026-08-31T18:00:00Z","state":{"name":"In Progress","type":"started"}}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}}`))
			return
		}
		if req.Variables["after"] != "next" {
			t.Errorf("second after = %v, want next", req.Variables["after"])
		}
		_, _ = w.Write([]byte(`{"data":{"project":{"id":"project-uuid","name":"P","issues":{"nodes":[{"identifier":"MIR-2","title":"two","updatedAt":"2026-08-31T18:00:00Z","state":{"name":"Done","type":"completed"}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
	})

	project, issues, err := queryLinearProjectDetail(client, "project-uuid")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || project.Name != "P" || len(issues) != 2 || issues[1].Identifier != "MIR-2" {
		t.Fatalf("paginated detail = project %+v, issues %+v, calls %d", project, issues, calls)
	}
}

func TestBuildProjectSnapshotJoinsTrackedAndLegacyRigs(t *testing.T) {
	fakeGh(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspaces := filepath.Join(home, "workspaces")
	if err := os.MkdirAll(workspaces, 0o755); err != nil {
		t.Fatal(err)
	}

	tracked := filepath.Join(workspaces, "mir-1697-command-override")
	if err := os.Mkdir(tracked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(tracked, manifest{
		ID: "mir-1697", Title: "Command override", Tracker: "linear", TrackerID: "MIR-1697",
		Parked: time.Now(), Repos: map[string]string{"runtime": "o/r"},
		Branches: map[string][]string{"runtime": {"phinze/mir-1697"}},
	}); err != nil {
		t.Fatal(err)
	}

	legacy := filepath.Join(workspaces, "mir-1686-recipes")
	if err := os.Mkdir(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(legacy, manifest{ID: "mir-1686", Title: "Recipes"}); err != nil {
		t.Fatal(err)
	}

	client := projectTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
  "data": {"project": {
    "id": "project-uuid", "name": "Bring Your Own Image", "description": "",
    "url": "https://linear.app/miren/project/byoi", "health": null,
    "updatedAt": "2026-08-13T20:04:15Z", "startDate": "2026-07-23",
    "targetDate": "2026-08-31", "progress": 0.7, "scope": 3,
    "status": {"name": "In Progress", "type": "started"}, "lead": {"name": "Paul"},
    "currentProgress": {"scopeCount": 3, "startedIssueCount": 2, "completedIssueCount": 1, "addedIssueCountToday": 0},
    "lastUpdate": null,
    "issues": {"nodes": [
      {"identifier":"MIR-1697","title":"Command override","url":"https://linear/MIR-1697","updatedAt":"2026-08-31T18:00:00Z","priority":0,"estimate":null,"state":{"name":"In Review","type":"started"},"assignee":{"name":"Paul"},"projectMilestone":null},
      {"identifier":"MIR-1686","title":"Recipes","url":"https://linear/MIR-1686","updatedAt":"2026-08-31T17:00:00Z","priority":0,"estimate":null,"state":{"name":"In Progress","type":"started"},"assignee":{"name":"Paul"},"projectMilestone":null},
      {"identifier":"MIR-1452","title":"First-class image","url":"https://linear/MIR-1452","updatedAt":"2026-08-30T17:00:00Z","priority":0,"estimate":null,"state":{"name":"Done","type":"completed"},"assignee":{"name":"Paul"},"projectMilestone":null}
    ]}
  }}
}`))
	})

	snapshot, err := buildProjectSnapshot(client, "project-uuid")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Issues) != 3 {
		t.Fatalf("issues = %d", len(snapshot.Issues))
	}
	if got := snapshot.Issues[0].Rig; got == nil || got.ID != "mir-1697" || !got.Parked || len(got.PRs) != 1 {
		t.Fatalf("tracked join = %+v", got)
	}
	if got := snapshot.Issues[1].Rig; got == nil || got.ID != "mir-1686" {
		t.Fatalf("legacy join = %+v", got)
	}
	if snapshot.Issues[2].Rig != nil || snapshot.Issues[2].State.Type != "completed" {
		t.Fatalf("unrigged completed issue = %+v", snapshot.Issues[2])
	}
	blob, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), `"nodes"`) {
		t.Errorf("snapshot duplicated raw project issue nodes: %s", blob)
	}
}

func TestProjectRigIDIsReadableAndBounded(t *testing.T) {
	got := projectRigID("A Very Long Project Name That Keeps Going Well Past the Filesystem Naming Budget for a Rig")
	if !strings.HasPrefix(got, "project-") || len(got) > 60 {
		t.Fatalf("project rig id = %q", got)
	}
}
