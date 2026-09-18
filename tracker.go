package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type task struct {
	Identifier string // e.g. "MIR-75", "PERS-3", "phinze/rig#9"
	Title      string
	BranchName string // e.g. "phinze/mir-75-add-zig-stack"
	// Source is the tracker the task came from. Blank reads as Linear: the
	// PR-link lookup builds tasks without one, and every rig made before there
	// was a second source is a Linear rig.
	Source taskSource
	URL    string // the task's page, recorded as the manifest's tracker_url
	Repo   string // GitHub only: the owner/repo the issue lives in
	Domain string // Vikunja only: the personal-tasks domain that holds it
}

func (t task) source() taskSource {
	if t.Source == "" {
		return sourceLinear
	}
	return t.Source
}

var linearIDRe = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)

// resolveIssueID turns `rig up` args into a task reference. An exact
// identifier is used directly: a GitHub issue by `owner/repo#n` or URL, and a
// `TEAM-123` shape as either Linear or a personal task, which resolveTask
// tells apart later so the lookup isn't spent on a re-up. Anything else
// (including no args) opens a live fzf picker whose list is a fresh search of
// the current source re-run on Tab, seeded with whatever query the args spelled
// out; ctrl-t cycles the source. source is the --source flag, or "" for the
// default. Returns a zero ref (no error) when the user cancels the picker.
func resolveIssueID(args []string, pick *agentPick, source taskSource) (taskRef, error) {
	if len(args) == 1 {
		if ref := parseGitHubIssueRef(args[0]); ref != nil {
			return taskRef{source: sourceGitHub, id: ref.identifier()}, nil
		}
		if linearIDRe.MatchString(args[0]) {
			return taskRef{source: source, id: args[0]}, nil
		}
	}

	// fzf shells out to `rig __issues STATE {q}` on Tab, so the candidate list is
	// whatever the current source returns for the current query rather than a
	// fuzzy filter over one frozen fetch. Point it at our own binary so row
	// formatting stays in one place (runIssueRows).
	exe, err := os.Executable()
	if err != nil {
		return taskRef{}, fmt.Errorf("locating rig binary: %w", err)
	}
	src := &sourcePick{source: sourceLinear}
	if source != "" {
		src.source = source
	}
	if err := src.prepare(); err != nil {
		return taskRef{}, fmt.Errorf("preparing issue picker: %w", err)
	}
	defer src.cleanup()
	reloadCmd := shellQuote(exe) + " __issues " + shellQuote(src.statePath) + " {q}"

	sel, err := fzfLiveSelect(reloadCmd, issuePrompt(src.source), strings.Join(args, " "), pick, src.fzfArgs(exe, reloadCmd)...)
	if err != nil {
		return taskRef{}, err
	}
	if sel == "" {
		return taskRef{}, nil
	}
	return parseIssueSelection(sel)
}

// parseIssueSelection reads the row fzf handed back. A row with no identifier
// is the error row issueRows prints when a source can't be reached, and
// picking it fails with that message rather than quietly cancelling, so the
// reason lands on stderr where you're already looking.
func parseIssueSelection(sel string) (taskRef, error) {
	cols := strings.Split(sel, "\t")
	ref := taskRef{id: strings.TrimSpace(cols[0])}
	if ref.id == "" {
		msg := "picker returned an empty row"
		if len(cols) >= 3 && cols[2] != "" {
			msg = cols[2]
		}
		return taskRef{}, fmt.Errorf("%s", msg)
	}
	if len(cols) >= 4 {
		if s, err := parseTaskSource(cols[3]); err == nil {
			ref.source = s
		}
	}
	return ref, nil
}

// runIssueRows backs the live issue picker (the hidden `rig __issues` command
// fzf shells out to). Its first argument is the picker's source state file;
// the rest is the query. It prints tab-delimited Identifier\tState\tTitle\tSource
// rows — empty query lists each source's default open set, anything else feeds
// that source's search.
func runIssueRows(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: rig __issues STATEFILE [QUERY...]")
	}
	source, ok := readSourceState(args[0])
	if !ok {
		source = sourceLinear
	}
	query := strings.TrimSpace(strings.Join(args[1:], " "))
	_, _ = os.Stdout.WriteString(issueRows(source, query))
	return nil
}

// issueRows renders the picker's rows for one source and query. Because fzf
// runs this on a keypress, a lookup failure can't go to stderr without
// splatting over the UI; it used to yield no rows at all, which made a tracker
// outage indistinguishable from having nothing to pick up. It now yields one
// row with the error where the rows would be: a blank identifier (so
// parseIssueSelection knows to refuse it), the state `error`, and the message
// as the title.
func issueRows(source taskSource, query string) string {
	cands, err := fetchSourceIssues(source, query, 25)
	if err != nil {
		return fmt.Sprintf("\terror\t%s: %s\t%s\n", source.label(), oneLine(err.Error()), source)
	}
	var b strings.Builder
	for _, c := range cands {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", c.Identifier, c.State, c.Title, source)
	}
	return b.String()
}

// oneLine flattens an error message onto a single row: a newline would become
// a second, identifier-less row and a tab would shift the columns.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// fetchSourceIssues is the picker's one fan-out point over sources.
func fetchSourceIssues(source taskSource, query string, limit int) ([]issueCandidate, error) {
	switch source {
	case sourceGitHub:
		return fetchGitHubIssues(query, limit)
	case sourceVikunja:
		return fetchVikunjaTasks(query)
	}
	return fetchLinearIssues(query, limit)
}

type issueCandidate struct {
	Identifier string
	State      string
	Title      string
}

func fetchLinearIssues(search string, limit int) ([]issueCandidate, error) {
	client, err := newLinearClient()
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("no Linear API token found")
	}
	var data struct {
		Issues struct {
			Nodes []linearIssueNode `json:"nodes"`
		} `json:"issues"`
		SearchIssues struct {
			Nodes []linearIssueNode `json:"nodes"`
		} `json:"searchIssues"`
	}
	query := `query IssuePicker($first: Int!) {
  issues(
    first: $first
    orderBy: updatedAt
    filter: {
      state: { type: { neq: "completed" } }
      or: [
        { assignee: { null: true } }
        { assignee: { isMe: { eq: true } } }
      ]
    }
  ) { nodes { identifier title state { name } } }
}`
	variables := map[string]any{"first": limit}
	if search != "" {
		query = `query IssueSearch($term: String!, $first: Int!) {
  searchIssues(term: $term, first: $first, includeArchived: false) {
    nodes { identifier title state { name } }
  }
}`
		variables["term"] = search
	}
	if err := client.query(query, variables, &data); err != nil {
		return nil, err
	}
	nodes := data.Issues.Nodes
	if search != "" {
		nodes = data.SearchIssues.Nodes
	}
	cands := make([]issueCandidate, len(nodes))
	for i, node := range nodes {
		cands[i] = issueCandidate{Identifier: node.Identifier, State: node.State.Name, Title: node.Title}
	}
	return cands, nil
}

// resolveTask fetches the task a ref names. A ref with no source is a
// `TEAM-123` id that could be Linear's or a personal task's: the two mint the
// same shape, so the personal-tasks domains are asked whether the prefix is
// theirs before Linear is. That costs a few local subprocess calls, and only
// on a genuine create; an exact id that names an existing rig never gets here.
func resolveTask(ref taskRef) (task, error) {
	switch ref.source {
	case sourceGitHub:
		gh := parseGitHubIssueRef(ref.id)
		if gh == nil {
			return task{}, fmt.Errorf("%q is not a GitHub issue (want owner/repo#n or an issue url)", ref.id)
		}
		return resolveGitHubTask(*gh)
	case sourceVikunja:
		return resolveVikunjaTask(ref.id)
	}
	if !linearIDRe.MatchString(ref.id) {
		return task{}, fmt.Errorf("%q is not an issue identifier (want MIR-75, PERS-3, or owner/repo#9)", ref.id)
	}
	if ref.source == "" {
		prefix, _, _ := strings.Cut(ref.id, "-")
		if domain, err := vikunjaDomainFor(prefix); err == nil && domain != "" {
			return resolveVikunjaTask(ref.id)
		}
	}
	return resolveLinearTask(ref.id)
}

func resolveLinearTask(id string) (task, error) {
	client, err := newLinearClient()
	if err != nil {
		return task{}, err
	}
	if client == nil {
		return task{}, fmt.Errorf("no Linear API token found")
	}
	var data struct {
		Issue linearIssueNode `json:"issue"`
	}
	query := `query ResolveIssue($id: String!) {
  issue(id: $id) { identifier title branchName url }
}`
	if err := client.query(query, map[string]any{"id": id}, &data); err != nil {
		return task{}, err
	}
	if data.Issue.Identifier == "" {
		return task{}, fmt.Errorf("Linear issue %s not found", id)
	}
	if data.Issue.BranchName == "" {
		return task{}, fmt.Errorf("Linear returned no branchName for %s", id)
	}
	return task{
		Source: sourceLinear, Identifier: data.Issue.Identifier, Title: data.Issue.Title,
		BranchName: data.Issue.BranchName, URL: data.Issue.URL,
	}, nil
}

type linearIssueNode struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	BranchName string `json:"branchName"`
	URL        string `json:"url"`
	State      struct {
		Name string `json:"name"`
	} `json:"state"`
}

type linkedLinearTask struct {
	Task     task
	LinkKind string
}

const linearGraphQLEndpoint = "https://api.linear.app/graphql"

type linearClient struct {
	HTTP     *http.Client
	Endpoint string
	Token    string
}

func newLinearClient() (*linearClient, error) {
	token, err := linearAPIToken()
	if err != nil || token == "" {
		return nil, err
	}
	endpoint := os.Getenv("RIG_LINEAR_GRAPHQL_ENDPOINT")
	if endpoint == "" {
		endpoint = linearGraphQLEndpoint
	}
	return &linearClient{
		HTTP:     &http.Client{Timeout: 5 * time.Second},
		Endpoint: endpoint,
		Token:    token,
	}, nil
}

// linkedLinearTasksForPR asks Linear which issues are linked to a GitHub PR.
// GitHub integration links are exposed as issue attachments, and their URL is
// the canonical reverse-lookup key. This is stronger than inferring identity
// from either branch naming or prose in the PR body.
func linkedLinearTasksForPR(prURL string) ([]linkedLinearTask, error) {
	client, err := newLinearClient()
	if err != nil || client == nil {
		return nil, err
	}
	return queryLinkedLinearTasks(client, prURL)
}

func linearAPIToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv("LINEAR_API_TOKEN")); token != "" {
		return token, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home for Linear credentials: %w", err)
	}
	tokenPath := filepath.Join(home, ".linear_api_token")
	info, err := os.Stat(tokenPath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("checking Linear credentials: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by other users; run chmod 600 %s", tokenPath, tokenPath)
	}
	raw, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", fmt.Errorf("reading Linear credentials: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

func queryLinkedLinearTasks(client *linearClient, prURL string) ([]linkedLinearTask, error) {
	query := `query LinkedIssues($url: String!) {
  attachmentsForURL(url: $url) {
    nodes {
      metadata
      issue { identifier title branchName }
    }
  }
}`
	var data struct {
		AttachmentsForURL struct {
			Nodes []struct {
				Metadata struct {
					LinkKind string `json:"linkKind"`
				} `json:"metadata"`
				Issue linearIssueNode `json:"issue"`
			} `json:"nodes"`
		} `json:"attachmentsForURL"`
	}
	if err := client.query(query, map[string]any{"url": prURL}, &data); err != nil {
		return nil, err
	}
	linked := make([]linkedLinearTask, 0, len(data.AttachmentsForURL.Nodes))
	for _, node := range data.AttachmentsForURL.Nodes {
		if node.Issue.Identifier == "" {
			continue
		}
		linked = append(linked, linkedLinearTask{
			Task: task{
				Identifier: node.Issue.Identifier,
				Title:      node.Issue.Title,
				BranchName: node.Issue.BranchName,
			},
			LinkKind: node.Metadata.LinkKind,
		})
	}
	return linked, nil
}

func (client *linearClient) query(query string, variables map[string]any, data any) error {
	requestBody := struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{Query: query, Variables: variables}
	raw, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("encoding Linear query: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, client.Endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("building Linear query: %w", err)
	}
	req.Header.Set("Authorization", client.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("querying Linear: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading Linear response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Linear returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var response struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("parsing Linear response: %w", err)
	}
	if len(response.Errors) > 0 {
		return fmt.Errorf("Linear: %s", response.Errors[0].Message)
	}
	if err := json.Unmarshal(response.Data, data); err != nil {
		return fmt.Errorf("parsing Linear data: %w", err)
	}
	return nil
}

// primaryLinkedLinearTask chooses the relationship that best represents the
// authoring rig when a PR links several issues. A closing link is primary,
// followed by a contributes/Part-of link, then any remaining relationship.
func primaryLinkedLinearTask(linked []linkedLinearTask) (task, bool) {
	for _, want := range []string{"closes", "contributes"} {
		for _, link := range linked {
			if link.LinkKind == want {
				return link.Task, true
			}
		}
	}
	if len(linked) > 0 {
		return linked[0].Task, true
	}
	return task{}, false
}

// rigID returns the rig's local id: the lowercased identifier, except for a
// GitHub issue, whose `<repo>-<n>` shape is taskRef's to derive.
func (t task) rigID() string {
	return taskRef{source: t.source(), id: t.Identifier}.rigID()
}

// basedirName strips any "<user>/" prefix off the branch name so the resulting
// slug is short and matches the issue: "phinze/mir-75-add-zig-stack" →
// "mir-75-add-zig-stack". Synthesized branches carry the same shape, so this
// covers every source; a task with no branch at all falls back to its own slug.
func (t task) basedirName() string {
	if t.BranchName == "" {
		return taskSlug(t.rigID(), t.Title)
	}
	return stripBranchUserPrefix(t.BranchName)
}

// workBranchName returns the branch this rig should eventually push. Linear's
// generated name contains the exact issue identifier, which makes the GitHub
// integration link the PR and drive its status before the PR body has said
// whether this change closes the issue or is merely part of it. Replacing the
// identifier's separator keeps the branch recognizable and reversible without
// presenting Linear with that exact token: "phinze/mir-75-add-zig-stack" →
// "phinze/mir_75-add-zig-stack".
func (t task) workBranchName() string {
	if t.source() != sourceLinear {
		// Only Linear links on the branch name; the synthesized branches are
		// already the ones to push.
		return t.BranchName
	}
	id := strings.ToLower(t.Identifier)
	escapedID := strings.Replace(id, "-", "_", 1)
	prefix, slug := "", t.BranchName
	if user, after, ok := strings.Cut(slug, "/"); ok {
		prefix, slug = user+"/", after
	}
	i := strings.Index(strings.ToLower(slug), id)
	if i == -1 {
		// If Linear ever changes the branch shape and omits the identifier, the
		// name already cannot trigger identifier-based linking.
		return t.BranchName
	}
	return prefix + slug[:i] + escapedID + slug[i+len(id):]
}

// stripBranchUserPrefix drops a leading "<user>/" from a branch name, the shape
// Linear and most PR branches carry ("phinze/mir-75-add-zig-stack" →
// "mir-75-add-zig-stack"). Shared by the issue flow (basedirName) and the PR
// pickup, which needs the same slug to land a resumed rig at the issue's path.
func stripBranchUserPrefix(branch string) string {
	if _, after, ok := strings.Cut(branch, "/"); ok {
		return after
	}
	return branch
}

// branchIssueIDRe matches the Linear-issue token a branch slug leads with —
// "mir-75" out of "mir-75-add-zig-stack" — mirroring linearIDRe's shape,
// lowercased (branch names are). It must be followed by a dash or the end so a
// bare "mir-75" also matches but "mir-750" (a different issue) doesn't bleed in.
var branchIssueIDRe = regexp.MustCompile(`^([a-z][a-z0-9]*-[0-9]+)(?:-|$)`)

// escapedBranchIssueIDRe recognizes the reversible issue token Rig uses for
// new Linear work branches. The underscore deliberately is not a valid Linear
// identifier separator, so it carries identity without enabling branch-based
// status automation.
var escapedBranchIssueIDRe = regexp.MustCompile(`^([a-z][a-z0-9]*)_([0-9]+)(?:-|$)`)

// leadingIssueID pulls the Linear issue id off the front of a branch slug, or
// "" when the branch didn't come from an issue. It's what lets a PR pickup
// recover the id its originating `rig up <issue>` used, so the rig rebuilds
// under the same id and basedir.
func leadingIssueID(slug string) string {
	m := branchIssueIDRe.FindStringSubmatch(slug)
	if m == nil {
		return ""
	}
	return m[1]
}

// leadingEscapedIssueID reverses Rig's branch-safe issue token ("mir_75")
// back into the Linear identifier used by the rig ("mir-75").
func leadingEscapedIssueID(slug string) string {
	m := escapedBranchIssueIDRe.FindStringSubmatch(slug)
	if m == nil {
		return ""
	}
	return m[1] + "-" + m[2]
}

// restoreIssueSlug turns a Rig work-branch slug back into the original Linear
// basedir slug. This preserves the cwd agent sessions use for history when an
// authoring PR is picked up after its local rig was removed.
func restoreIssueSlug(slug string) string {
	if leadingEscapedIssueID(slug) == "" {
		return slug
	}
	return strings.Replace(slug, "_", "-", 1)
}
