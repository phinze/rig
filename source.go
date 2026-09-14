package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// taskSource is where a `rig up` task comes from. Linear is work; GitHub is
// issues on your own repos; Vikunja is the personal tracker behind the
// `personal-tasks` helper. The string is also the manifest's `tracker` value,
// which is why the Vikunja one is spelled by product rather than by the
// helper's name: the manifest records what the id means, not how it was
// fetched.
type taskSource string

const (
	sourceLinear  taskSource = "linear"
	sourceGitHub  taskSource = "github"
	sourceVikunja taskSource = "vikunja"
)

// taskSources is the cycle order for the issue picker: Linear first because
// that's the default and the one `rig up` has always meant.
var taskSources = []taskSource{sourceLinear, sourceGitHub, sourceVikunja}

// sourceCycleKey advances the picker's source. ctrl-o is the agent bar, and
// fzf leaves ctrl-r, s, t, v, x and z unbound; t reads as "tracker". The radar
// spends ctrl-t on showing torn-down rigs, but the two never share a screen,
// and ctrl-s (the honest mnemonic) is XOFF on any terminal without `-ixon`,
// which is a worse surprise than an overloaded letter.
const sourceCycleKey = "ctrl-t"

// label is the short name the picker prompt and --source use. Vikunja goes by
// "tasks" there, matching the `personal-tasks` helper you'd reach for by hand.
func (s taskSource) label() string {
	if s == sourceVikunja {
		return "tasks"
	}
	return string(s)
}

func (s taskSource) next() taskSource {
	for i, cand := range taskSources {
		if cand == s {
			return taskSources[(i+1)%len(taskSources)]
		}
	}
	return taskSources[0]
}

// parseTaskSource accepts the manifest spelling, the picker label, and the
// helper's name, since all three are things you'd plausibly type.
func parseTaskSource(name string) (taskSource, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "linear":
		return sourceLinear, nil
	case "github", "gh":
		return sourceGitHub, nil
	case "vikunja", "tasks", "personal-tasks":
		return sourceVikunja, nil
	}
	return "", fmt.Errorf("unknown source %q (want linear, github, or tasks)", name)
}

// issuePrompt is the picker prompt for a source. The source rides in the
// prompt rather than in a header bar because the prompt is the line you're
// typing into, and "which tracker am I searching" is exactly the prompt's
// question. It also keeps the header block the agent bar's own: `rig __agent
// cycle` reprints that block from a fixed hint, so a source bar there would
// freeze at whatever it said when the picker opened.
func issuePrompt(s taskSource) string {
	return "Pick issue [" + s.label() + "]: "
}

// sourcePick is the source choice as it travels through the issue picker. Like
// agentPick, it round-trips through a temp file because fzf can only hand state
// back that way: ctrl-t shells out to `rig __source cycle STATE`, which advances
// the file and prints the new prompt, and the reload that follows reads the
// file to know which tracker to search. Unlike agentPick there is no sync back
// after the picker exits: every row is stamped with the source that produced
// it, so the selection says where it came from on its own.
type sourcePick struct {
	source    taskSource
	statePath string
}

// prepare creates the state file. It's separate from fzfArgs because the
// reload command has to name the file, and the bind has to carry the reload.
func (p *sourcePick) prepare() error {
	f, err := os.CreateTemp("", "rig-source-*")
	if err != nil {
		return err
	}
	p.statePath = f.Name()
	_ = f.Close()
	return os.WriteFile(p.statePath, []byte(string(p.source)+"\n"), 0o600)
}

// fzfArgs returns the ctrl-t bind: advance the file and redraw the prompt, then
// run reloadCmd (the row command with fzf's {q} placeholder, which reads the
// file) so the rows follow the prompt.
func (p *sourcePick) fzfArgs(exe, reloadCmd string) []string {
	cycle := fmt.Sprintf("%s __source cycle %s", shellQuote(exe), shellQuote(p.statePath))
	return []string{
		"--bind=" + sourceCycleKey + ":transform-prompt(" + cycle + ")+reload(" + reloadCmd + ")",
	}
}

func (p *sourcePick) cleanup() {
	if p.statePath == "" {
		return
	}
	_ = os.Remove(p.statePath)
	p.statePath = ""
}

func readSourceState(path string) (taskSource, bool) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	s, err := parseTaskSource(string(blob))
	if err != nil {
		return "", false
	}
	return s, true
}

// cycleSourceState advances the source recorded at path and returns the prompt
// fzf should redraw.
func cycleSourceState(path string) (string, error) {
	s := sourceLinear
	if cur, ok := readSourceState(path); ok {
		s = cur
	}
	s = s.next()
	if err := os.WriteFile(path, []byte(string(s)+"\n"), 0o600); err != nil {
		return "", err
	}
	return issuePrompt(s), nil
}

// runSourcePickCmd backs the hidden `rig __source` subcommand fzf's
// transform-prompt shells out to.
func runSourcePickCmd(args []string) error {
	if len(args) != 2 || args[0] != "cycle" {
		return fmt.Errorf("usage: rig __source cycle STATEFILE")
	}
	prompt, err := cycleSourceState(args[1])
	if err != nil {
		return err
	}
	fmt.Println(prompt)
	return nil
}

// taskRef is a task named but not yet resolved: enough to find an existing rig
// for it without touching the network, which is what keeps re-upping into work
// you already have as instant as `rig switch`. A blank source means the id's
// shape decides at resolve time; see resolveTask.
type taskRef struct {
	source taskSource
	id     string // MIR-75, PERS-3, or owner/repo#9
}

// rigID is the local id the resolved task will carry, derived without a lookup.
// Linear and Vikunja ids lowercase directly. A GitHub issue becomes
// `<repo>-<n>`: the number alone is unique only per repo, and `pr-<n>` is
// already rig's reserved shape for a PR-derived rig.
func (r taskRef) rigID() string {
	if ref := parseGitHubIssueRef(r.id); ref != nil {
		return ref.rigID()
	}
	return strings.ToLower(r.id)
}

// githubIssueRef is a GitHub issue named by repo and number.
type githubIssueRef struct {
	Owner  string
	Repo   string
	Number int
}

var (
	githubIssueShortRe = regexp.MustCompile(`^([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)#([0-9]+)$`)
	githubIssueURLRe   = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/issues/([0-9]+)/?$`)
)

// parseGitHubIssueRef reads the two exact spellings of a GitHub issue:
// `owner/repo#9` (what the picker rows carry and what gh prints) and the
// issue's URL. A bare `#9` is deliberately not one of them; it would need cwd
// to name the repo, and `rig up` no longer assumes the checkout you're standing
// in is the one the task is for.
func parseGitHubIssueRef(s string) *githubIssueRef {
	m := githubIssueShortRe.FindStringSubmatch(s)
	if m == nil {
		m = githubIssueURLRe.FindStringSubmatch(s)
	}
	if m == nil {
		return nil
	}
	n, _ := strconv.Atoi(m[3])
	return &githubIssueRef{Owner: m[1], Repo: m[2], Number: n}
}

func (r githubIssueRef) identifier() string {
	return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number)
}

func (r githubIssueRef) rigID() string {
	return fmt.Sprintf("%s-%d", strings.ToLower(r.Repo), r.Number)
}

func (r githubIssueRef) url() string {
	return fmt.Sprintf("https://github.com/%s/%s/issues/%d", r.Owner, r.Repo, r.Number)
}

// ghIssueNode is the JSON `gh` returns for one issue, from both `gh search
// issues` and `gh issue view`.
type ghIssueNode struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	State      string `json:"state"`
	URL        string `json:"url"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

// fetchGitHubIssues lists open issues across the repos you own, which is the
// whole of what the GitHub source means: work tracked elsewhere (Linear) has
// its own source, and issues on other people's projects arrive as PRs or
// reviews, not pickups. An empty query lists the most recently updated.
func fetchGitHubIssues(search string, limit int) ([]issueCandidate, error) {
	login, err := ghCurrentLogin()
	if err != nil {
		return nil, err
	}
	args := []string{"search", "issues",
		"--owner=" + login, "--state=open", "--sort=updated",
		"--limit=" + strconv.Itoa(limit),
		"--json=repository,number,title,state",
	}
	if search != "" {
		args = append(args, "--", search)
	}
	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		return nil, execError("gh search issues", err)
	}
	return parseGitHubIssueRows(out)
}

func parseGitHubIssueRows(raw []byte) ([]issueCandidate, error) {
	var nodes []ghIssueNode
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil, fmt.Errorf("parsing gh search issues: %w", err)
	}
	cands := make([]issueCandidate, 0, len(nodes))
	for _, n := range nodes {
		owner, repo, ok := strings.Cut(n.Repository.NameWithOwner, "/")
		if !ok {
			continue
		}
		ref := githubIssueRef{Owner: owner, Repo: repo, Number: n.Number}
		cands = append(cands, issueCandidate{Identifier: ref.identifier(), State: strings.ToLower(n.State), Title: n.Title})
	}
	return cands, nil
}

// resolveGitHubTask fetches one issue and shapes it as a task. The branch is
// synthesized, since GitHub has no branchName of its own: `<login>/<rig id>-
// <slug>`, the same shape Linear mints, unescaped because GitHub links PRs to
// issues from the body and never from the branch.
func resolveGitHubTask(ref githubIssueRef) (task, error) {
	out, err := exec.Command("gh", "issue", "view", strconv.Itoa(ref.Number),
		"--repo", ref.Owner+"/"+ref.Repo, "--json", "number,title,state,url").Output()
	if err != nil {
		return task{}, execError("gh issue view", err)
	}
	var node ghIssueNode
	if err := json.Unmarshal(out, &node); err != nil {
		return task{}, fmt.Errorf("parsing gh issue view: %w", err)
	}
	if node.Number == 0 {
		return task{}, fmt.Errorf("GitHub issue %s not found", ref.identifier())
	}
	tk := task{
		Source:     sourceGitHub,
		Identifier: ref.identifier(),
		Title:      node.Title,
		URL:        node.URL,
		Repo:       ref.Owner + "/" + ref.Repo,
	}
	tk.BranchName = synthesizedBranch(ref.rigID(), node.Title)
	return tk, nil
}

// synthesizedBranch is the work branch for a task whose tracker mints none.
// The login prefix mirrors Linear's shape so basedirName can strip it the same
// way; a gh that can't say who you are drops the prefix rather than the branch.
func synthesizedBranch(rigID, title string) string {
	slug := taskSlug(rigID, title)
	login, err := ghCurrentLogin()
	if err != nil || login == "" {
		return slug
	}
	return login + "/" + slug
}

func execError(what string, err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("%s: %s", what, strings.TrimSpace(string(ee.Stderr)))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// vikunjaDomains lists the `personal-tasks` domains, which are the
// subdirectories of the helper's config dir (nix writes one per project). The
// helper is the only thing that knows how to reach Vikunja, so rig goes through
// it rather than learning the server, the token, or the project ids itself.
func vikunjaDomains() ([]string, error) {
	dir := os.Getenv("PERSONAL_TASKS_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".config", "personal-tasks")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("listing personal-tasks domains: %w", err)
	}
	var domains []string
	for _, e := range entries {
		if e.IsDir() {
			domains = append(domains, e.Name())
		}
	}
	sort.Strings(domains)
	return domains, nil
}

// vikunjaTaskNode is the JSON veans prints for one task, from `list` and `show`.
type vikunjaTaskNode struct {
	ID         int    `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	Done       bool   `json:"done"`
	Buckets    []struct {
		Title string `json:"title"`
	} `json:"buckets"`
}

// vikunjaContext is the part of `personal-tasks <domain> context` rig reads:
// the project's identifier prefix, for routing an exact id to its domain, and
// the server, for the task's URL.
type vikunjaContext struct {
	Server            string `json:"server"`
	ProjectIdentifier string `json:"project_identifier"`
}

func personalTasks(domain string, args ...string) ([]byte, error) {
	out, err := exec.Command("personal-tasks", append([]string{domain}, args...)...).Output()
	if err != nil {
		return nil, execError("personal-tasks "+domain+" "+args[0], err)
	}
	return out, nil
}

func vikunjaContextFor(domain string) (vikunjaContext, error) {
	out, err := personalTasks(domain, "context")
	if err != nil {
		return vikunjaContext{}, err
	}
	var ctx vikunjaContext
	if err := json.Unmarshal(out, &ctx); err != nil {
		return vikunjaContext{}, fmt.Errorf("parsing personal-tasks %s context: %w", domain, err)
	}
	return ctx, nil
}

// fetchVikunjaTasks lists open tasks across every domain. The search is
// server-side on the title, which is what veans exposes; three small calls
// rather than one because each domain is its own project and the helper
// scopes by domain on purpose.
func fetchVikunjaTasks(search string) ([]issueCandidate, error) {
	domains, err := vikunjaDomains()
	if err != nil {
		return nil, err
	}
	filter := "done = false"
	if search != "" {
		// Vikunja's filter grammar quotes with single quotes and has no escape
		// for one inside; dropping them costs a query character, not the picker.
		filter += " && title like '%" + strings.ReplaceAll(search, "'", "") + "%'"
	}
	var cands []issueCandidate
	for _, domain := range domains {
		out, err := personalTasks(domain, "list", "--filter", filter)
		if err != nil {
			return nil, err
		}
		rows, err := parseVikunjaTaskRows(out)
		if err != nil {
			return nil, err
		}
		cands = append(cands, rows...)
	}
	return cands, nil
}

func parseVikunjaTaskRows(raw []byte) ([]issueCandidate, error) {
	var nodes []vikunjaTaskNode
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil, fmt.Errorf("parsing personal-tasks list: %w", err)
	}
	cands := make([]issueCandidate, 0, len(nodes))
	for _, n := range nodes {
		cands = append(cands, issueCandidate{Identifier: n.Identifier, State: n.state(), Title: n.Title})
	}
	return cands, nil
}

func (n vikunjaTaskNode) state() string {
	if n.Done {
		return "done"
	}
	if len(n.Buckets) > 0 && n.Buckets[0].Title != "" {
		return n.Buckets[0].Title
	}
	return "open"
}

// vikunjaDomainFor finds which domain mints ids with the given prefix
// ("PERS" → personal), or "" when none does. It's what routes an exact
// `rig up PERS-3` away from Linear, whose ids look the same.
func vikunjaDomainFor(prefix string) (string, error) {
	domains, err := vikunjaDomains()
	if err != nil {
		return "", err
	}
	for _, domain := range domains {
		ctx, err := vikunjaContextFor(domain)
		if err != nil {
			return "", err
		}
		if strings.EqualFold(ctx.ProjectIdentifier, prefix) {
			return domain, nil
		}
	}
	return "", nil
}

// resolveVikunjaTask fetches one task by identifier and shapes it as a task.
// The branch is synthesized as for GitHub; the URL uses the task's numeric id,
// since the identifier is a per-project index that the web UI doesn't route on.
func resolveVikunjaTask(id string) (task, error) {
	prefix, _, ok := strings.Cut(id, "-")
	if !ok {
		return task{}, fmt.Errorf("%q is not a personal-tasks identifier (want PREFIX-N)", id)
	}
	domain, err := vikunjaDomainFor(prefix)
	if err != nil {
		return task{}, err
	}
	if domain == "" {
		return task{}, fmt.Errorf("no personal-tasks domain mints %s ids", prefix)
	}
	out, err := personalTasks(domain, "show", id)
	if err != nil {
		return task{}, err
	}
	var node vikunjaTaskNode
	if err := json.Unmarshal(out, &node); err != nil {
		return task{}, fmt.Errorf("parsing personal-tasks show: %w", err)
	}
	if node.Identifier == "" {
		return task{}, fmt.Errorf("personal task %s not found", id)
	}
	ctx, err := vikunjaContextFor(domain)
	if err != nil {
		return task{}, err
	}
	tk := task{
		Source:     sourceVikunja,
		Identifier: node.Identifier,
		Title:      node.Title,
		URL:        strings.TrimRight(ctx.Server, "/") + "/tasks/" + strconv.Itoa(node.ID),
		Domain:     domain,
	}
	tk.BranchName = synthesizedBranch(tk.rigID(), node.Title)
	return tk, nil
}
