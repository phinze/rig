# Rig

Workspace tool for task-shaped, multi-repo work. Replaces the `jpickup` and
`jreview` fish functions with a single tool whose unit is the *task* (Linear
issue, GH issue, whatever's next) rather than the branch, and whose data model
accepts "this task touches two repos" without contortion.

See [DESIGN.md](./DESIGN.md) for the full shape, motivations, open questions,
and naming history. That doc is the source of truth for *why*; this file
captures *how it currently works* as the code grows.

## What This Project Does

`rig up PROJ-123` resolves a task from its tracker, creates a basedir under
`~/workspaces/`, drops a jj workspace for the primary repo inside it, writes
a `.rig.toml` and `.envrc`, and spawns a tmux session ready for an agent to
work in. `rig new` starts the same shape from a free-form kickoff when there is
no ticket yet, followed by an optional textarea for pasted context that lands in
`KICKOFF.md` at the rig root (esc skips it; piped stdin fills it non-
interactively). The agent is pointed at that file rather than handed its
contents, because the launch prompt travels by `tmux send-keys` into a shell.
`rig add owner/repo` brings additional repos under the same rig. `rig down`
breaks it back down.

`rig sweep` is the pass over every rig that proposes each one's next step. It's
plan-then-stream: a Bubble Tea board of checkable actions, then the TUI exits and
the real gh and teardown output streams. It reuses `parkedDisposition` for state
and `rigTeardownBlocker`/`teardownRig` for teardown, so it can't disagree with
`waiting`, `radar`, or `reap` about what a rig's state is — only about what to do
about it. See DESIGN.md §"Sweeping".

Two things there are easy to get wrong. Merges never arrive pre-checked and `a`
skips them, because merging is the only irreversible act in the pass. And
"pre-checked" is a separate judgment from "safe": `sweepCollectable` asks whether
losing the rig would annoy you, gating on `sweepStaleAfter` (reap's 24h window)
for rigs with no PR on record. Don't reach for `agentActiveWindow` there — it's
three minutes and exists for the working/idle dot.

Each row is subject + why, not one blurred column: `sweepSubject` says what the
rig is about (PR title, then `status.Title`, then `claudeSessionTitle`) and
`p.detail` says why it's in this group. Column widths come from `m.columns()`
once per frame across every group, so all four share one grid — size a column
inside a group and the board stops reading as a table.

The tmux layout is a Recto carousel: `main/<repo>` holds the task-level agent
and the active repo's persistent Recto, while the other repos wait as
full-screen Recto windows. `rig recto <repo> [recto args...]` promotes one and
optionally drives it. Ad hoc shells are ordinary splits from a repo's Recto,
not permanent empty panes.

Every Recto starts with `--pr`, review rig or authoring rig, because a rig is
one task and the task's diff is the whole stack rather than whatever commit is
on top. `rectoCommand` is the single place that decides, and every launch site
goes through it — see its comment for why the default is nearly free (`b`
cycles back to `@-` in one keystroke).

The agent is selectable with `--agent`, which takes either the long name or the
short one it goes by in the picker: `cld`, `cdx`, `agy`. It defaults to Claude
for compatibility, and `RIG_AGENT` (which every rig exports) moves the starting
position. Rig writes equivalent generated instructions and tracks session
activity for all three.

Every prompt `up`, `new`, and `review` already show carries an agent bar that
ctrl-o cycles, so the choice never costs a screen of its own. Two things there
are load-bearing. The key is ctrl-o by elimination, not preference: fzf,
textinput, and textarea between them claim most of the ctrl row (including
ctrl-a for beginning-of-line), the radar already means new-session and refresh
by ctrl-t and ctrl-r, and ctrl-a is the tmux prefix besides. The fzf half works
by binding `transform-header` to a hidden `rig __agent cycle` — fzf can only
hand state back through a file, so the choice round-trips through a temp file
that `pick.sync()` reads after the picker exits. And `ensurePicked` is not the
general path: it's the fallback for an invocation that prompted for *nothing*
(`rig review <url>`, or an `up` with both an exact id and `--repo`), which is
the only way those get to pick at all. An explicit `--agent` skips it, because
you don't get asked what you just said.

## Architecture

TODO: fill in as the shape solidifies. For now, see DESIGN.md §"Shape
sketch" and §"CLI shape".

## Development

```sh
# direnv handles environment setup automatically on cd
go build ./...
go test ./...
nix flake check
```

This is a personal, single-author repo. Land work by committing straight to
`main` — no PR ceremony, no review gate, `pr-time` is overkill here. The whole
flow is: keep the tree green (`go build ./... && go test ./... && nix flake
check`), draft the commit message and get a nod, `jj describe` the change, then
advance the `main` bookmark to it.

## Conventions

- stdlib net/http preferred over frameworks
- Shell-out to `jj`, `gh`, `tmux`, `linearis` via `os/exec` rather than
  pulling in heavy SDKs. The whole point of rig is to compose tools that
  already exist.
- TODO: Document project-specific conventions as they emerge.

## Related

- `nix-config/home-manager/phinze/fish-functions/jpickup.fish`: the
  single-repo Linear pickup this tool subsumes.
- `nix-config/home-manager/phinze/fish-functions/jreview.fish`: the PR
  review sibling, same shape.
- `~/src/github.com/phinze/memex/Projects/Ideas/rig.md`: original idea
  doc. DESIGN.md is a snapshot of it.
