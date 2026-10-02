package main

import (
	"fmt"
	"os"
	"strings"
)

type linearIssueProject struct {
	Identifier string `json:"identifier"`
	Project    *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"project"`
}

// relayDeliver is the send underneath relay, a seam so tests can stand in for
// a live agent session.
var relayDeliver = deliverRigMessage

// runRelay sends a discovery from a task rig to the overview rig for its
// Linear project. Relay owns only the addressing: a task agent knows its
// issue, not which local rig coordinates that issue's project, so this
// resolves one to the other and hands the text to the same delivery `rig send`
// uses. It deliberately does not write to Linear: the overview agent can
// connect the discovery to sibling issues and draft the durable external
// update with a human in the loop.
//
// It used to post to the notify store, which reached the project agent only
// when it next read `rig project status`, so a discovery could sit unread for
// a day. Delivery now lands at the agent's next turn boundary and fails loudly
// when the project rig isn't running, which is the answer the task agent
// needs to hear rather than a note filed where nobody is looking.
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
	text := fmt.Sprintf("Discovery from %s (via rig relay):\n%s", issue.Identifier, message)
	if err := relayDeliver(*overview, text, "", currentSender()); err != nil {
		return fmt.Errorf("relaying to %s: %w", overview.ID, err)
	}
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
