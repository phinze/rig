# rig

Workspace tool for task-shaped, multi-repo work. The unit of work is the *task*
(a Linear issue, a GitHub PR to review, whatever's next) rather than the branch,
and the data model is happy with "this task touches two repos."

`rig up PROJ-123` resolves a task, builds a basedir under `~/workspaces/`, drops
a [jj](https://github.com/jj-vcs/jj) workspace for the repo inside it, writes a
`.rig.toml` and `.envrc`, and spawns a tmux session ready for an agent to work
in. `rig review <pr-url>` does the same shape for reviewing a pull request.
`rig add owner/repo` brings more repos under the same rig, `rig ls` / `rig cd`
move between rigs in flight, and `rig down` breaks it all back down.

It exists to fold a pair of fish functions (`jpickup` / `jreview`) into one tool
that composes things I already lean on (jj, gh, tmux, recto, linearis) instead
of reinventing them. See [DESIGN.md](./DESIGN.md) for the full shape and the
reasoning behind it.

## Heads up

This is a personal tool, built around my own workflow and machine setup, so it's
opinionated and not packaged for general use. You're very welcome to read it,
borrow from it, or open an issue to chat about the ideas. I just wouldn't count
on it being stable or supported.
