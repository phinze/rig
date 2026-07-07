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
  truth; the basedir `.envrc` and `rig env` project `RIG_ID`,
  `RIG_BASEDIR`, and per-workspace keys so downstream tools (claude
  context, jj templates, `rig down`) read from one place.

## Where it stands

Built, in Go, and in daily use as a single multi-command binary that
ships under `pkgs/`. It outgrew the "prototype in Python first" plan below:
the shape stuck fast enough that it went straight to Go. `rig up`,
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
  .rig.toml          # id, title, created, [parked], [repos], [branches]
  .envrc             # exports RIG_BASEDIR, RIG_ID; rig env adds the rest
  api/               # jj workspace of phinze/api
  web/               # jj workspace of phinze/web
```

The manifest is flat TOML: scalar `id` / `title` / `created` (and `parked`
once dormant), a `[repos]` table mapping each subdir to its `owner/repo`,
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
- `rig review <pr>` — the jreview sibling, pointed at *other people's* work:
  pitch a rig around a PR awaiting your review, checked out read-only at its
  head. Hand it a URL that turns out to be yours and it just routes you to
  `up`.
- `rig add owner/repo` — add a repo to the rig you're in (cwd-derived).
- `rig track <pr>` — record a second PR's branch on a repo already in the
  rig, so down/reap gate on it too.
- `rig pr` — open the rig's PR in the browser.
- `rig ls` — list rigs in flight (the call-sheet); `--full` adds PR/CI
  columns, `--format=json` is the stable API.
- `rig switch` (alias `cd`) — jump to a rig; fzf if no arg, most-recently-
  attached first. `rig radar` (below) is the richer live-TUI take.
- `rig park` / `rig wake` / `rig waiting` — the dormant-review lifecycle:
  park sends a finished rig quiet (kills its session, keeps the basedir),
  waiting reports which parked rig's review came back, wake stands it back
  up for `claude --resume`.
- `rig down` — break the rig down; refuses to drop unmerged work.
- `rig reap` — bulk-collect merged, WIP-free, idle rigs through the same
  teardown path as down.
- `rig env` — print the identity exports for the direnv stdlib to eval.

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
stops deriving the primary repo from cwd alone. `detectPrimaryRepo` becomes the
fast path when you happen to be inside a checkout; otherwise `up` resolves the
repo the way `review` already does, via `ghq` (cloning on demand), with a
picker ranked by zoxide frecency. The repo picker only runs on the create path,
so it never slows a re-up.

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
basename.

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

- **Merged**: every repo workspace's work is an ancestor of `trunk()`
  in its source repo. A missing bookmark is not a green light (possible
  unpushed WIP); jj errors mean skip, never guess.
- **No WIP**: no non-empty commits reachable from `@` that aren't on
  trunk (catches both dirty `@` and the jj-new-on-top-of-WIP shape).
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

Implementation surfaced one wrinkle: the direnv anchor rig writes into
workspaces whose repo ships no .envrc gets auto-tracked by jj, leaving
`@` permanently non-empty — no such rig would ever reap. So `@` gets
exactly one allowance: a diff of precisely `.envrc` whose content is
the bare anchor. Anything else dirty at `@` blocks.

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
  with those sessions); the host stdlib is a one-line eval. The layered
  `source_up` idea didn't survive contact with repos that ship their own
  .envrc — see §Naming.
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
2. Sand off the open questions still open (sandbox primitive, multi-tracker
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
