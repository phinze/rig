package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type linearIssueProject struct {
	Identifier string `json:"identifier"`
	Project    *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"project"`
}

// runRelay sends a private, local discovery from a task rig to the overview
// rig for its Linear project. It deliberately does not write to Linear: the
// overview agent can connect the discovery to sibling issues and draft the
// durable external update with a human in the loop.
func runRelay(args []string) error {
	message := strings.TrimSpace(strings.Join(args, " "))
	if message == "" {
		return fmt.Errorf("usage: rig relay <project-relevant discovery>")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	basedir, err := findBasedir(cwd)
	if err != nil {
		return err
	}
	m, err := readManifest(basedir)
	if err != nil {
		return err
	}
	if m.isProject() {
		return fmt.Errorf("relay starts in a task rig, not the project overview itself")
	}
	issueID := m.TrackerID
	if m.Tracker != "linear" || issueID == "" {
		legacy := strings.ToUpper(m.ID)
		if !linearIDRe.MatchString(legacy) {
			return fmt.Errorf("current rig has no Linear issue identity")
		}
		issueID = legacy
	}

	client, err := newLinearClient()
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("no Linear API token found")
	}
	issue, err := queryLinearIssueProject(client, issueID)
	if err != nil {
		return err
	}
	if issue.Project == nil {
		return fmt.Errorf("%s is not assigned to a Linear project", issueID)
	}
	rigs, err := listRigs()
	if err != nil {
		return err
	}
	var overview *rigInfo
	for i := range rigs {
		if rigs[i].Kind == "project" && rigs[i].Tracker == "linear" && rigs[i].TrackerID == issue.Project.ID {
			overview = &rigs[i]
			break
		}
	}
	if overview == nil {
		return fmt.Errorf("no project rig for %s; run `rig project %s` first", issue.Project.Name, shellQuote(issue.Project.Name))
	}
	key := "relay-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := runNotifyPost([]string{
		"--source", "rig:" + strings.ToLower(issue.Identifier),
		"--key", key,
		"--title", "Discovery from " + issue.Identifier,
		"--body", message,
		"--rig", overview.ID,
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rig: relayed %s discovery to %s\n", issue.Identifier, overview.ID)
	return nil
}

func queryLinearIssueProject(client *linearClient, issueID string) (linearIssueProject, error) {
	var data struct {
		Issue linearIssueProject `json:"issue"`
	}
	query := `query IssueProject($id: String!) {
  issue(id: $id) { identifier project { id name url } }
}`
	if err := client.query(query, map[string]any{"id": issueID}, &data); err != nil {
		return linearIssueProject{}, err
	}
	if data.Issue.Identifier == "" {
		return linearIssueProject{}, fmt.Errorf("Linear issue %s not found", issueID)
	}
	return data.Issue, nil
}
