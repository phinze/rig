package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// rigClaudeMD is one of the agent-facing breadcrumbs rig drops at the basedir
// root. Unlike a static skill it can carry the live task and repo set.
const rigClaudeMDName = "CLAUDE.md"

const rigAgentsMDName = "AGENTS.md"

// rigKickoffName holds the context pasted at `rig new` time. Unlike the
// generated instruction files it is written once and never touched again: it's
// your words, not rig's, and the agent is pointed at it rather than handed it,
// so a blob of any size costs the launch prompt one path.
const rigKickoffName = "KICKOFF.md"

// writeRigKickoff records the kickoff line and its pasted context at the
// basedir root. The heading gives the blob a title when the paste itself is
// raw log or chat transcript with no shape of its own.
func writeRigKickoff(basedir, kickoff, context string) error {
	body := fmt.Sprintf("# Kickoff: %s\n\n%s\n", kickoff, strings.TrimSpace(context))
	return os.WriteFile(filepath.Join(basedir, rigKickoffName), []byte(body), 0o644)
}

func hasRigKickoff(basedir string) bool {
	_, err := os.Stat(filepath.Join(basedir, rigKickoffName))
	return err == nil
}

// writeRigClaudeMD renders the basedir CLAUDE.md from the manifest. It's
// regenerated whenever the repo set changes (initial up, every rig add) so the
// repo list never drifts. Kept deliberately thin: auto-loaded context should
// earn its tokens, so this states the rig model and the live facts and stops.
func writeRigClaudeMD(basedir string, m manifest) error {
	body := renderRigInstructions(basedir, m)
	return os.WriteFile(filepath.Join(basedir, rigClaudeMDName), []byte(body), 0o644)
}

// writeRigAgentInstructions writes the same live rig context in each agent's
// native shape. Claude and Codex use parent instruction files; Antigravity's
// workspace rules live under .agents/rules. The launch prompt also points to
// AGENTS.md explicitly, covering clients that stop discovery at the repo cwd.
func writeRigAgentInstructions(basedir string, m manifest) error {
	body := []byte(renderRigInstructions(basedir, m))
	for _, name := range []string{rigClaudeMDName, rigAgentsMDName} {
		if err := os.WriteFile(filepath.Join(basedir, name), body, 0o644); err != nil {
			return err
		}
	}
	rulesDir := filepath.Join(basedir, ".agents", "rules")
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(rulesDir, "rig.md"), body, 0o644)
}

// abbrevHome renders a path with $HOME collapsed to ~ so the generated
// instructions read the same way a person would write them. On any hiccup
// resolving home it just returns the path untouched.
func abbrevHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return path
}

func renderRigInstructions(basedir string, m manifest) string {
	if m.isProject() {
		return renderProjectRigInstructions(basedir, m)
	}
	var b strings.Builder

	heading := m.ID
	if m.Title != "" {
		heading = fmt.Sprintf("%s: %s", m.ID, m.Title)
	}
	fmt.Fprintf(&b, "# Rig %s\n\n", heading)
	b.WriteString("You are working inside a **rig**: a task-shaped workspace dedicated to this\n")
	b.WriteString("one task, holding every repo the task touches.\n\n")

	if hasRigKickoff(basedir) {
		fmt.Fprintf(&b, "- **The brief lives in `%s`** at the rig root (`../%s` from a repo):\n", rigKickoffName, rigKickoffName)
		b.WriteString("  the kickoff line plus whatever context was pasted with it. Read it before\n")
		b.WriteString("  you start.\n")
	}

	fmt.Fprintf(&b, "- **This rig is your home: `%s`.** The task's repos live below, under this\n", abbrevHome(basedir))
	b.WriteString("  directory, and that's where the work happens. Bring another repo the task\n")
	b.WriteString("  touches in with `rig add owner/repo` rather than reaching out to a checkout\n")
	b.WriteString("  elsewhere — stay inside the rig unless there's a very good contextual reason\n")
	b.WriteString("  not to.\n")

	b.WriteString("- The repos here are **jj workspaces**, not plain git checkouts. Use `jj`\n")
	b.WriteString("  (`jj st`, `jj diff`, `jj log`), not `git`. Git commands fail here with\n")
	b.WriteString("  \"not a git repository\". The `jj` skill has the full playbook.\n")
	b.WriteString("- The repo under each workspace moves while you work: other rigs share it,\n")
	b.WriteString("  bookmarks advance, and branches get force-pushed. `@` is yours alone; `@-`\n")
	b.WriteString("  and bookmark names re-resolve as the repo shifts, so when it matters that\n")
	b.WriteString("  we mean the same code, name the commit id. Read `jj op log` before\n")
	b.WriteString("  deciding something broke — a working copy that jumped is usually another\n")
	b.WriteString("  workspace's operation, not damage.\n")

	if len(m.Repos) > 0 {
		b.WriteString("- Repos in this rig:\n")
		subdirs := make([]string, 0, len(m.Repos))
		for subdir := range m.Repos {
			subdirs = append(subdirs, subdir)
		}
		sort.Strings(subdirs)
		for _, subdir := range subdirs {
			fmt.Fprintf(&b, "  - `%s` (./%s)\n", m.Repos[subdir], subdir)
		}
	}

	b.WriteString("- The main tmux window holds the task-level agent plus the currently relevant\n")
	b.WriteString("  Recto diff. Every repo has one persistent Recto; inactive ones are full-window\n")
	b.WriteString("  tabs. Use `rig recto <repo>` to pull one beside the agent, or combine the\n")
	b.WriteString("  promotion with a Recto command, e.g. `rig recto cloud focus src/app.go:42`.\n")
	b.WriteString("  Do not manipulate tmux panes directly. For an ad hoc shell, split from a\n")
	b.WriteString("  repo's Recto so tmux inherits the correct working directory.\n")
	b.WriteString("- If this work uncovers something that could change a sibling issue or the\n")
	b.WriteString("  wider Linear project, run `rig relay <discovery>`. It sends a private local\n")
	b.WriteString("  note to the project's overview rig; it does not post to Linear.\n")
	b.WriteString("- Tear the whole rig back down with `rig down`.\n\n")

	b.WriteString("<!-- generated by rig; regenerated as repos are added. edits get overwritten. -->\n")

	return b.String()
}

func renderProjectRigInstructions(basedir string, m manifest) string {
	var b strings.Builder
	heading := m.Title
	if heading == "" {
		heading = m.ID
	}
	fmt.Fprintf(&b, "# Project rig: %s\n\n", heading)
	b.WriteString("You are working inside a **project overview rig**: persistent coordination\n")
	b.WriteString("context for one Linear project. It observes and orchestrates task rigs; it\n")
	b.WriteString("does not own a code checkout or a Recto diff.\n\n")
	fmt.Fprintf(&b, "- **This rig is your home: `%s`.** Stay here while coordinating. Do not edit\n", abbrevHome(basedir))
	b.WriteString("  sibling task workspaces directly; enter or dispatch the task rig that owns\n")
	b.WriteString("  the work.\n")
	if m.TrackerURL != "" {
		fmt.Fprintf(&b, "- Linear project: %s\n", m.TrackerURL)
	}
	b.WriteString("- Start each pass with `rig project status --format=json`. It joins Linear\n")
	b.WriteString("  scope and issue state to live rig, agent, PR, review, and CI state, and\n")
	b.WriteString("  includes private discoveries relayed from task rigs in its inbox.\n")
	b.WriteString("- Treat Linear as the durable shared record. Draft comments, issue changes,\n")
	b.WriteString("  project updates, and other externally visible writing for approval before\n")
	b.WriteString("  posting it.\n")
	b.WriteString("- Use `rig switch ISSUE` when a task needs direct human attention. Keep this\n")
	b.WriteString("  rig focused on project-level health, missing work, dependencies, and flow\n")
	b.WriteString("  between issues.\n")
	b.WriteString("- To restart parked work without leaving this overview, use `rig dispatch\n")
	b.WriteString("  ISSUE <prompt>`. For review feedback, the prompt should tell the task agent\n")
	b.WriteString("  to run its address-pr-review workflow. Dispatch refuses a live agent.\n")
	b.WriteString("- Tear this overview down explicitly with `rig down` when the project no\n")
	b.WriteString("  longer needs coordination. Ordinary sweep will not collect it.\n\n")
	b.WriteString("<!-- generated by rig; regenerated from the project manifest. -->\n")
	return b.String()
}
