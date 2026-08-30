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
a `.rig/manifest.toml` and `.envrc`, and spawns a tmux session ready for an agent to
work in. `rig new` starts the same shape from a free-form kickoff when there is
no ticket yet, followed by an optional textarea for pasted context that lands in
`KICKOFF.md` at the rig root (esc skips it; piped stdin fills it non-
interactively). The agent is pointed at that file rather than handed its
contents, because the launch prompt travels by `tmux send-keys` into a shell.
The kickoff, context, repo, and agent controls are one Bubble Tea wizard shared
with the radar; ctrl-n opens it there without leaving the radar's alternate
screen or tmux popup.
Radar shows its hosting session first as non-selectable `CURRENT` context, then
keeps the other live rigs and plain sessions in one section, with parked rigs in
a separate section. The cursor still starts on the most recent other session.
Both action sections are ordered by the same durable last-touched timestamp,
which is also the age shown in the first column; background agent and PR updates
never move rows. ctrl-p toggles the selected rig between sections without
leaving the board; Enter on a parked rig still wakes and enters it.
Parking captures the carousel's hot repo and the selected agent's conversation;
when invoked inside that session, it opens the radar to pick the next hop before
killing it. Wake reconstructs the full repo-rooted agent + Recto layout and
resumes it.
`rig resume [query]` performs the same runtime repair without changing parked
state, defaulting to the rig containing cwd when no query is given.
`rig add owner/repo` brings additional repos under the same rig. `rig down`
breaks it back down.

`rig sweep` is the pass over every rig that proposes each one's next step. It's
plan-then-stream: a Bubble Tea board of checkable actions, then the TUI exits and
the real gh and teardown output streams. It reuses `parkedDisposition` for state
and `rigTeardownBlocker`/`teardownRig` for teardown, so it can't disagree with
`waiting` or `radar` about what a rig's state is — only about what to do
about it. See DESIGN.md §"Sweeping".

Sweep is also the *only* thing that decides a rig's fate. `rig reap` used to
collect merged, WIP-free, idle rigs unattended from a nightly timer, and that
authority has been removed: `rigTeardownBlocker` reasons in commits, branches,
and PR states, so a rig whose whole value is its agent conversation reads
exactly like one whose work already shipped, and the nightly pass could not tell
them apart. It ate a real one on 2026-08-03. Reap is now a janitor that retries
stranded teardown jobs and stops orphaned tmux/iso scopes, nothing more, and
`--runtime-only` survives only as a no-op so deployed units don't break. If you
find yourself adding a policy gate to reap, that's the signal the decision
belongs in sweep instead. See DESIGN.md §"Reaping".

Teardown is reversible for a week. `prepareTeardownJob` writes a tombstone
before it destroys anything, so `down` and `sweep` both get it without knowing
it exists and nothing can kill a rig by a path that skips it. The field that
matters is the agent session id: every agent store keys on cwd or workspace,
never on rig, so it's resolvable exactly once — while the basedir still exists.
That asymmetry is the whole design, and it's why the write is eager on a path
that usually won't need it. `rig history` lists the window, `rig resurrect <id>`
rebuilds the rig and resumes the conversation, and the radar shows the same
thing as a `RECENTLY TORN DOWN` section whose rows carry a `stone` and
resurrect on Enter.

That section is hidden at rest and revealed by a filter, which is the load-
bearing half: you search for a rig not knowing it's gone, and an empty result
would teach you it never existed. ctrl+t forces it open (not ctrl+h — that's
0x08, which some terminals send for backspace, and backspace edits the filter).
Those rows live outside `rigRows`, which is what already scopes the PR fan-out
and the parked toggle to live rigs, so don't reach for them there — and note
that exclusion is *why* `radarGlyph` and `radarTailSegs` need their own `stone`
branches, since the tail's unfetched "…" would otherwise never resolve. ctrl+p
refuses them explicitly, because a history row keeps its old basedir in `Path`
and would slip past the check that catches bare sessions. Recording is best-effort on
purpose: a rig you asked to tear down must go away even if we couldn't write
its tombstone. See DESIGN.md §"Tombstones".

Two things there are easy to get wrong. Merges never arrive pre-checked and `a`
skips them, because merging is the only irreversible act in the pass. And
"pre-checked" is a separate judgment from "safe": `sweepCollectable` asks whether
losing the rig would annoy you, gating on `sweepStaleAfter` (24h, formerly
reap's idle window and now its only home) for rigs with no PR on record.
Don't reach for `agentActiveWindow` there — it's
three minutes and exists for the working/idle dot.

Each row is subject + why, not one blurred column: `sweepSubject` says what the
rig is about (PR title, then `status.Title`, then `claudeSessionTitle`) and
`p.detail` says why it's in this group. Column widths come from `m.columns()`
once per frame across every group, so all four share one grid — size a column
inside a group and the board stops reading as a table.

`rig notify` is the ambient inbox, and it's the one part of rig that isn't
rig-scoped. Tools that aren't rigs (crons, watchers, `nix-config-sync`) post to
it and the entries surface in `ls`, `sweep`, and `radar`. Identity is
`(source, key)` rather than a generated id, because the archetypal poster is an
hourly job saying the same thing every hour: re-posting a key updates one entry
and bumps `Count`, so you read "stalled, 37 runs" instead of scrolling 37 rows.
An entry may name a rig, which pins it to that rig's row; most won't, and those
banner above the table. Each board picks its own density — `ls` and `sweep`
print every loose entry, the radar prints one summary line because it lives in a
popup where rows are expensive.

The tmux layout is a Recto carousel: `main/<repo>` holds the task-level agent
and the active repo's persistent Recto, while the other repos wait as
full-screen Recto windows. `rig recto <repo> [recto args...]` promotes one and
optionally drives it. Ad hoc shells are ordinary splits from a repo's Recto,
not permanent empty panes.

Every Recto uses Recto's default branch-point base, review rig or authoring rig,
because a rig is one task and the task's diff is the whole stack rather than
whatever commit is on top. `rectoCommand` is the single launch contract, and
every launch site goes through it so creation, add, resume, and resurrection
cannot disagree.

External tools learn the current rig through `rig info --format=json`, never by
parsing `.rig/manifest.toml`. The manifest is Rig's private persistence format;
the JSON shape is the compatibility boundary. The API exposes the absolute rig
root as lifecycle identity. Review rigs also expose the current repository's
durable PR locator, so Recto can restore PR context without learning Rig's
private layout. Recto owns its authored state beneath XDG for standalone and
Rig launches alike. Teardown asks `recto state forget` about each workspace on
a best-effort basis; Rig never parses Recto's files or computes its state key.

A review rig also pins each fetched `pull/N/head` with a reserved, untracked
`rig-review/…` bookmark in the shared jj repository. The bookmark is the
reachability root a workspace registration is not: force-pushing and fetching a
tracked PR branch cannot abandon the reviewed commits or rebase the empty
review working copy out from under Recto. `rig review --refresh <url>` is the
only path that advances the pin and working copy to a new head. Teardown deletes
the pin after forgetting the workspace.

The agent is selectable with `--agent`, which takes either the long name or the
short one it goes by in the picker: `cld`, `cdx`, `agy`. `RIG_AGENT` moves the
starting position, with Claude as the fallback beneath it. Rig writes equivalent
generated instructions and tracks session activity for all three.

`rig env` deliberately does not export `RIG_AGENT`, though it once did. Every
other key it projects has a downstream consumer (`RIG_PORT` a dev server,
`GH_REPO` gh, `ISO_SESSION` iso); that one had none, and because `parseAgent`
reads the same name, exporting it meant a rig silently seeded the picker for the
next rig you made from inside it. The name now belongs to you, not to whichever
rig you're standing in — which is what makes setting it in home-manager a real
global default. If you ever want a rig's agent readable from inside it, read
`.rig/manifest.toml`, or pick a name that doesn't feed back into the picker.

Codex additionally gates every unseen directory behind a trust prompt, which a
rig trips by construction: a fresh basedir and a fresh workspace under it, every
time. So rig seeds `[projects."…"] trust_level = "trusted"` into
`~/.codex/config.toml` as it makes each directory, and takes the entries back at
teardown. The other three exits were checked and are closed:
`--dangerously-bypass-approvals-and-sandbox` doesn't cover the prompt, trusting
an ancestor doesn't cover its children, and a `-c projects."…"` override is
ignored because the gate reads the file rather than the merged config. Seeding
is unconditional rather than codex-only, because trust belongs to the directory
and not to whatever process opens it. See `codextrust.go` for why the append
must check first (a duplicate TOML table breaks codex's config load outright)
and why a missing `~/.codex` means skip rather than create. Claude and
antigravity need none of this; both were verified to start clean in a directory
they've never seen.

Codex's *hook* trust is a separate, global thing keyed on the hooks file's path
and content hash, so it isn't rig's to fix. If it starts asking on every launch,
the cause is upstream: nix-managed `~/.codex/hooks.json` whose commands are
absolute store paths, which change on every home-manager rebuild and invalidate
every hash with them.

Every prompt `up`, `new`, and `review` already show carries an agent bar that
ctrl-o cycles, so the choice never costs a screen of its own. Two things there
are load-bearing. The key is ctrl-o by elimination, not preference: fzf,
textinput, and textarea between them claim most of the ctrl row (including
ctrl-a for beginning-of-line), the radar already means new-rig and refresh by
ctrl-n and ctrl-r, and ctrl-a is the tmux prefix besides. The fzf half works
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

### Talking to jj

rig shells out to jj. Always ask for data with an explicit `-T` template,
never by reading jj's human output. A template is a contract rig writes, so
jj is free to change how it renders and rig doesn't care; scanning the
default rendering makes rig's correctness depend on jj's formatting choices.
`workspaceRegistered` learned this the hard way, matching a `"<name>:"`
prefix out of `jj workspace list` until 0.44 added workspace roots to that
line.

Templated `jj workspace list` requires **jj 0.44 or newer**, which is rig's
floor. Worth knowing that nix-config now bumps jj automatically within days
of a release, so jj moves under rig without anyone deciding to upgrade; the
e2e suite runs real jj commands, which is what catches it when that goes
wrong.

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
check`), write and apply the commit message, then show the final change and
message once before advancing the `main` bookmark.

## Conventions

- stdlib net/http preferred over frameworks
- Shell out to `jj`, `gh`, and `tmux` via `os/exec` rather than pulling in
  heavy SDKs. Linear is the exception: its small read-only adapter talks to
  GraphQL directly so PR attachment lookup, issue resolution, and picker search
  share one auth and error boundary.
- TODO: Document project-specific conventions as they emerge.

## Related

- `nix-config/home-manager/phinze/fish-functions/jpickup.fish`: the
  single-repo Linear pickup this tool subsumes.
- `nix-config/home-manager/phinze/fish-functions/jreview.fish`: the PR
  review sibling, same shape.
- `~/src/github.com/phinze/memex/Projects/Ideas/rig.md`: original idea
  doc. DESIGN.md is a snapshot of it.
