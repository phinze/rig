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
no ticket yet. `rig add owner/repo` brings additional repos under the same rig.
`rig down` breaks it back down.

`rig sweep` is the pass over every rig that proposes each one's next step. It's
plan-then-stream: a Bubble Tea board of checkable actions (teardowns pre-checked,
merges deliberately not), then the TUI exits and the real gh and teardown output
streams. It reuses `parkedDisposition` for state and
`rigTeardownBlocker`/`teardownRig` for teardown, so it can't disagree with
`waiting`, `radar`, or `reap` about what a rig's state is — only about what to do
about it. See DESIGN.md §"Sweeping".

The tmux layout is a Recto carousel: `main/<repo>` holds the task-level agent
and the active repo's persistent Recto, while the other repos wait as
full-screen Recto windows. `rig recto <repo> [recto args...]` promotes one and
optionally drives it. Ad hoc shells are ordinary splits from a repo's Recto,
not permanent empty panes.

The agent is selectable with `--agent claude|codex|antigravity` (or
`RIG_AGENT`) and defaults to Claude for compatibility. Rig writes equivalent
generated instructions and tracks session activity for all three.

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
