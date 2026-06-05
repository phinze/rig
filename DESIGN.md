# Rig: workspace tool to replace jpickup/jreview

## The want

A single tool that owns the workflow currently split across `jpickup`
and `jreview` fish functions, reshaped for where the work actually is:

- **Tasks, not branches.** The unit is a Linear issue / GH issue /
  whatever-comes-next, not a branch name. jj workspaces make this
  natural because the bookmark gets named at push time, not at
  workspace creation.
- **Multi-repo per task.** Common case stays single-repo, but the data
  model accepts "this task touches `api` and `web`" without contortion.
- **Multi-tracker.** Linear today, GH issues already a thing, GitLab /
  Jira / whatever later. Pluggable tracker shim returns
  `{id, title, primary_repo?, branch_hint?}`.
- **Multi-forge.** Decoupled from tracker concerns. Where the PR lands
  at push time is a separate axis from where the work came from.
- **Sandbox-aware basedir.** The basedir doubles as the boundary for
  yolo-claude (bwrap / `--allowed-paths` / whatever), so containment is
  structural, not bolted on.
- **Metadata + .envrc at the basedir.** A `.rig.toml` is the source of
  truth; an `.envrc` exports `RIG_ID`, `RIG_TRACKER`, `RIG_BASEDIR` so
  downstream tools (claude context, jj templates, `rig down`) read
  from one place.

## Current state

`jpickup.fish` does single-repo Linear pickup via fish, deriving the
workspace path from `~/workspaces/<host>/<owner>/<repo>/<branch>`.
`jreview.fish` is the PR-review sibling using the same path shape. The
git-flavored `review.fish` still lives alongside but is on the chopping
block (separate, simpler collapse — drop the "j" prefix once jj is the
only path).

Path shape today bakes the branch name into the directory, which is a
git-worktree habit. jj doesn't need this — bookmarks are deferrable —
so the shape can become `~/workspaces/<task-id>-<short-slug>/<repo>/`
with the slug cosmetic and the bookmark resolved later.

## Shape sketch

```
~/workspaces/proj-123-fix-auth/
  .rig.toml          # {id, tracker, title, primary_repo, repos: [...]}
  .envrc             # exports RIG_ID, RIG_TRACKER, RIG_BASEDIR
  api/               # jj workspace of phinze/api
    .envrc           # source_up; export GH_REPO=phinze/api
  web/               # jj workspace of phinze/web
    .envrc           # source_up; export GH_REPO=phinze/web
```

CLI shape — `rig up`/`rig down` is real industry idiom (oil rigs,
audio crews, sailing all rig up before a job and rig down after), and
a "rig" reads naturally as purpose-built apparatus assembled for one
job (climbing rig, fishing rig, sound rig):

- `rig up PROJ-123` — pitch a new rig: resolve issue, create basedir,
  add first repo workspace, spawn tmux session with claude.
- `rig up <gh-issue-url>` — same shape, GH dispatch.
- `rig add owner/repo` — add a repo to the rig you're in (cwd-derived).
- `rig down` — break the rig down; flush final metadata, archive or
  remove the basedir, kill the tmux session.
- `rig ls` — list rigs in flight (the call-sheet equivalent).
- `rig cd PROJ-123` — jump to a rig; fzf if no arg.

## Naming

A rig's identity comes in three levels, all derived once at `up`/`review`
time:

1. **Task id** — the compact handle: `mir-1221` (Linear mints it, globally
   unique via the team prefix) or `pr-845` (GitHub, unique per repo only).
2. **Task slug** — `<task-id>-<title-slug>`, capped at 60 chars with a hard
   cut, the same shape Linear mints for branch names. Linear hands it to us
   via `branchName` (minus the `user/` prefix); for GitHub PRs we derive it
   from the PR title. Names the basedir.
3. **Working-tree id** — `<task-id>-<repo>`, one per repo workspace. Already
   exists as the jj workspace name; also the right value to project into the
   environment for tools that need a per-tree key (iso's `ISO_SESSION`,
   compose's project name). Main checkouts get the parallel-but-different
   form `<owner>-<repo>`.

The principle underneath: **truncated paths are for display only, never for
identity**. The pre-rig layout happened to give every working tree a unique
basename (the leaf dir was a branch slug), and tools quietly grew the
assumption that `basename $(pwd)` identifies a project — iso's session
names, sophon's notification grouping, docker-compose's default project
name. Rig's layout (`<basedir>/<repo>`) broke that by reintroducing
repo-named leaf dirs. Rather than contorting the layout to keep the
heuristic accidentally true, identity is declared (manifest → direnv → env
vars) and tools should consume the env var first, falling back to
enough-path-to-be-unique rather than basename.

tmux sessions are named with the full basedir path in session-wizard's
full-path convention (`~/workspaces/...`, lowercased, `. :` → `-`), so a
`t` jump into a rig dir finds the existing session instead of spawning a
duplicate. Full paths are never ambiguous; only truncation is.

## Open questions

- **Language.** Fish is at its ceiling for this shape (TOML parsing,
  plugin dispatch, multi-command surface). Go fits the neighborhood
  (peer to `recto`, `jj`, `gh`, ships clean as a `pkgs/` derivation).
  Python stdlib is the cheap-iteration alternative — `tomllib`,
  `argparse`, `subprocess` all available globally without a venv dance.
  Lean: Python prototype to find the CLI shape, port to Go once stable.
- **Tracker shim shape.** Minimum interface: `resolve_issue(id) ->
  Task`, plus maybe `mark_in_progress(id)` and `mark_done(id)`. GH
  issues lack a canonical `branchName` field, so the shim has to
  synthesize one (or just defer entirely, since jj doesn't need it up
  front).
- **Sandbox primitive.** bwrap on Linux, claude code's own
  `--allowed-paths`, or something else? Decide before locking in the
  basedir-as-boundary assumption.
- **direnvrc stdlib migration.** Current path-parsing trick
  (`~/workspaces/github.com/...` → `GH_REPO`) stops being load-bearing.
  Layered `source_up` setup replaces it. Old single-repo sessions can
  age out as their tasks finish.
- **Interactive picker source mixing.** No-arg `rig up` should fzf
  across pickable issues. Merge Linear + GH into one list with a
  source column, or pick the tracker first? Merged is nicer but means
  two API calls per invocation.
- **`rig down` destructiveness.** Does it delete the basedir, archive
  it somewhere (`~/workspaces/.archive/`?), or just unregister the jj
  workspaces and leave the dir for manual cleanup? Probably "archive
  by default, `--purge` for delete," but worth a beat.

## Next actions

1. Sit with the design a few days; let the shape either stick or crack
   under typing.
2. Prototype in Python — single file, `tomllib`, `subprocess`. Goal:
   `rig up PROJ-123` end-to-end on one repo, then `rig add owner/repo`
   for the second.
3. If the CLI shape feels right after a real week of use, scaffold
   from launchpad, port to Go, package under `pkgs/`.

## Related

- `nix-config/home-manager/phinze/fish-functions/jpickup.fish` —
  current implementation.
- `nix-config/home-manager/phinze/fish-functions/jreview.fish` —
  sibling for PR review.
- `Projects/Ideas/review-first-diff-tool.md` — same family (control
  surface for agent-driven work), different facet.
- `Projects/Ideas/agentic-memex-experiment.md` — adjacent in the "what
  does my tooling want to be in an agent-heavy world" cluster.

## Naming history

Working name `stagehand` (theater metaphor: set/strike/rig/call) came
up first but landed too theme-heavy. `rig` keeps the strongest verb
from that set and ditches the cuteness — same metaphor's still there
if you squint (the rig is what the stagehand sets up), without
pinning the whole tool to it.
