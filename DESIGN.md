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
  terminal agents (bwrap / `--allowed-paths` / whatever), so containment is
  structural, not bolted on.
- **Metadata + .envrc at the basedir.** A `.rig.toml` is the source of
  truth; the basedir `.envrc` and `rig env` project `RIG_ID`,
  `RIG_BASEDIR`, `RIG_AGENT`, and per-workspace keys so downstream tools (agent
  context, jj templates, `rig down`) read from one place.

## Where it stands

Built, in Go, and in daily use as a single multi-command binary that
ships under `pkgs/`. It outgrew the "prototype in Python first" plan below:
the shape stuck fast enough that it went straight to Go. `rig up`, `new`,
`review`, `add`, `track`, `pr`, `ls`, `switch`, `park`, `wake`, `waiting`,
`down`, `reap`, and `env` are all real. `jpickup` and `jreview` are
subsumed; the single-repo Linear pickup and the PR-review sibling both
fall out of `rig up` and `rig review` now.

The pre-rig layout baked the branch name into the directory
(`~/workspaces/<host>/<owner>/<repo>/<branch>`), a git-worktree habit. jj
doesn't need it, since bookmarks are deferrable, so the shape became
`~/workspaces/<task-id>-<short-slug>/<repo>/` with the slug cosmetic and
the bookmark resolved later. `rig env` still reads the old layout for
sessions that predate the move, and those age out.

## Shape sketch

```
~/workspaces/proj-123-fix-auth/
  .rig.toml          # id, title, created, agent, [parked], [repos], [branches]
  .envrc             # exports RIG_BASEDIR, RIG_ID; rig env adds the rest
  api/               # jj workspace of phinze/api
  web/               # jj workspace of phinze/web
```

The manifest is flat TOML: scalar `id` / `title` / `created` (plus `agent` for
non-Claude rigs, and `parked` once dormant), a `[repos]` table mapping each subdir to its `owner/repo`,
and a `[branches]` table mapping each subdir to the branches its work rides
(primary first, `rig track` secondaries after). `GH_REPO`, `RIG_WORKSPACE`,
and a stable per-workspace `RIG_PORT` are projected by `rig env` rather than
written into per-repo .envrc files: direnv loads only the nearest .envrc, so
a repo shipping its own (nix devshells) would shadow them (see §Naming).

CLI shape — `rig up`/`rig down` is real industry idiom (oil rigs,
audio crews, sailing all rig up before a job and rig down after), and
a "rig" reads naturally as purpose-built apparatus assembled for one
job (climbing rig, fishing rig, sound rig):

- `rig up PROJ-123` — the one door to your own work. Resolve the issue (or
  a PR of yours), and either drop into the rig that already exists or build
  the one that doesn't. A gh-issue or your-own-PR URL dispatches the same
  shape.
- `rig new [kickoff]`: start your own work before it has a tracker identity.
  With no argument it asks for a one-line kickoff, then offers a textarea for
  whatever context the title only gestures at, then offers the primary repo
  picker. This is one Bubble Tea wizard shared with radar, where ctrl-n opens it
  in place without dropping the popup's alternate screen. The kickoff becomes
  the title and a readable local id; the workspace starts at `trunk()` with no
  branch recorded until the work grows one. Pasted context is written to
  `KICKOFF.md` at the rig root rather than
  inlined in the launch prompt, which is typed into the agent's shell and would
  make a poor courier for a Slack thread. Being a file is the better half of the
  bargain anyway: the brief outlives the context window, a resumed session, and
  the arrival of a second agent. Empty is the common answer — esc skips the step,
  and piped stdin (`pbpaste | rig new fix the flake`) is the shape for scripts
  and agent shells with no TTY to prompt at.
- `rig review <pr>` — the jreview sibling, pointed at *other people's* work:
  pitch a rig around a PR awaiting your review, checked out read-only at its
  head. Hand it a URL that turns out to be yours and it just routes you to
  `up`.
- `rig add owner/repo` — add a repo to the rig you're in (cwd-derived).
- `rig recto <repo> [args]` — pull that repo's persistent Recto beside the
  task-level agent; optional arguments drive the promoted viewer.
- `rig track <pr>` — record a second PR's branch on a repo already in the
  rig, so down/reap gate on it too.
- `rig pr` — open the rig's PR in the browser, resolving across added repos and
  tracked/current branches; when several match, pick one with fzf.
- `rig ls` — list rigs in flight (the call-sheet); `--full` adds PR/CI
  columns, `--format=json` is the stable API.
- `rig switch` (alias `cd`) — jump to a rig; fzf if no arg, most-recently-
  attached first. `rig radar` (below) is the richer live-TUI take.
- `rig park` / `rig wake` / `rig waiting` — the dormant-review lifecycle:
  park sends a finished rig quiet (kills its session, keeps the basedir),
  waiting reports which parked rig's review came back, wake stands it back
  up for the selected agent to resume.
- `rig down` — break the rig down; refuses to drop unmerged work.
- `rig reap` — bulk-collect merged, WIP-free, idle rigs through the same
  teardown path as down.
- `rig env` — print the identity exports for the direnv stdlib to eval.

## Agent choice

`rig up`, `rig new`, and `rig review` accept `--agent`, by long name
(`claude|codex|antigravity`) or by the three-letter one the picker uses
(`cld|cdx|agy`). `RIG_AGENT` supplies the default when the flag is absent, with
Claude retained as the compatibility default. Rig launches the selected terminal
agent in the left pane and saves the choice in the manifest.

The choice is also pickable, and deliberately not as a screen of its own. Every
prompt a creation command already puts up — the kickoff line, the context
textarea, the issue, repo, and PR pickers — carries an agent bar that ctrl-o
cycles, so an agent you don't care about costs nothing and one you do costs a
keystroke. The key fell out of elimination rather than mnemonics. ctrl-a is
beginning-of-line in all three surfaces the bar rides on (fzf, bubbles'
textinput, its textarea) and the tmux prefix besides; between them those three
also claim b, c, d, e, f, g, h, j, k, l, m, n, p, q, t, u, v, w, and y. Of
what's left, ctrl-n and ctrl-r already mean something in the radar, and ctrl-x
reads as an emacs prefix waiting for a second key. fzf can't
return the cycled value through its exit status, so it binds `transform-header`
to a hidden `rig __agent cycle` — the same shell-back-into-rig idiom the live
issue picker uses — which advances a temp file and reprints the header block,
the picker's own hint included.

An invocation that prompts for nothing (`rig review <url>`, or an `up` carrying
both an exact id and `--repo`) has nothing to ride along on, so there the bar
becomes its own one-line prompt rather than letting the default pass silently.
It runs past every resume check, so re-running `rig review <url>` on a rig you
already have still just attaches. Naming `--agent` skips it outright; inheriting
`RIG_AGENT` does not, because a rig exports it to everything running inside it
and "the rig I happen to be sitting in" isn't the same statement as "this one
should use Codex." It renders the same generated
context as `CLAUDE.md`, `AGENTS.md`, and `.agents/rules/rig.md`, leaving those
files at the basedir so they do not become jj changes inside a repo workspace.
Codex and Antigravity are explicitly pointed to `../AGENTS.md` in their opening
prompt in case their instruction discovery stops at the repo cwd.

Agent choice also reaches the lifecycle machinery. `rig ls`, radar, and reap
take the newest matching turn from Claude's project JSONL files, Codex's rollout
JSONL files, or Antigravity's timestamped prompt history. Radar recognizes all
three commands in tmux, including wrapped Codex command names.

The session itself is a Recto carousel. The stable `main/<repo>` window holds
the task-level agent and whichever repository is currently relevant. Every
other repository has one persistent, full-window Recto. `rig recto <repo>`
parks the outgoing viewer in its repo window and joins the requested one beside
the agent, preserving each TUI's scroll, base, and focus state. Recto-only
parking windows disappear while their pane is in main; a window with an ad hoc
shell survives, and the outgoing Recto parks beside that shell when it returns.
Shells are ordinary tmux splits from a Recto, so they inherit the right repo cwd
without becoming permanent layout furniture.

## Up my work, review other work

The two pickup verbs split on a single axis: whose work is it. `rig up` is
where your own work lives — a Linear issue you're starting, or a PR of yours
you're coming back to. `rig review` is where other people's does — a PR you've
been asked to look at. That split is what the manifest's `kind` already
encodes, and it's what sets the terminal condition: an authoring rig is done
when it merges, a review rig when you've posted your review (see §"rig down
destructiveness").

Picking up your own PR is authoring, not reviewing, and the plumbing has to
know the difference. A review fetches `pull/N/head` read-only, fork-safe,
detached from any pushable branch, which is exactly right for someone else's
code. Your own PR wants `branch@origin` so you can push more commits onto it,
and `kind = "up"` so teardown guards it as unmerged work. Same
clone-colocate-fetch-workspace path, one bit of divergence hanging off
authorship.

An authoring PR pickup also has to land at the *same path* its originating `rig
up <issue>` used, because that's where the claude sessions you built live —
claude keys its history by cwd, and a rig's cwd is `<basedir>/<repo>`. The
bridge is the branch: a PR born from a Linear issue rides
`phinze/mir-75-add-zig-stack`, the very slug `rig up MIR-75` turned into id
`mir-75` and basedir `mir-75-add-zig-stack`. So the pickup derives its identity
from the branch rather than the PR number, reconstructing that id and path
exactly. A live issue-rig is then found by id and switched to; a `down`'d one
rebuilds at the same basedir, so `claude --resume` still lists what you did
there. Only a PR with no issue id in its branch (not born from a tracker) falls
back to `pr-<n>`, since there was never a sibling rig to line up with.

Because `up` owns your work end to end, it's idempotent by design. `rig up X`
doesn't mean "create a rig," it means "put me in my rig for X, making it if it
isn't there." If the rig's in flight, switch to it. If it's parked, wake it.
If it got `down`'d entirely, rebuild it from `branch@origin` and your pushed
work comes back with the branch. The existence check comes before any network:
`up` matches `listRigs()` the way `switch` does (id/slug/title, no gh or
linearis round-trip), so re-upping into work you already have is as instant as
switching. The tracker call only happens on genuine creation.

The invariant that makes this safe rather than scary: **`down` can't destroy
anything `up` can't rebuild.** `down`'s safety gate is "no unmerged, no
unpushed work," which is precisely the precondition that makes `up`'s
reconstruction lossless. Branch still open? `branch@origin` restores it.
Already merged? The branch may be gone but the work is in `trunk()`, so
`resolveStartRev` starts you there, which is correct. The durable state was
always the PR plus origin, never the basedir. Unpushed local commits are the
only thing that wouldn't survive, and `down` refuses to drop those without
`--force`. The property holds by construction.

Since `up` now materializes a rig for a repo you may not be standing in, it
stops deriving the primary repo from cwd alone — and stops *assuming* cwd even
when you are in a checkout, since the task is often for a different repo and a
silent wrong guess rigs the wrong tree. The picker always opens (ranked by
zoxide frecency, cloning nothing that ghq already has), with the cwd repo pinned
to the top as the default row: you confirm it with one Enter rather than have it
chosen for you. `--repo owner/repo` is the way to skip the picker outright, and
it clones on demand the way `review` does. The one place cwd is still taken
automatically is a non-interactive caller with no tty to draw a picker, where
it's the only answer we can give without the flag. The picker only runs on the
create path, so it never slows a re-up.

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
heuristic accidentally true, identity is declared in the manifest and
projected into the environment by `rig env`, which the global direnv stdlib
evals before every .envrc (`has rig && eval "$(rig env)"`). It can't ride
in rig-written .envrc files: direnv loads only the nearest .envrc (no
cascade), so a repo shipping its own — nix devshells — would shadow the
basedir's exports. Tools should consume the env vars first
(`RIG_WORKSPACE` is the per-working-tree key, same shape as the jj
workspace name), falling back to enough-path-to-be-unique rather than
basename. Rig also prepends a rig-local shim directory to `PATH`. Its `gh`
shim resolves `GH_REPO` from the invocation cwd, rather than the agent's
startup environment, so one agent can safely ship several repos from the same
rig.

tmux sessions are named with the full basedir path in session-wizard's
full-path convention (`~/workspaces/...`, lowercased, `. :` → `-`), so a
`t` jump into a rig dir finds the existing session instead of spawning a
duplicate. Full paths are never ambiguous; only truncation is.

## Reaping (`rig reap`)

The nightly `dev-session-cleanup` in nix-config exists because old-style
workspaces had no owner: reaping one meant path archaeology (parse
`<host>/<owner>/<repo>/<branch>` back out of the filesystem), heuristic
merge detection against the main repo, and hand-rolled teardown that had
to mirror what jpickup set up. Rig inverts that. Every workspace has a
manifest, an id, and a single teardown code path (`rig down`), so
cleanup stops being archaeology and becomes enumeration plus policy.
The nightly's rig-shaped replacement is one line: `rig reap`.

Shape: walk `rig ls`, and for each rig decide reapability with the same
fail-closed posture the shell script earned the hard way:

- **Accounted for**: every non-empty off-trunk commit reachable from `@`
  is covered by a merged PR's immutable GitHub head OID. This survives
  squash merges (where the original commits never become ancestors of
  trunk) and GitHub deleting the head bookmark after merge. An evolving
  rig records each secondary branch with `rig track`; finding work on a
  current but unrecorded bookmark blocks with that command as the hint.
- **No extra WIP**: work beyond the pushed PR head blocks, whether it is
  still at `@` or parked under an empty `jj new` commit. A non-empty `@`
  that exactly matches a merged PR head is already accounted for and does
  not need a ceremonial `jj new` before teardown.
- **Idle**: no recent attention. Two signals, both persistent and
  neither resettable by accident: the newest claude session JSONL mtime
  under `~/.claude/projects` for cwds inside the basedir (a turn
  appends whether human-driven or autonomous; repaint doesn't), and the
  rig's own age (a rig younger than the idle window can't be idle).
  File changes are deliberately the VCS gates' job — jj already sees
  any non-ignored modification as WIP, and losing a gitignored scratch
  file is the accepted cost of not mtime-crawling every workspace
  nightly. Earlier designs died here. `window_activity` (the shell
  script's hard-won lesson) turned out to lie: claude's TUI repaints
  at rest in some states, pinning sessions to "active" forever — the
  same blind spot that quietly neutered the legacy script's claude
  phase, which walked processes to their pane's window_activity. And
  tmux's attach-based signals (`session_last_attached`,
  `client_activity`) reset on a mere peek, so checking whether a rig
  was dead would keep it alive another day.

Each source repo gets one best-effort `jj git fetch` per run so trunk()
reflects what actually merged; a failed fetch just means checking
against a stale trunk, which fails closed too.

Reapable rigs go through the same code path as `rig down`
(`teardownRig`). Teardown also grew the tool cleanup `down` previously
lacked: stop the rig's iso session *by exact name* (the same
`dev-<id>-<repo>` rig env emits). Never `iso stop --all-sessions` from
a workspace dir — iso's project scope is basename-derived, so that
would also stop the main checkout's container of a same-named repo.

Division of labor stays the same as `rig env`: rig owns layout,
manifest, and teardown knowledge; nix-config owns scheduling (the
systemd timer invokes `rig reap` and keeps the legacy phase only until
old-layout workspaces age out).

Teardown jobs are durable so a crash can't lose the inventory, but durability
turned out to have a sharp edge: a job that outlives its rig is aimed at a
*name*, not at the thing it was made for. Session name, iso session, jj
workspace names and agent scratch dirs are all `<id>`- or basedir-shaped, and
`rig up` on the same ticket rebuilds every one of them identically. So a job
stuck since 14:34 replayed its forget step at 18:07 and deregistered the jj
workspace of a rig created at 17:31 — which then sat orphaned for five days,
invisible to a gate that could only report jj's "doesn't have a working-copy
commit" and fail closed.

Two things keep that from recurring. The job records the manifest's `created`
stamp, and a retry that finds a *different* stamp at its basedir knows it's
been superseded: it abandons every rig-shaped step and clears only the trash
it made. And the forget step, the irreversible one, now records progress per
source repo as it goes, so it is never replayed at all.

What kept the job alive long enough to do the damage was the other half:
`RemoveAll` on the quarantined copy failed permanently, because a container
had written root-owned build output into a root-owned directory and unlinking
a file needs write access to its *parent*. Retaining the job there is correct
and stays — the bytes are real and somebody has to collect them — but the
error now names the cause, since retrying a permission failure achieves
nothing on its own and the operator has no way to guess that sudo is the
missing ingredient. The retention was never the bug; replaying the *other*
steps was.

Deferred to a future pass: being smart about the claude sessions
running inside a rig. Killing the rig's tmux session takes its pane
processes with it, but the legacy nightly's phase 3 (SIGTERM idle
`claude-unwrapped` processes wherever they live) has no rig-aware
replacement yet.

## The radar (`rig radar`)

`rig switch` and `rig waiting` answer two halves of the same question,
"which rig do I go to next?", and answer it in prose. Switch is the
instant, local half (jump to a live rig, most-recently-attached first, no
network). Waiting is the network half (which parked rig's review came
back). The radar folds both into one live view: a TUI popup, bound the way
ilmari was, that renders every rig and its real state and lets you act on
the one you pick.

The point is that it doesn't guess. ilmari (the popup this replaces) infers
agent state by scraping pane output for `running` / `waiting-input` /
`finished`, because from the outside that is all it can see. Rig is on the
inside. It already knows whether a session is live, whether a claude turn
landed in the last few minutes (`agentState`), whether the rig is parked,
and what its PRs' review decision is (`parkedDisposition`). The radar is
that truth drawn as a board instead of re-derived from terminal scrollback.

Layout is two sections. *In flight* is the switch view: unparked rigs,
most-recently-attached first, the session you're in dropped, each row
carrying its live agent state. *Parked / awaiting review* is the waiting
view: dormant rigs ranked by how much they want you (changes-requested →
approved → merged → still-waiting), the same ranking `rig waiting` prints.
One glance covers both "where was I" and "what came back."

Enter does the right thing per row so you never pick a verb: a live rig
switches, an in-flight rig whose session was killed gets a bare one stood
up first, a parked rig wakes (clears the park stamp, spawns a session for
`claude --resume`, attaches). The switch itself is the same popup trick
ilmari uses. The popup inherits `$TMUX`, so `tmux switch-client` from inside
it moves the underlying client, and the `-E` popup tears down as the program
exits, landing you in the target.

The review column is where the live TUI earns itself. Switch stays instant
by never touching the network; waiting pays the gh cost because it's an
explicit command. The radar gets both: it renders immediately from local
state, then fires the same concurrent `enrichWithPRs` fan-out in the
background and fills the PR/review cells in as they land. Local agent state
re-ticks every couple seconds on top, so a rig going hot updates while
you're looking at it, the thing ilmari's bell-on-transition gestures at,
keyed off real turn activity instead of a scrollback heuristic.

Built with Bubble Tea, the one place rig reaches past its
stdlib-and-shell-outs posture for a real dependency. The justification is
that the whole design is render-now, fill-in-later, tick-forever, and that
is exactly Bubble Tea's Msg/Cmd model; ilmari itself is a real ratatui TUI,
so a real TUI lib is matching it, not betraying the ethos. The framework
layer stays thin (one model file) and the guts stay in the helpers switch
and waiting already share.

For now the radar takes over ilmari's slot (the prefix-`i` popup) and
becomes the primary rig switcher; session-wizard keeps prefix-`t` for
everything that isn't a rig. The horizon is holistic: teach the radar about
plain tmux sessions too, so it's the one board for all of it and
session-wizard's job folds in as well. Not yet; first it has to earn the
switcher seat on rigs alone.

## Sweeping (`rig sweep`)

Monday morning, and Friday left behind a dozen rigs that are each one
command from their next step. Two of them merged over the weekend and want
tearing down. Four are approved and green and want merging. One came back
with change requests and wants you in the seat. The rest want nothing.
Finding that out costs a `rig waiting`, a squint, and then a dozen commands
typed by hand.

The odd thing is that rig already *computes* every bit of that judgment; it
just never acts on it. `waiting` folds a rig's PRs into a disposition and
prints a table with a next-step column you retype. `radar` renders the same
dispositions live but its only verb is Enter, meaning "go there" — and half
of Monday's pile doesn't want you to go anywhere, it wants one command run
against it. `reap` does act, but only at the fully-resolved-and-cold end
(merged, WIP-free, 24h idle) and only ever to tear down. A ready-to-merge
PR is precisely a rig reap must refuse.

So the gap isn't a view and it isn't a policy. It's the actionable middle:
a pass over the rig set that proposes each rig's next step and then carries
out the ones you agree with. That's a third interaction model, distinct from
a glanceable board and from an unattended cron.

The shape is plan-then-stream, the same trick `down` already plays when it
pops the radar to pick a landing spot: a Bubble Tea board of proposed
actions where you toggle what to apply, then the TUI releases the terminal
and the real `gh` and teardown output streams underneath it. The first cut
was a pure stream that asked y/n per rig, and smoke-testing killed it — with
six approved PRs and one teardown you scrolled past six things you couldn't
act on to reach the single prompt that mattered, and the hint explaining why
the six did nothing printed *after* it. Choosing across a dozen rigs at once
is a board problem. Watching the chosen work happen is a stream problem.
Doing both in one modality made each worse. So: board to decide, stream to
execute, and `-n` to plan without either.

The ladder is `parkedDisposition` generalized past parked rigs, which is
most of why this was cheap to build: same vocabulary, so sweep, waiting, and
the radar can't disagree about what a rig's state *is*, only about what to
do about it. Merged and clean tears down. Approved with CI clear offers the
merge. Changes requested is reported with its wake command rather than
executed, because it wants a human and a batch pass is the wrong shape for
that. A review rig ignores its disposition entirely and asks the teardown
gate, since "you posted a review" is its terminal condition and the author's
merge state says nothing about it.

Empty `Checks` means the repo has no CI configured, not CI that hasn't
reported, so it counts as clear. Red CI, though, is your move rather than a
reviewer's, so it outranks everything except a change request and leaves the
quiet section entirely. `parkedDisposition` can't see this — it folds review
state only — and burying "CI failing" under "awaiting review" was the single
most misleading thing the board did. Pending checks stay quiet: they resolve
themselves, and nagging about them would put half the board in the needs-you
pile every time anyone pushed.

"No PR" turned out to cover far more ground than it sounds like: work you
haven't pushed, a repo that lands straight on trunk and never has one, and —
most often — a rig whose PR merged and whose branch GitHub then deleted, so
there's nothing left to look up. Only the teardown gate can tell those apart,
so it always asks. An earlier cut short-circuited on a live tmux session
here, meaning to protect the rig you're sitting in. Nearly every rig has a
live session, so all that really did was hide finished work: three merged,
spotless rigs sat in the quiet list reading "in flight" and could never be
offered. Whether you're mid-thought is a checkbox default now, not a reason
to withhold the row, and the signal for it is a recent agent turn rather than
a session that exists.

Which leaves the question the "no PR" bucket kept dodging: a clean tree with
no PR means either "this shipped and the branch is gone" or "nothing came of
this yet", and those want opposite defaults. Every rig carries a live agent
conversation — the smallest on a real board was still 95 turns — so a rig with
no commits isn't empty, it's a thinking session that hasn't landed anything.
Tearing that down loses no data (the transcript lives under `~/.claude`, and
`rig up` rebuilds the basedir so `--resume` still finds it) but it loses the
thread: you have to remember the rig existed. That's a real cost, and it isn't
one a merged rig carries.

So the manifest records a PR number the first time one is seen. A branch is
perishable and a PR number isn't, so a rig that shipped stays recognisable
long after GitHub deletes the head. Absent means *unknown*, never "never had
one" — rigs predating the field read the same as rigs that genuinely produced
nothing, and the copy says "no PR on record" rather than pretending to know.

That feeds a checkbox default, not an eligibility rule: a rig that shipped is
pre-checked the moment it's clean, because tearing it down is bookkeeping. A
rig with nothing on record waits until it's been untouched for a day —
reap's `maxIdle` window, reused deliberately so the two don't hold different
opinions about when a rig has stopped mattering. An agent mid-turn is never
pre-checked whatever else is true. The row is always visible and always one
`a` away, so this only ever picks the default. Note that the "am I mid-turn"
window and the "has this gone stale" window are different questions and must
not share a constant: `agentActiveWindow` is three minutes and exists to drive
a working/idle dot, and an early cut wrongly borrowed it here, pre-checking
rigs you'd been talking to twenty minutes earlier.

The rows carry metadata beyond the verdict, because the disposition alone
kept flattening rigs that wanted different things. PR refs make a rig's scope
visible ("awaiting review" reads differently once you see it's two PRs across
two repos). A WIP marker names repos holding work that is neither on trunk
nor covered by a PR head — subtracting those heads is what makes it mean
something, since otherwise every open PR's own commits read as WIP and a
fully-pushed rig wore the same badge as one sitting on unpushed work. And an
idle age, because "awaiting review" since lunch and since Tuesday are not the
same situation.

None of which told you what a rig was *about*. The board's free-text column
started out as the verdict — "no PR on record", "merged and clean" — and the
MERGE group was the only one that read well, because it had already been
special-cased to show PR titles instead. That case generalizes: "no PR on
record" is true of every row carrying it, the same way "approved, CI clear"
is true of every merge row, so a board of ticket numbers left you deciding
whether to delete `mir-1477` on the strength of the number alone. The verdict
became its own narrow column (the *why*) and the wide one became the
*subject*, answering "what is this rig about" for every group.

The subject ladder is PR title, then task title, then agent session title. A
PR title is the most authoritative account of what a rig produced and the one
you most want in front of you before pressing enter on a merge. The task
title costs nothing — it's been on `rigStatus` all along, and the radar has
been rendering it since day one; sweep simply never looked. A merge row names
every PR it would land, since one checkbox lands all of them, but everywhere
else a lone PR speaks for its rig and several don't (two titles joined
overrun a column you're only reading for context), so a multi-repo rig falls
back to its own title outside the MERGE group.

The third rung reads the `ai-title` record out of the newest Claude
transcript, which is a bounded tail read: Claude Code re-emits the record
every time it refines the title, so the newest sits at the very end of the
file — on a 13MB transcript the last one landed in the final 0.1%. It almost
never fires, since a manifest always carries a title; it's there for one
written before the field existed. Worth knowing if it's ever promoted up the
ladder, because the tradeoff isn't one-sided: the agent's own title is
*sharper* than the task title on ticket rigs, where the manifest holds a
problem statement ("Cluster-local OCI registry still has no authentication…")
and the agent holds an intent ("Add authentication to cluster-local OCI
registry") — and *worse* on kickoff rigs, where rig's own launch prompt
bleeds in and "sweep polish" becomes "Sweep polish rig kickoff and analysis".
It's also Claude-only; Codex rollouts and Antigravity history record a cwd
and a timestamp but never a title.

Colour follows the split. Selection state rides the subject, the widest cell
and so the one that reads as "queued" from across the board. Red is reserved
for a why that names something broken, which today is exactly the wake rows'
failing CI — it used to cover the whole line, and now it sits on the four
words that earned it while the title beside them stays readable. All four
columns resolve against one grid shared by every group, so the board reads as
a single table; a subject column that shifted between MERGE and NEEDS YOU
would be worse than no subject column at all.

Merging is the one genuinely new capability here, and the only irreversible
step. Teardown is safe to batch precisely because the lifecycle invariant
holds: `down` can't destroy anything `up` can't rebuild, so a wrong check
costs a rebuild, not work. Merge has no such property, and the board encodes
that asymmetry in its defaults: teardown rows start checked, merge rows
start unchecked, and `a` (select all) is defined to touch only teardowns. A
flag would have been the obvious guard, but a checkbox you must deliberately
tick is a *better* confirmation than a flag plus a y/n, and it doesn't make
you remember an incantation every Monday. `gh pr merge` is called without
`--delete-branch` on purpose: teardown accounts for merged work by immutable
head OID and copes fine with a branch GitHub already removed, but deleting
the *local* branch under a live jj workspace is a different and much worse
problem.

The cascade is what keeps it one sitting instead of two. Merging is usually
the only thing standing between a rig and teardown, so a rig whose PRs just
landed drops the fetch cache (trunk moved under every workspace, and the gate
would otherwise ask its question against a trunk predating the very merge
it's reacting to), re-asks the gate, and tears down right there in the
stream. The first cut instead re-planned and put the board back up, which
made you approve the same rig twice — you already said "land this", and
teardown is the reversible half. The gate keeps the final say either way: the
first real run merged three PRs and `chase-a-flake` still stayed, because
`nix-config` had working-copy changes. Teardown itself goes through
`rigTeardownBlocker` and `teardownRig` unchanged, re-checked under the rig
lock the way reap does. The plan is built and checked off *outside* the lock,
deliberately, since holding it while you read the board would block every
other rig command.

The scan runs *inside* the TUI rather than before it. It costs a gh
round-trip per branch plus a jj fetch and teardown check per candidate, which
on a real Monday is several seconds; doing it first and opening the board
afterwards meant staring at a dead terminal wondering if it had hung. So the
board goes up immediately in a loading state and the scan reports its phases
into it — reading workspaces, asking GitHub about N rigs, then checking each
teardown candidate by name. Same render-now-fill-in-later posture the radar
takes, for the same reason. Keys other than the exits are inert while it
loads, since a keystroke that half-acts on an empty board is worse than one
that does nothing.

Every action needs a deliberate check, which means a sweep with nobody to
answer can only ever be a plan. A non-tty invocation prints the same report
and stops rather than refusing outright, so the command stays safe to pipe or
drop in a cron without a surprise teardown.

Execution fails fast, which the first real run taught us. The merge method
was guessed as squash; `mirendev/runtime` forbids squash merges outright, so
all six queued merges failed identically and the one line that mattered was
buried under five repetitions of itself. An error during the stream is nearly
always a fact about the setup rather than about one PR, so the first one stops
the pass with everything after it untouched. The default is now a merge
commit, which is what we actually use. The single exception to fail-fast is a
rig whose lock is held: that's another rig command mid-flight, not a broken
sweep, so it skips and the pass carries on.

A rig spanning repos carries one PR per repo (plus anything `rig track`
recorded), and they land together or not at all — one checkbox, every PR. The
corollary is that a rig whose PRs *disagree* about review, one approved and
one still out, never reaches the "approved" disposition and is deliberately
left alone. Landing half a cross-repo change is worse than landing none of it.

Building the board also turned up a bug in the branch heuristic underneath
all of this. `jjPRBranch` looked for the closest bookmark in `::@` that
wasn't `trunk()` — which excludes the trunk commit but not bookmarks already
merged *into* trunk. A merged branch stays a local bookmark, so once a rig's
own branch was deleted post-merge, the search happily returned some *other*
rig's merged branch instead. Three rigs were simultaneously resolving to one
unrelated bookmark, pointing their PR lookups (and `rig pr`) at a PR none of
them owned. Excluding all of `::trunk()` fixes it, and leaves every live
branch untouched.

The horizon is folding this back into the radar: once the ladder exists as a
function, a radar row already knows its own next step, and the board can
grow per-row verbs that call into the same policy instead of duplicating it.
That's the right order — the pass earns the policy, the board borrows it.

## Open questions

- ~~**Language.**~~ Answered: Go. Fish was at its ceiling for this shape
  (TOML parsing, plugin dispatch, multi-command surface); Go fits the
  neighborhood (peer to `recto`, `jj`, `gh`, ships clean as a `pkgs/`
  derivation). The Python-prototype-first hedge proved unnecessary, since
  the shape held on the first pass.
- **Tracker shim shape.** Minimum interface: `resolve_issue(id) ->
  Task`, plus maybe `mark_in_progress(id)` and `mark_done(id)`. GH
  issues lack a canonical `branchName` field, so the shim has to
  synthesize one (or just defer entirely, since jj doesn't need it up
  front).
- **Sandbox primitive.** bwrap on Linux, claude code's own
  `--allowed-paths`, or something else? Decide before locking in the
  basedir-as-boundary assumption.
- ~~**direnvrc stdlib migration.**~~ Answered: `rig env` owns all layout
  and manifest knowledge (including the legacy
  `~/workspaces/github.com/...` → `GH_REPO` path-parse, which ages out
  with those sessions); the host stdlib is a one-line eval. Repo-owned
  `.envrc` files and the parent basedir `.envrc` provide the direnv
  entrypoints; the stdlib projects the right environment from either one.
- **Interactive picker source mixing.** No-arg `rig up` should fzf
  across pickable issues. Merge Linear + GH into one list with a
  source column, or pick the tracker first? Merged is nicer but means
  two API calls per invocation.
- ~~**`rig down` destructiveness.**~~ Answered by the park/reap lifecycle
  rather than an archive dir: `down` tears the basedir down but refuses to
  drop unmerged work, `park` keeps a finished rig on disk while its review
  is out, and `reap` bulk-collects the merged-and-idle. The safety gate
  turned out to matter more than an archive would have. One wrinkle the gate
  had to grow: "done" means different things for authoring versus reviewing.
  An authoring rig is done when its work merges, so teardown guards its local
  commits. A `rig review` pickup holds the *author's* commits, fetched
  read-only — nothing of yours to merge, and the PR staying open is the whole
  point — so its terminal condition is "you've posted a review", not "the PR
  merged". The manifest records the rig's `kind` so the shared teardown
  judgment can tell them apart; a review rig with no review from you yet stays
  put (and `--force` still overrides).

## Next actions

The prototype-and-port plan is spent; rig is the Go tool now, so what's
left is growth on the built base:

1. `rig radar` (above) — the live-TUI switcher, first replacing ilmari on
   rigs, later absorbing plain tmux sessions so it becomes the one board.
2. Fold `rig sweep`'s ladder back into the radar as per-row verbs, so the
   board can advance the row you're looking at instead of only switching
   to it.
3. Sand off the open questions still open (sandbox primitive, multi-tracker
   shim, picker source mixing) as real use pushes on them.

## Related

- `nix-config/home-manager/phinze/fish-functions/jpickup.fish` — the
  single-repo Linear pickup `rig up` subsumed.
- `nix-config/home-manager/phinze/fish-functions/jreview.fish` — the
  PR-review sibling `rig review` subsumed.
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
