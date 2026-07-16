# rig

Workspace tool for task-shaped, multi-repo work. The unit of work is the *task*
(a Linear issue, a GitHub PR to review, whatever's next) rather than the branch,
and the data model is happy with "this task touches two repos."

`rig up PROJ-123` resolves a task, builds a basedir under `~/workspaces/`, drops
a [jj](https://github.com/jj-vcs/jj) workspace for the repo inside it, writes a
`.rig.toml` and `.envrc`, and spawns a tmux session ready for an agent to work
in. `rig new` starts that same shape from a free-form kickoff when there is no
ticket yet. `rig review <pr-url>` does it for reviewing a pull request.
`rig add owner/repo` brings more repos under the same rig, `rig ls` / `rig cd`
move between rigs in flight, and `rig down` breaks it all back down.

Claude Code is the default agent, but it is not baked into the rig. Pass
`--agent codex` or `--agent antigravity` to `rig up`, `rig new`, or
`rig review`; set `RIG_AGENT` to launch that agent in the left pane instead. The
choice is saved in `.rig.toml`; generated `CLAUDE.md`, `AGENTS.md`, and
Antigravity workspace rules carry the same live task context, and `ls`, `radar`,
and `reap` read all three agents' session activity.

It exists to fold a pair of fish functions (`jpickup` / `jreview`) into one tool
that composes things I already lean on (jj, gh, tmux, recto, linearis) instead
of reinventing them. See [DESIGN.md](./DESIGN.md) for the full shape and the
reasoning behind it.

## Heads up

This is a personal tool, built around my own workflow and machine setup, so it's
opinionated and not packaged for general use. You're very welcome to read it,
borrow from it, or open an issue to chat about the ideas. I just wouldn't count
on it being stable or supported.
