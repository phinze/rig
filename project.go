package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// linearProjectCandidate is the cheap shape used by the creation picker.
// Project UUID is the durable identity: human project identifiers are an
// optional Linear workspace feature and cannot be assumed to resolve.
type linearProjectCandidate struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Status struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"status"`
}

type linearProjectProgress struct {
	ScopeCount           int `json:"scopeCount"`
	StartedIssueCount    int `json:"startedIssueCount"`
	CompletedIssueCount  int `json:"completedIssueCount"`
	AddedIssueCountToday int `json:"addedIssueCountToday"`
}

type linearProjectUpdate struct {
	CreatedAt time.Time `json:"createdAt"`
	Body      string    `json:"body"`
	Health    string    `json:"health"`
	URL       string    `json:"url"`
	User      struct {
		Name string `json:"name"`
	} `json:"user"`
}

type linearProjectIssue struct {
	Identifier string    `json:"identifier"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Priority   int       `json:"priority"`
	Estimate   *float64  `json:"estimate"`
	State      struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Assignee *struct {
		Name string `json:"name"`
	} `json:"assignee"`
	ProjectMilestone *struct {
		Name string `json:"name"`
	} `json:"projectMilestone"`
}

type linearProjectDetail struct {
	ID              string                `json:"id"`
	Name            string                `json:"name"`
	Description     string                `json:"description"`
	URL             string                `json:"url"`
	Health          string                `json:"health"`
	UpdatedAt       time.Time             `json:"updatedAt"`
	StartDate       string                `json:"startDate"`
	TargetDate      string                `json:"targetDate"`
	Progress        float64               `json:"progress"`
	Scope           float64               `json:"scope"`
	CurrentProgress linearProjectProgress `json:"currentProgress"`
	Status          struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"status"`
	Lead *struct {
		Name string `json:"name"`
	} `json:"lead"`
	LastUpdate *linearProjectUpdate `json:"lastUpdate"`
}

type linearProjectDetailQuery struct {
	linearProjectDetail
	Issues struct {
		Nodes    []linearProjectIssue `json:"nodes"`
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
	} `json:"issues"`
}

type projectIssueSnapshot struct {
	linearProjectIssue
	Rig *rigStatus `json:"rig,omitempty"`
}

type projectSnapshot struct {
	GeneratedAt time.Time              `json:"generated_at"`
	Project     linearProjectDetail    `json:"project"`
	Issues      []projectIssueSnapshot `json:"issues"`
	Inbox       []notification         `json:"inbox,omitempty"`
}

var uuidRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// runProject is both the project-rig constructor and its local status surface.
// Creation mirrors `rig up`: an explicit project is idempotent, while an
// ambiguous or absent query opens a picker carrying the ordinary agent bar.
func runProject(args []string) error {
	if len(args) > 0 && args[0] == "status" {
		return runProjectStatus(args[1:])
	}
	pick, args, err := extractAgentFlag(args)
	if err != nil {
		return err
	}
	defer pick.cleanup()

	client, err := newLinearClient()
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("no Linear API token found")
	}
	project, err := resolveLinearProject(client, strings.TrimSpace(strings.Join(args, " ")), pick)
	if err != nil || project == nil {
		return err
	}
	if done, err := attachExistingProjectRig(project.ID); err != nil || done {
		return err
	}
	if ok, err := pick.ensurePicked(); err != nil || !ok {
		return err
	}

	rigID := projectRigID(project.Name)
	basedir, err := basedirPath(rigID)
	if err != nil {
		return err
	}
	m := manifest{
		ID: rigID, Title: project.Name, Kind: "project", Agent: string(pick.kind),
		Tracker: "linear", TrackerID: project.ID, TrackerURL: project.URL,
	}
	if err := createBasedir(basedir, m); err != nil {
		return err
	}
	if err := writeRigAgentInstructions(basedir, m); err != nil {
		return fmt.Errorf("writing project rig instructions: %w", err)
	}
	session, err := spawnProjectSession(basedir, sessionSpec{
		agent:  pick.kind,
		prompt: projectKickoff(project.Name),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rig: project %s — %s\n", project.Name, basedir)
	return attachOrReport(session)
}

func projectKickoff(name string) string {
	return fmt.Sprintf(
		"You are coordinating the Linear project %q. Start with `rig project status --format=json`. Reconcile what is done, stale in Linear, missing, or blocked against the live task rigs, then propose the next actions needed to finish the project. Confirm those actions with me before acting on another rig or shared project state. Draft any Linear or GitHub writes before posting them.",
		name,
	)
}

func projectRigID(name string) string { return taskSlug("project", name) }

func attachExistingProjectRig(projectID string) (bool, error) {
	rigs, err := listRigs()
	if err != nil {
		return false, err
	}
	for _, r := range rigs {
		if r.Kind == "project" && r.Tracker == "linear" && r.TrackerID == projectID {
			return true, activateRig(r)
		}
	}
	return false, nil
}

func resolveLinearProject(client *linearClient, query string, pick *agentPick) (*linearProjectCandidate, error) {
	if uuidRe.MatchString(query) {
		return queryLinearProjectCandidate(client, query)
	}
	search := query
	if strings.HasPrefix(query, "https://linear.app/") {
		search = "" // URLs are matched exactly against the ordinary project list.
	}
	candidates, err := queryLinearProjects(client, search, 100)
	if err != nil {
		return nil, err
	}
	for i := range candidates {
		if candidates[i].ID == query || candidates[i].URL == query || strings.EqualFold(candidates[i].Name, query) {
			return &candidates[i], nil
		}
	}
	if len(candidates) == 1 && query != "" {
		return &candidates[0], nil
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no Linear project matches %q", query)
	}
	rows := make([]string, 0, len(candidates))
	for _, p := range candidates {
		rows = append(rows, strings.Join([]string{p.Name, p.Status.Name, p.URL, p.ID}, "\t"))
	}
	selected, err := fzfSelect(rows, "Pick project: ", pick)
	if err != nil {
		if !stdinIsTTY() {
			names := make([]string, 0, min(5, len(candidates)))
			for _, p := range candidates[:min(5, len(candidates))] {
				names = append(names, p.Name)
			}
			return nil, fmt.Errorf("Linear project query %q is ambiguous (%s); pass an exact name, URL, or UUID", query, strings.Join(names, ", "))
		}
		return nil, err
	}
	if selected == "" {
		return nil, nil
	}
	fields := strings.Split(selected, "\t")
	if len(fields) < 4 {
		return nil, fmt.Errorf("unexpected project picker result %q", selected)
	}
	for i := range candidates {
		if candidates[i].ID == fields[3] {
			return &candidates[i], nil
		}
	}
	return nil, fmt.Errorf("selected Linear project disappeared")
}

func queryLinearProjects(client *linearClient, search string, limit int) ([]linearProjectCandidate, error) {
	var data struct {
		Projects struct {
			Nodes []linearProjectCandidate `json:"nodes"`
		} `json:"projects"`
		SearchProjects struct {
			Nodes []linearProjectCandidate `json:"nodes"`
		} `json:"searchProjects"`
	}
	query := `query Projects($first: Int!) {
  projects(first: $first, orderBy: updatedAt) {
    nodes { id name url status { name type } }
  }
}`
	variables := map[string]any{"first": limit}
	if search != "" {
		query = `query SearchProjects($term: String!, $first: Int!) {
  searchProjects(term: $term, first: $first, includeArchived: false) {
    nodes { id name url status { name type } }
  }
}`
		variables["term"] = search
	}
	if err := client.query(query, variables, &data); err != nil {
		return nil, err
	}
	if search != "" {
		return data.SearchProjects.Nodes, nil
	}
	return data.Projects.Nodes, nil
}

func queryLinearProjectCandidate(client *linearClient, id string) (*linearProjectCandidate, error) {
	var data struct {
		Project *linearProjectCandidate `json:"project"`
	}
	query := `query Project($id: String!) {
  project(id: $id) { id name url status { name type } }
}`
	if err := client.query(query, map[string]any{"id": id}, &data); err != nil {
		return nil, err
	}
	if data.Project == nil {
		return nil, fmt.Errorf("Linear project %s not found", id)
	}
	return data.Project, nil
}

func queryLinearProjectDetail(client *linearClient, id string) (linearProjectDetail, []linearProjectIssue, error) {
	query := `query ProjectOverview($id: String!, $after: String) {
  project(id: $id) {
    id name description url health updatedAt startDate targetDate progress scope
    status { name type }
    lead { name }
    currentProgress
    lastUpdate { createdAt body health url user { name } }
    issues(first: 100, after: $after) {
      nodes {
        identifier title url updatedAt priority estimate
        state { name type }
        assignee { name }
        projectMilestone { name }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`
	var project linearProjectDetail
	var issues []linearProjectIssue
	after := ""
	for {
		var data struct {
			Project linearProjectDetailQuery `json:"project"`
		}
		variables := map[string]any{"id": id, "after": nil}
		if after != "" {
			variables["after"] = after
		}
		if err := client.query(query, variables, &data); err != nil {
			return linearProjectDetail{}, nil, err
		}
		if data.Project.ID == "" {
			return linearProjectDetail{}, nil, fmt.Errorf("Linear project %s not found", id)
		}
		if project.ID == "" {
			project = data.Project.linearProjectDetail
		}
		issues = append(issues, data.Project.Issues.Nodes...)
		if !data.Project.Issues.PageInfo.HasNextPage || data.Project.Issues.PageInfo.EndCursor == "" {
			break
		}
		after = data.Project.Issues.PageInfo.EndCursor
	}
	return project, issues, nil
}

func runProjectStatus(args []string) error {
	jsonOut := false
	for _, arg := range args {
		switch arg {
		case "--format=json":
			jsonOut = true
		case "--format=table":
			jsonOut = false
		default:
			return fmt.Errorf("usage: rig project status [--format=json|table]")
		}
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
	if !m.isProject() || m.Tracker != "linear" || m.TrackerID == "" {
		return fmt.Errorf("current rig is not a Linear project rig")
	}
	client, err := newLinearClient()
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("no Linear API token found")
	}
	snapshot, err := buildProjectSnapshot(client, m.TrackerID)
	if err != nil {
		return err
	}
	if jsonOut {
		blob, err := json.MarshalIndent(snapshot, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(blob))
		return nil
	}
	return printProjectSnapshot(snapshot)
}

func buildProjectSnapshot(client *linearClient, projectID string) (projectSnapshot, error) {
	project, issues, err := queryLinearProjectDetail(client, projectID)
	if err != nil {
		return projectSnapshot{}, err
	}
	rigs, err := listRigs()
	if err != nil {
		return projectSnapshot{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return projectSnapshot{}, err
	}
	allStatuses := rigStatuses(rigs, home, time.Now())
	var overviewID string
	for _, status := range allStatuses {
		if status.Kind == "project" && status.Tracker == "linear" && status.TrackerID == projectID {
			overviewID = status.ID
			break
		}
	}
	issueIDs := make(map[string]bool, len(issues))
	for _, issue := range issues {
		issueIDs[strings.ToUpper(issue.Identifier)] = true
	}
	var matching []rigStatus
	for _, status := range allStatuses {
		if status.Kind == "project" {
			continue
		}
		trackerMatch := status.Tracker == "linear" && issueIDs[strings.ToUpper(status.TrackerID)]
		legacyMatch := status.Tracker == "" && issueIDs[strings.ToUpper(status.ID)]
		if trackerMatch || legacyMatch {
			matching = append(matching, status)
		}
	}
	enrichWithPRs(matching)
	byIssue := make(map[string]*rigStatus, len(matching))
	for i := range matching {
		key := matching[i].TrackerID
		if key == "" {
			key = matching[i].ID
		}
		byIssue[strings.ToUpper(key)] = &matching[i]
	}

	snapshot := projectSnapshot{GeneratedAt: time.Now(), Project: project}
	if overviewID != "" {
		snapshot.Inbox = notificationsForRig(activeNotifications(), overviewID)
	}
	for _, issue := range issues {
		snapshot.Issues = append(snapshot.Issues, projectIssueSnapshot{
			linearProjectIssue: issue,
			Rig:                byIssue[strings.ToUpper(issue.Identifier)],
		})
	}
	sort.SliceStable(snapshot.Issues, func(i, j int) bool {
		return snapshot.Issues[i].State.Type != "completed" && snapshot.Issues[j].State.Type == "completed"
	})
	return snapshot, nil
}

func printProjectSnapshot(snapshot projectSnapshot) error {
	p := snapshot.Project
	target := "no target"
	if p.TargetDate != "" {
		target = "target " + p.TargetDate
	}
	fmt.Printf("%s  %s  %.0f%%  %s\n", p.Name, p.Status.Name, p.Progress*100, target)
	if p.Health == "" || p.LastUpdate == nil {
		var missing []string
		if p.Health == "" {
			missing = append(missing, "health")
		}
		if p.LastUpdate == nil {
			missing = append(missing, "project update")
		}
		fmt.Fprintf(os.Stderr, "rig: no %s on record\n", strings.Join(missing, " or "))
	}
	for _, line := range notifyBanner(snapshot.Inbox) {
		fmt.Fprintln(os.Stderr, line)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, issue := range snapshot.Issues {
		rigState, prState := "no rig", "-"
		if issue.Rig != nil {
			rigState = agentMarker(*issue.Rig)
			prState = prMarker(*issue.Rig)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", issue.Identifier, issue.State.Name, rigState, prState, issue.Title)
	}
	return w.Flush()
}
