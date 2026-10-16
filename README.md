# rly

Relay log bundles to a centralized location used for development and test environments.

`rly` snapshots what's happening on one machine (tee'd logs, HTTP state
endpoints, DB backups) into a single timestamped archive and pushes it over
your existing SSH access to another machine — typically the box where an AI
coding agent is working, so it can read the logs directly instead of you
copy-pasting terminal output.

- **Bundle, don't stream**: each run is one `YYYYMMDD-HHMMSS-bundle.tar.gz`.
- **No duplication**: each bundle holds only what's new since the last one,
  so every log line is sent exactly once.
- **No code changes**: processes just `tee` their output into a log dir.
- **Bounded**: a bundle over `max_bundle_bytes` (default 50M uncompressed)
  needs your OK before it's built; nothing is ever silently dropped.
- **Graceful**: anything unreachable is skipped and noted in `MANIFEST.md`.
- **Only ssh needed**: the push is a single `ssh` call; no rsync, no new
  server or credentials. Aliases from `~/.ssh/config` work as-is.

**Scope: dev/test only.** `rly` is for the development and testing loop, not
production. For production observability, use a proper centralized logging
stack such as Grafana Loki, OpenSearch, or the ELK stack, with shippers like
Vector, Fluent Bit, or the OpenTelemetry Collector.

## Install

```sh
go install github.com/nosaka4i/rly@latest
```

## Quick start

```sh
cd ~/proj                # any project directory; git not required
rly init                 # creates .rly/ (at the git root if inside a repo)
$EDITOR .rly/config      # set remote target, log sources, snapshots; check it in
./server 2>&1 | tee -a logs/server.log     # however your processes run
rly bundle               # bundle + push
rly push ./test-output.txt   # push one file, timestamp-prefixed
```

## Commands

| Command | |
|---|---|
| `rly bundle` | Bundle what's new since the last bundle into the staging dir, push it. `--full` for everything |
| `rly push FILE` | Push a single file as `<timestamp>-<name>`; skips all gathering. Works outside a project with `--remote` |
| `rly init [DIR]` | Create `.rly/` in the project (git root, else current dir, or DIR): config template, `.gitignore`, staging dir |
| `rly config` | Print the resolved configuration |

Running `rly` with no command prints help. Every command takes `--help`.

Tab completion: `source <(rly completion bash)` (also `zsh`, `fish`,
`powershell`).

## Configuration

Each project directory — a git repo or any other folder — has its own
`.rly/` directory:

```
.rly/
  config         # what to collect — TOML, checked in
  config.local   # personal overrides (e.g. your ssh alias) — gitignored
  staging/       # bundles land here — gitignored
  state.json     # what earlier bundles already sent — gitignored
  .gitignore     # written by `rly init`
```

`rly` finds it by walking up from the current directory, the way git finds
`.git`, so running from any subdirectory uses the same project. Relative
paths in the config are resolved from the project root. Outside a project, `rly`
tells you to run `rly init`; it never guesses.

Precedence: **flag > env var > `.rly/config.local` > `.rly/config`**.
`config.local` uses the same keys; tables merge key by key, and arrays
replace.

```toml
# staging_dir = ".rly/staging"   # default
# max_bundle_bytes = "50M"       # default; ask before bigger bundles

[remote]
target = "devbox"            # ssh alias, host, or user@host
# path = "~/.rly/inbox/proj" # default: ~/.rly/inbox/<project dir name>
# ssh = "ssh -p 2222"

# Log sources: any number, wherever the logs live. No default.
[[logs]]
path = "logs/*.log"          # glob, relative to the project root
skip = ["noisy"]             # optional; "noisy" matches noisy.log

[[logs]]
path = "~/.ollama/logs/server.log"
name = "ollama.log"          # optional name inside the bundle (files only)

[[http]]
name = "health"
url = "http://localhost:3000/health"
timeout = "10s"

# WAL-safe sqlite backup; {out} is the path the command must write to.
[[snapshot]]
name = "db"
command = "sqlite3 data/app.db \".backup {out}\""
output = "app.db"
```

| Setting | Env var | Flag |
|---|---|---|
| config file | `RLY_CONFIG` | `--config` |
| `staging_dir` | `RLY_STAGING` | `--staging` |
| `remote.target` | `RLY_REMOTE` | `--remote` |
| `remote.path` | `RLY_REMOTE_PATH` | `--remote-path` |
| `[[logs]]` sources | `RLY_LOGS` (comma-separated) | `--log PATH` (repeatable) |
| `max_bundle_bytes` | `RLY_MAX_BUNDLE_BYTES` | `--max-bundle-bytes` |

`--log`/`RLY_LOGS` replace the configured log sources for that run (paths
relative to the current directory). Each bundled log is named after its
file, or `name`; two sources landing on the same name is an error, so set
`name` on one of them.

Sizes accept `512K`, `50M`, `1.5G` (base 1024) or a plain byte count.

`--no-push` builds the bundle locally only. With no remote target
configured, the bundle is kept locally and `rly` says so.

## Incremental bundles

`rly bundle` records in `.rly/state.json` how far into each log it has
sent, and a hash of each HTTP response and snapshot. Each bundle then holds
only:

- **logs**: complete lines appended since the last bundle (a trailing
  partial line waits for the next one). A log's first bundle carries the
  whole file, and a log that shrank or was replaced (new inode) is sent
  again from the start. There are no line caps.
- **HTTP / snapshots**: only if the content changed.

If nothing is new, no bundle is made. State advances as soon as a bundle
is written locally; a bundle whose push fails (or `--no-push`, or no
remote) stays **pending** and is pushed, oldest first, at the start of the
next `rly bundle` — so each piece of data lives in exactly one bundle.
`--full` ignores the state and sends everything, every log from its start.
Delete `state.json` to start over.

### Size limit

Before compressing, `rly bundle` adds up the bundle's uncompressed size. If
it's over `max_bundle_bytes` (default 50M — about 5M on the wire, since text
logs compress roughly 10:1):

- **at a terminal**, it asks: `bundle would be 180M (~18M compressed), over
  max_bundle_bytes 50M; continue? [y/N]`. Anything but `y` aborts with
  nothing bundled and the state untouched — no data is lost or skipped.
- **without a terminal** (scripts), it exits with an error, again bundling
  nothing.
- `--yes` / `-y` skips the question. `--full` is never limited.

## Authentication

`rly` handles no credentials and doesn't use rsync. Each push is one plain
`ssh` call with the file on stdin:

```sh
ssh <target> 'mkdir -p <path> && cat > <tmp> && mv <tmp> <final>'
```

So authentication is whatever `ssh` already does for that target:
`~/.ssh/config` aliases, `IdentityFile`, `ssh-agent` / the macOS Keychain,
`ProxyJump`, and so on. To use a different command, set
`[remote] ssh = "ssh -p 2222"`.

**Recommended: key-based auth**, so pushes never prompt:

```sh
ssh-keygen -t ed25519        # only if you don't have a key yet
ssh-copy-id devbox           # installs your public key on the remote
```

Optionally give the host an alias in `~/.ssh/config`:

```
Host devbox
  HostName 203.0.113.10
  User ubuntu
  IdentityFile ~/.ssh/id_ed25519
```

Verify with `ssh devbox true`: if that connects without a password, `rly`
will too.

**Without a key:** if the server allows password auth, `ssh` prompts on the
terminal on every run (it reads the password from the tty, not stdin, so
this works). Many servers and cloud VMs disable password auth, in which case
the push fails with `Permission denied`. `rly bundle` keeps the bundle
locally as pending when a push fails, and pushes it on the next run.

## Bundle layout

```
20261015-143000-bundle/
  MANIFEST.md        # previous bundle; what's new (with byte ranges),
                     # unchanged, and skipped and why
  logs/*.log
  http/<name>.json|.txt
  snapshot/<output>
```

Snapshot commands run via `sh -c` in `dir` (default: the project root),
with `{out}` replaced by the shell-quoted output path and `$RLY_OUT` set to
it.
