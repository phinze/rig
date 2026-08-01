# rig

Workspace tool for task-shaped, multi-repo work. The unit of work is the *task*
(a Linear issue, a GitHub PR to review, whatever's next) rather than the branch,
and the data model is happy with "this task touches two repos."

`rig up PROJ-123` resolves a task, builds a basedir under `~/workspaces/`, drops
a [jj](https://github.com/jj-vcs/jj) workspace for the repo inside it, writes a
`.rig.toml` and `.envrc`, and spawns a tmux session ready for an agent to work
in. `rig new` starts that same shape from a free-form kickoff when there is no
ticket yet, with a second step for pasting in whatever context the one-liner
leaves out. `rig review <pr-url>` does it for reviewing a pull request.
`rig add owner/repo` brings more repos under the same rig, `rig ls` / `rig cd`
move between rigs in flight, and `rig down` breaks it all back down.

`rig sweep` is the Monday-morning pass. It works out every rig's next step and
draws them as a board you check off: merged work pre-checked to tear down,
approved-and-green PRs offered to merge but left unchecked, and the rigs that
came back wanting a human listed with the command to wake them. Enter drops out
of the TUI and streams the actual work underneath it. `-n` plans without
touching anything.

The tmux session is a Recto carousel. Its main window holds the task-level
agent beside the currently relevant repo's diff; every other repo waits as a
full-screen Recto tab with its viewer state intact. `rig recto cloud` pulls a
repo into the main hot seat, and `rig recto cloud focus path:42` promotes and
drives it in one step. Repo tabs do not carry permanent shells, but splitting
from a Recto inherits that repo's working directory when one is useful.

Claude Code is the default agent, but it is not baked into the rig. `rig up`,
`rig new`, and `rig review` all let you pick: whatever prompt they're already
showing carries an agent bar that ctrl-o cycles through `cld`, `cdx`, and `agy`,
and an invocation that prompts for nothing gets the bar on its own. `--agent`
names one without being asked and `RIG_AGENT` moves the starting position. The
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
