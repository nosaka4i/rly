# `rly` — requirements for the standalone Go rewrite

Source: distilled from `tools/rly/rly.sh` in the `nosaka` monorepo (bash
prototype, itself renamed from `metri`/`metry`/`tell`). That script is
being treated as throwaway reference material, not something to port
line-by-line — this doc captures the *behavior and reasoning* worth
keeping, generalized away from nosaka-specific hardcoding since this is
becoming an open-source tool for agentic-coding workflows generally, not
a nosaka-only script. Written 2026-10-15, for copying into the new
standalone repo once it's set up.

## Problem statement

When an AI coding agent runs on a remote box (a cloud dev session, a
Linux VM) but the thing being developed actually runs/is tested on a
different machine (a Mac, for GUI apps or anything needing real
hardware), the agent has no direct way to see that machine's logs, DB
state, or live service health. Today that means manual copy/paste of
terminal output. `rly` automates the "gather everything useful, push it
to wherever the agent is working" step: one command, one timestamped
archive, pushed over an existing SSH connection.

This is explicitly part of a small toolkit (alongside a dev-process
launcher and a mock-pulling tool) aimed at closing real gaps in
present-day agentic coding workflows — not a one-off script.

## Core behavior to preserve

- **Bundle, don't stream.** A snapshot of current state, not a live
  tail. Re-run for a fresh snapshot.
- **One archive per run**, timestamped, not loose files — easy to
  hand off/delete as a unit.
- **`tee`-based log capture, not a code change.** The tool never
  modifies how a target process logs — it just expects that process's
  stdout/stderr already piped through `tee` into a known directory.
  Nothing upstream (the server, whisper.cpp, ollama, whatever) needs to
  know `rly` exists.
- **Bounded log size.** Captured logs are the last N lines, not the
  whole file — the underlying log files `tee` writes are opened once and
  never rotate, so for a long-running dev process the full file only
  grows. Default-bounded (bash version: 10k lines), overridable per run.
  *(Superseded 2026-10-15 by incremental bundles + `max_bundle_bytes`;
  see the amendments under "Decided".)*
- **Push over existing SSH access**, not a new server/credential — same
  trust boundary the user already has to that machine.
- **Both a full bundle AND a single-file push mode** — sometimes you
  just want to hand over one file (a script's own run output), not
  gather the whole environment.
- **Graceful partial failure.** Any one log/endpoint/DB that isn't
  currently reachable gets skipped with a note, never a hard failure —
  running with only some of N configured sources up is the common case,
  not an edge case.

## What must become generic/config-driven (was hardcoded in the bash prototype)

The bash version is tightly coupled to nosaka's own stack. All of this
needs to become project-level config in the Go version, not knowledge
baked into the tool itself:

- **Which logs to bundle.** Bash version originally hardcoded exactly
  `server`/`whisper`/`ollama`; later generalized to "glob every `*.log`
  in one configured directory." The Go version should keep the
  glob-a-directory approach (simpler than an explicit per-log list, and
  self-updating when a new process starts logging into that directory)
  but the directory itself, and any excluded names, must be per-project
  config, not a nosaka-specific default.
- **Live state snapshots.** Bash version hardcoded three nosaka HTTP
  endpoints (`/memory/export`, `/jobs`, `/health`) against a hardcoded
  `localhost:3000`. Needs to become an arbitrary list of named HTTP GET
  targets in config — zero, one, or many, project-defined.
- **DB snapshot.** Bash version hardcoded a `sqlite3 ... .backup`
  command against a hardcoded `nosaka.db` path, specifically because
  that DB runs in WAL mode (a plain file copy risks missing recent
  writes still sitting in a separate `-wal` file while the process is
  live). Needs to become an arbitrary, optional, project-defined
  "snapshot command" (could be `sqlite3 ... .backup`, could be
  `pg_dump`, could be nothing) — the WAL-mode reasoning is why this
  needs to be a real command a project configures, not a `cp`.
- **Remote push target.** Bash version hardcoded an SSH alias
  (`nosaka`) and had a separate, always-explicit remote-side path env
  var. Both need to be ordinary config/flags, not assumed.

A reasonable shape: a per-project config file (e.g. `.rly.toml` or
similar) the user writes once, declaring: log directory, excluded log
names, a list of `{name, url}` HTTP snapshot targets, an optional DB
snapshot command + output filename, and the remote SSH target + path.
`rly` itself ships with zero project-specific knowledge.

## Local staging directory — the one piece already redesigned, keep this

The bash prototype went through three stages worth preserving as the
final answer, not re-deriving:

1. Originally defaulted silently to `$(pwd)/.debug` — rejected because
   running the tool from different subdirectories of the same project
   without noticing scattered bundles across multiple disconnected
   `.debug` dirs with zero feedback that it was happening.
2. Added an explicit `*_HOME`-style env var override — better, but still
   silently fell back to the cwd-dependent default if unset.
3. **Final shape: an explicit `init` subcommand** that creates a
   directory and persists its path to a small local config file, which
   every other invocation reads as its fallback. No env var set, no
   config file yet → **hard error telling the user to run `init`**, never
   a silent cwd-dependent guess.

Precedence for the Go version should keep this same shape: an explicit
flag/arg for one-off overrides > an env var for session-level overrides
> the path `init` saved > error-and-tell-them-to-run-init. Never fall
back to the working directory silently.

## Single-file push mode

Needed independently of the full bundle: push one arbitrary file (e.g.
a test script's saved output) to the same remote location, timestamp-
prefixed the same way, skipping all the snapshot/log-gathering/archive
steps entirely. Real reason this is a separate code path and not just
"call the bundle path with one file in a temp dir": pushing a single
file should only ever transfer that one file, not scan/diff an entire
accumulated local staging directory just to send one new thing.

## Explicitly NOT required

- Not a live/streaming log tail.
- Not a process supervisor/manager (doesn't start or stop the target
  processes — that's the separate dev-launcher tool in the same
  toolkit).
- Not a general file-sync tool — one-way push only, point-to-point,
  same shape as `scp`.

## Decided

- **Config file location/naming: `~/.rly/config`.** Decided 2026-10-15,
  following well-known CLI convention rather than the bash prototype's
  own ad hoc choice (`tools/rly/.rly-home`, a bare dotfile sitting next
  to the script, repo-relative). Docker keeps `~/.docker/config.json`;
  kubectl keeps `~/.kube/config` — both a plainly-named `config` file
  inside a dedicated per-tool dot-directory, user-home-level rather than
  repo-relative. The bash prototype deliberately was NOT changed to
  match this — this was documented for the Go rewrite to implement
  fresh, not retrofitted into throwaway code.
- **Config file is the source of truth; env vars/flags are override-only.**
  The bash prototype used env vars (`RLY_HOME`, `RLY_REMOTE_HOME`,
  `LOG_DIR`, `LOG_TAIL_LINES`, `SKIP_LOGS`) as the primary way to
  configure anything beyond the hardcoded defaults — none of them
  persist, so every new shell/machine meant re-exporting them (or
  relying on the fallback). `~/.rly/config` should hold all five as
  persistent defaults — staging dir, remote target, log dir, tail-line
  count, skipped-logs list — and env vars/flags become purely
  per-invocation *overrides* of whatever the config says, never the
  required way to set something. Same relationship `KUBECONFIG` has to
  `~/.kube/config`: an env var can override which config gets used, or
  override one setting for one run, but nothing is required to be
  exported for normal day-to-day use.

- **Amended 2026-10-15: config is per repo, not under `~/`.** The
  `~/.rly/config` decision above is superseded: each repo has its own
  `.rly/` — `.rly/config` (checked in: what to collect), `.rly/config.local`
  (gitignored: personal overrides such as the ssh alias), `.rly/staging/`
  (gitignored: bundles). `rly` finds it by walking up from the working
  directory like git finds `.git`, so subdirectories share one project and
  there is still no silent cwd-dependent guess. `rly init` creates it at the
  git root. Logs from elsewhere (e.g. another repo) are still reachable by
  pointing a log source at them.
- **Amended 2026-10-15: bundles are incremental — zero duplication.** Each
  `rly bundle` holds only what's new since the previous bundle: log lines
  appended since then (tracked by byte offset + inode in
  `.rly/state.json`; truncated/replaced logs are resent from the start),
  and HTTP/snapshot output only when its hash changed. Nothing new → no
  bundle. State advances as soon as a bundle is written locally; unpushed
  bundles (failed push, `--no-push`, no remote) stay pending and are pushed
  oldest-first on the next run, so every piece of data lives in exactly one
  bundle. `--full` is the explicit exception (complete snapshot on request).
- **Amended 2026-10-15: bounded size is a confirm-before-send limit, not
  tail lines.** With incremental bundles the "last N lines" bound (bash:
  10k lines) no longer has a job, so it is gone: a log's first bundle,
  a truncated/replaced log and `--full` all carry the whole file, and every
  later bundle carries every new line. The bound is now `max_bundle_bytes`
  (default 50M uncompressed, ~5M gzipped at ~10:1; config, env
  `RLY_MAX_BUNDLE_BYTES`, `--max-bundle-bytes`), checked for the whole
  bundle before compressing. Over it, rly asks at a terminal (`[y/N]`) and
  refuses without one; declining bundles nothing and leaves the state
  untouched, so nothing is ever silently dropped. `--yes` skips the
  question; `--full` is never limited.
- **Amended 2026-10-15: multiple log sources, no forced layout.** The
  single `[logs] dir`/`pattern` (and its `.rly/logs` default) is replaced
  by a list of `[[logs]]` sources, each a `path` (file or glob; relative to
  the project root, `~` expands) with optional `name` (bundle name, files
  only) and `skip`. There is no default source. A project can tee
  everything into its own folder (nosaka uses `logs/*.log`) or read logs
  where tools already keep them (e.g. `~/.ollama/logs/server.log`), or mix
  both. `--log`/`RLY_LOGS` replace the sources for one run (replacing
  `--log-dir`/`RLY_LOG_DIR` and `--skip`/`RLY_SKIP_LOGS`). Sent-offsets are
  keyed by absolute file path, so earlier state carries over unchanged.

## Open questions for the new session to decide

- **Config file format.** `~/.rly/config` as JSON/YAML/TOML — kubectl's
  own file is YAML.
- CLI/flag-parsing library choice (e.g. `cobra`, `urfave/cli`, stdlib
  `flag`) and exact subcommand surface (`rly init`, `rly` bare for
  bundle+push, `rly push <file>` or similar for single-file mode,
  `rly help`).
- Whether the remote push stays rsync-over-ssh-alias (shelling out) or
  becomes a native Go SSH/SFTP client — rsync was originally chosen in
  the bash version specifically to avoid any new server/credential setup
  beyond existing SSH access; that constraint should carry over, the
  mechanism doesn't have to.
- Working name: the bash prototype is called `rly` (from "relay," vowels
  dropped, reads like "really") — carried forward as the default
  assumption here, open to reconsidering for the public repo.
