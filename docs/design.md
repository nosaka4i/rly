# Relaying dev logs and state to a remote agent

**Status: built — see [main.go](../main.go), [bundle.go](../bundle.go), [state.go](../state.go), [config.go](../config.go), [push.go](../push.go), [size.go](../size.go), [fileid_unix.go](../fileid_unix.go).**

## Context

The AI coding agent runs on a small Linux box in the US. The app it is
working on (nosaka: a node server, plus ollama and whisper.cpp, which need
far more than the box's ~2GB of RAM) runs and is tested on a Mac in China.
The agent can't reach the Mac (NAT, firewall), and Claude isn't offered
there, so the agent can't run on the Mac either. Until rly, the only way
the agent saw a test run was the user copy-pasting terminal output, and a
bash prototype (`tools/rly/rly.sh` in the nosaka monorepo; see
[requirements.md](requirements.md)) that bundled hardcoded nosaka logs.

rly is the generic, open-source replacement: the Mac pushes, over SSH
access the user already has, exactly what the agent needs to read. It is
for the **dev/test loop only**. For production use a real logging stack:
Grafana Loki, OpenSearch or ELK, fed by Vector, Fluent Bit or the
OpenTelemetry Collector.

Non-goals: live streaming, process supervision (starting and stopping the
app is the dev launcher's job), general-purpose file sync.

## The mechanism

### CLI ([main.go](../main.go))

Built with cobra. A fresh command tree per `run()` so tests can call it
repeatedly.

| Command | |
|---|---|
| `rly` | prints help, never acts |
| `rly bundle` | gather what's new, archive, push (`--full`, `--no-push`, `--yes`) |
| `rly push FILE` | push one file as `<timestamp>-<name>`; works without a project given `--remote` |
| `rly init [DIR]` | create `.rly/` at DIR, else the git root, else cwd |
| `rly config` | print the merged, resolved config |
| `rly version` | `0.0.1` |

`completion` exists but is hidden from help.

### Project layout and discovery ([config.go](../config.go))

Every project directory (a git repo or any folder) has its own `.rly/`:

```
.rly/config        TOML, checked in: what to collect
.rly/config.local  personal overrides (ssh alias…), gitignored
.rly/staging/      archives, gitignored
.rly/state.json    what has been sent, gitignored
.rly/.gitignore    written by init
```

`findProject` walks up from cwd to the nearest `.rly/config`, like git
finds `.git`; `--config`/`RLY_CONFIG` overrides it. No project means an
error telling you to run `rly init`, never a guess.

Precedence: **flag > env > config.local > config**. `config.local` is
overlaid on `config` as raw TOML maps (`mergeMaps`): tables merge key by
key, scalars and arrays replace. Relative paths in config resolve from the
project root; paths given as flags or env resolve from cwd, as typed.

Keys: `staging_dir`, `max_bundle_bytes` (`ByteSize` in
[size.go](../size.go): `"50M"`, `"1.5G"` or an integer), `[remote]
target/path/ssh`, `[[logs]] path/name/skip`, `[[http]] name/url/timeout`,
`[[snapshot]] name/command/output/dir/timeout`. The remote path defaults to
`~/.rly/inbox/<project dir name>`. There is no default log source.

Log sources are where the logs already are; rly imposes no layout:

```toml
[[logs]]
path = "logs/*.log"                  # project-relative glob (nosaka tees here)

[[logs]]
path = "~/.ollama/logs/server.log"   # a file where a tool keeps it
name = "ollama.log"                  # name inside the bundle
```

`name` is only allowed on a non-glob `path`. Two sources resolving to the
same bundle name is an error before anything is gathered. `--log PATH`
(repeatable) or `RLY_LOGS` (comma-separated) replaces the configured
sources for one run. An old `[logs]` table (`dir`/`pattern`) is rejected
with the new form in the error message.

### Bundle pipeline ([bundle.go](../bundle.go))

`buildBundle` stages into a temp dir under `staging/`, then:

1. **Logs**: each file matched by a `[[logs]]` source (minus that
   source's `skip`) is copied into `logs/<name>` from its saved offset to
   its last `\n`. A source matching nothing is noted under "Skipped". A partial trailing line waits
   for the next bundle. The whole file goes when there is no saved offset
   (first bundle), when the file shrank or its inode changed
   ([fileid_unix.go](../fileid_unix.go)), and with `--full`. There are no
   line caps.
2. **HTTP**: GET each target. The body is saved (even on non-2xx) and kept
   only if its sha256 differs from the last bundle.
3. **Snapshots**: `sh -c` the command from the project root, with `{out}`
   replaced by the shell-quoted output path (and `$RLY_OUT`). For example
   `sqlite3 app.db ".backup '{out}'"`, which is WAL-safe where `cp` isn't.
   The output is kept only if its hash changed.
4. Nothing new means no bundle ("nothing new since …").
5. **Size check**: before compressing, sum the staged bytes. If the total
   is over `max_bundle_bytes` (default 50M, ~5M gzipped) and this isn't
   `--full`, call `confirmSize`: `--yes` proceeds; a terminal gets
   `continue? [y/N]`; no terminal means an error. Declining bundles
   nothing and saves no state.
6. Write `MANIFEST.md` (previous bundle, new log lines with byte ranges,
   unchanged items, skipped items with reasons). Then write
   `YYYYMMDD-HHMMSS-bundle.tar.gz` atomically (`.part` then rename),
   moving the name forward a second if it collides.

One failing source never fails the bundle; it is listed under "Skipped".

### State and zero duplication ([state.go](../state.go))

`state.json` holds the last bundle name, per-log `{offset, dev, ino}` keyed
by the log's absolute source path (so renaming `name` or reordering
sources never resends anything), the
HTTP and snapshot hashes, and `pending`: archives not yet pushed, oldest
first. State is saved atomically **as soon as the archive is written
locally**, and the archive joins `pending`. Every `rly bundle` first pushes
pending archives in order, stopping at the first failure, and then pushes
the new one. So each byte lives in exactly one archive, and a failed push
is retried rather than re-bundled. A corrupt state file is treated as a
first run, with a warning.

### Push transport ([push.go](../push.go))

There is one `ssh <target> '<script>'` per file, with the file on stdin:

```
mkdir -p DIR && cat > DIR/.NAME.part && mv -f DIR/.NAME.part DIR/NAME
```

The remote needs only a POSIX shell. A leading `~` in the remote path is
expanded remotely as `"$HOME"`, and everything else is single-quoted.
Authentication is the user's own ssh setup (config aliases, keys, agent,
ProxyJump); rly handles no credentials. `remote.ssh` swaps the command,
e.g. `ssh -p 2222` (tests use it to point at a fake ssh).

### Testing ([rly_test.go](../rly_test.go))

- **Remote:** a fake `ssh` script that runs the remote command locally with
  `HOME` set to a temp dir, so the remote side is a real directory.
- **HTTP:** `httptest` servers.
- **Discovery:** `t.Chdir` exercises project discovery.
- **Timestamps:** `nowFunc` gives distinct ones.
- **Size prompt:** `stdinIsTerminal`/`promptIn` drive it.

## Why TOML, not YAML or JSON

The file is hand-edited and checked in, so it needs comments (JSON has
none). Its structure is shallow: a few tables and two arrays of tables.
TOML fits that without YAML's indentation and typing pitfalls.

## Why cobra, not stdlib `flag`

It is the user's standard for every Go CLI. It brings generated per-command
help, completion, and flags that can appear before or after positional
arguments. stdlib `flag` was the first cut and was replaced.

## Why bare `rly` prints help, not bundles

The bash prototype bundled when run without arguments. But bundling sends
data, possibly a DB snapshot, to another machine, which is the wrong thing
for a curious no-argument run to do. docker, kubectl, git and gh all print
help.

## Why per-project `.rly/`, not `~/.rly/config`

The first Go cut kept one config and staging dir in `$HOME`. But what to
collect (log dir, endpoints, snapshot command) belongs to the project and
should be checked in. The bash prototype's original sin was a silent `$(pwd)/.debug` that
scattered bundles across subdirectories. Walking up to the nearest `.rly/`
fixes that, as `.git` discovery does, without a global file. Personal bits
go in `config.local`.

## Why a list of log sources, not one log dir

rly first read one directory (`[logs] dir` + `pattern`), defaulting to
`.rly/logs/`. That forced every project into one layout: all logs in one
folder, and implicitly inside rly's own working directory. That is awkward
for logs that aren't the project's (ollama, whisper.cpp) or that a tool
already writes somewhere conventional (`~/.ollama/logs/`,
`~/Library/Logs/<app>/`). A list of paths and globs covers both styles:
one glob for a project that tees everything into its own folder (nosaka
uses `logs/*.log`), or one entry per file where tools keep them. With no
default, `.rly/` holds only rly's own files.

## Why one ssh pipe, not rsync or native SFTP

- **rsync**: the bash version used it, but macOS ships an old rsync (or
  openrsync) without `--mkpath`, and needing rsync on both ends buys
  nothing for one-file pushes.
- **Native Go SSH/SFTP**: this would have to reimplement `~/.ssh/config`
  parsing, agents, known_hosts and ProxyJump, and would drift from what
  `ssh` does.

Shelling out to `ssh` reuses all of the user's setup exactly. There is no
daemon on either side; `sshd` is already there.

## Why incremental bundles, not full snapshots each run

Full snapshots repeated about 9,800 of 10,000 log lines and the whole DB on
every run, and piled up identical archives on both machines. Logs are
append-only (tee never rewrites), so a byte offset is a complete record of
what has been sent. State is the HTTP and snapshot output, which only
needs sending when it changes. `--full` remains for an explicit standalone
snapshot.

## Why advance state on local write, not on successful push

If state only advanced after a push, a failed push would leave its data to
be bundled again next run, while the failed archive also sat in staging:
duplicate data. A pending queue keeps every byte in exactly one archive and
still guarantees delivery order.

## Why a confirm-before-send size limit, not tail lines or a silent trim

`tail_lines` existed because full-snapshot bundles carried an ever-growing
file. With incremental bundles it only dropped data, and with zero
duplication anything dropped is never sent. A silent per-log byte cap had
the same flaw. Checking uncompressed size before compressing is cheap and
early (the compressed size is only known at the end). Asking keeps the user
in control, refusing without a terminal keeps scripts safe, and declining
loses nothing.

## Related, deferred separately

- **`rly sync`**: a live, de-duplicated mirror (`<inbox>/live/`), appending
  log bytes and replacing state files over one ssh call per sync, with an
  optional `--watch`. Same state model, different output shape. Deferred
  until bundles prove insufficient.
- **Agent-pulled bundles**: a reverse ssh tunnel from the Mac (`autossh
  -R`) with a forced command in `authorized_keys`, so the agent can trigger
  `rly bundle` on demand without a general shell on the Mac.
- **Bundle retention**: pruning old archives from `staging/` and the
  remote inbox. Not needed while bundles are incremental and small.
- **Syncthing for a project's log folder**: continuous log mirroring as
  an alternative, outside rly.
