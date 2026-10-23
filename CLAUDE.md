# Rig

Rig is a workspace tool for task-shaped, multi-repo work. `rig up MIR-123`
resolves a task, makes a basedir under `~/workspaces/` with a jj workspace per
repo, and starts a multiplexer session with an agent in it. Everything else
(radar, sweep, park, wake, messaging, remote hosts) manages the rigs that
produces.

`rig help [cmd]` is the reference for what each command does. DESIGN.md holds
the original design and its reasoning. Commit messages hold why the code is
shaped the way it is; `jj log` and blame are the place to look for history.

## Development

```sh
go build ./...
go test ./...
nix flake check
```

direnv sets up the environment. This is a single-author repo: commit straight
to `main`, keep the tree green, and show the final change and message once
before advancing the `main` bookmark.

## Keeping this file useful

Every agent in this repo loads this file, so it holds only broad rules that
stay true as the code changes. History, incidents, measurements, and the
reasoning behind one function belong in commit messages or a comment beside
that code. When adding here, ask whether it would still be true after the
feature it mentions is rewritten. If not, it goes somewhere else.

## Code

- stdlib first, `net/http` over frameworks. Shell out to `jj`, `gh`, `tmux`,
  and `rex` with `os/exec` rather than pulling in SDKs. Linear is the
  exception: one small GraphQL adapter.
- Ask other tools for structured output (`jj -T` templates, `--json`, `-F`
  formats) and never parse their human-readable rendering. That rendering
  changes without notice, and tools like jj upgrade automatically here.
- Comments explain why the code is the way it is now. Leave out how it got
  there.

## Boundaries

- `.rig/manifest.toml` is rig's private format. Other tools read
  `rig info --format=json` and `rig ls --format=json`; that JSON is the
  compatibility contract.
- `internal/mux` is the only seam to a multiplexer, and it speaks rig's
  vocabulary. Only the backend packages know tmux or Rex specifics. `mux` does
  not know what an agent is.
- Treat a backend call as expensive. On Rex each one is a process spawn, and on
  a remote surface each one is a network round trip. Batch per backend, never
  call per row in a loop, and keep calls to other hosts off any render path.
- Verbs on a rig that lives on another host are forwarded to that host as
  `rig <verb>` (`remote.go`), so both hosts share one implementation. Keep
  the `forwardableVerbs` allowlist narrow.
- Settings live under `~/.config/rig/` and are statements the user made.
  Derived state lives under `~/.local/state/rig/`. Caches are disposable.

## Data and compatibility

- Preferences resolve narrowest scope first: flag, then env var, then
  `rig config`, then the built-in default. A preference applies only to new
  rigs. A rig records what it was built with, and an empty recorded field
  keeps meaning the default it meant when written.
- Agent stores key on absolute paths, so a rig's basedir and workspace paths
  never move once created.
- Commands resolve by unique prefix through `resolveCommand`, and an exact
  name always wins. A new command must not change what an existing
  abbreviation means. `__`-prefixed internals match exactly and never appear
  in suggestions.
- Every command has an entry in the `commands` table in `help.go`. Help text
  says what to type and what happens, written for an agent reading it cold.

## Safety

- Destroying a rig goes through `prepareTeardownJob`, which records a
  tombstone before anything is deleted. Nothing may delete a rig by a path
  that skips it.
- Only a person decides a rig's fate, through `down` or `sweep`. Unattended
  processes (reap, timers, serve) may only carry out decisions already
  recorded.
- Privileged steps are minimal, non-destructive where possible, and fail
  closed. Never elevate a delete.
- Anything that can't deliver, can't reach a host, or can't read a source
  says so loudly. Never fall back silently to an answer that looks like
  "nothing there".
- TUIs never write to stderr while drawing. Errors appear on the board as a
  row or banner.

## Tests

- The suite never touches real user state or reaches another host.
  `TestMain` in `safety_test.go` pins the XDG dirs, and helpers like
  `isolateRigConfig` handle per-test config. New code that reads a new
  location gets the same treatment.
- Tests that need privileges or a second uid skip unless explicitly enabled.
  Don't simulate one with a weaker stand-in, since a fake that passes for
  the wrong reason hides the bug it should catch.
- The e2e suite runs real jj and tmux. Prefer it over mocks when a change
  depends on how those tools behave.
