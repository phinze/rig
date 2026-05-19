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
work in. `rig add owner/repo` brings additional repos under the same rig.
`rig down` breaks it back down.

## Architecture

TODO: fill in as the shape solidifies. For now, see DESIGN.md §"Shape
sketch" and §"CLI shape".

## Development

```sh
# direnv handles environment setup automatically on cd
go build ./...
go test ./...
```

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
