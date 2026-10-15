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
rly init                 # creates ~/.rly/staging and ~/.rly/config
$EDITOR ~/.rly/config    # set remote target, log dir, snapshots
./server 2>&1 | tee ~/proj/logs/server.log   # however your processes run
rly bundle               # bundle + push
rly push ./test-output.txt   # push one file, timestamp-prefixed
```

## Commands

| Command | |
|---|---|
| `rly bundle` | Gather logs + snapshots, archive into the staging dir, push |
| `rly push FILE` | Push a single file as `<timestamp>-<name>`; skips all gathering |
| `rly init [DIR]` | Create the staging dir (default `~/.rly/staging`) and save it to the config |
| `rly config` | Print the resolved configuration |

Running `rly` with no command prints help. Every command takes `--help`.

Tab completion: `source <(rly completion bash)` (also `zsh`, `fish`,
`powershell`).

## Configuration

`~/.rly/config` (TOML) is the source of truth. Env vars and flags override
single values for one run: **flag > env var > config file**. `rly` never
guesses a staging directory from the working directory — without one it
tells you to run `rly init`.

```toml
staging_dir = "/Users/me/.rly/staging"

[remote]
target = "devbox"            # ssh alias, host, or user@host
path = "~/.rly/inbox"        # "~/" expands on the remote side
# ssh = "ssh -p 2222"

[logs]
dir = "~/proj/logs"
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
command = "sqlite3 ~/proj/app.db \".backup '{out}'\""
output = "app.db"
```

| Setting | Env var | Flag |
|---|---|---|
| config file | `RLY_CONFIG` | `--config` |
| `staging_dir` | `RLY_HOME` | `--home` |
| `remote.target` | `RLY_REMOTE` | `--remote` |
| `remote.path` | `RLY_REMOTE_PATH` | `--remote-path` |
| `logs.dir` | `RLY_LOG_DIR` | `--log-dir` |
| `logs.tail_lines` | `RLY_LOG_TAIL_LINES` | `--lines` |
| `logs.skip` | `RLY_SKIP_LOGS` (comma-separated) | `--skip` |

`--no-push` builds the bundle locally only. With no remote target
configured, the bundle is kept locally and `rly` says so.

## Bundle layout

```
20261015-143000-bundle/
  MANIFEST.md        # what was included, what was skipped and why
  logs/*.log
  http/<name>.json|.txt
  snapshot/<output>
```

Snapshot commands run via `sh -c` in `dir` (default: your home directory),
with `{out}` replaced by the shell-quoted output path and `$RLY_OUT` set to
it.
