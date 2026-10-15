# rly

Relay log bundles to a centralized location.

`rly` snapshots what's happening on one machine (tailed logs, HTTP state
endpoints, DB backups) into a single timestamped archive and pushes it over
your existing SSH access to another machine — typically the box where an AI
coding agent is working, so it can read the logs directly instead of you
copy-pasting terminal output.

- **Bundle, don't stream**: each run is one `YYYYMMDD-HHMMSS-bundle.tar.gz`.
- **No code changes**: processes just `tee` their output into a log dir.
- **Bounded**: each log is tailed to the last N lines (default 10000).
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
$EDITOR .rly/config      # set remote target, log dir, snapshots; check it in
./server 2>&1 | tee .rly/logs/server.log   # however your processes run
rly bundle               # bundle + push
rly push ./test-output.txt   # push one file, timestamp-prefixed
```

## Commands

| Command | |
|---|---|
| `rly bundle` | Gather logs + snapshots, archive into the staging dir, push |
| `rly push FILE` | Push a single file as `<timestamp>-<name>`; skips all gathering. Works outside a project with `--remote` |
| `rly init [DIR]` | Create `.rly/` in the project (git root, else current dir, or DIR): config template, `.gitignore`, logs and staging dirs |
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
  logs/          # tee your processes' output here — gitignored
  staging/       # bundles land here — gitignored
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

[remote]
target = "devbox"            # ssh alias, host, or user@host
# path = "~/.rly/inbox/proj" # default: ~/.rly/inbox/<project dir name>
# ssh = "ssh -p 2222"

[logs]
# dir = ".rly/logs"         # default; relative to the project root; can point elsewhere
pattern = "*.log"
tail_lines = 10000
skip = ["ollama"]            # names or globs; "ollama" matches ollama.log

[[http]]
name = "health"
url = "http://localhost:3000/health"
timeout = "10s"

# WAL-safe sqlite backup; {out} is the path the command must write to.
[[snapshot]]
name = "db"
command = "sqlite3 data/app.db \".backup '{out}'\""
output = "app.db"
```

| Setting | Env var | Flag |
|---|---|---|
| config file | `RLY_CONFIG` | `--config` |
| `staging_dir` | `RLY_STAGING` | `--staging` |
| `remote.target` | `RLY_REMOTE` | `--remote` |
| `remote.path` | `RLY_REMOTE_PATH` | `--remote-path` |
| `logs.dir` | `RLY_LOG_DIR` | `--log-dir` |
| `logs.tail_lines` | `RLY_LOG_TAIL_LINES` | `--lines` |
| `logs.skip` | `RLY_SKIP_LOGS` (comma-separated) | `--skip` |

`--no-push` builds the bundle locally only. With no remote target
configured, the bundle is kept locally and `rly` says so.

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
locally when a push fails.

## Bundle layout

```
20261015-143000-bundle/
  MANIFEST.md        # what was included, what was skipped and why
  logs/*.log
  http/<name>.json|.txt
  snapshot/<output>
```

Snapshot commands run via `sh -c` in `dir` (default: the project root),
with `{out}` replaced by the shell-quoted output path and `$RLY_OUT` set to
it.
