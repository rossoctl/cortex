# Bundled script plugins and `agentop script` — Design

Status: draft for review, 2026-10-26. Cortex only; the moca change that uses it comes later.

## Problem

1. Installing a script plugin is a manual chore: copy files into `~/.cortex/plugins/<dir>/`, set the exec bit, and add a `script` entry to `~/.cortex/config.yaml`. `moca-sandbox` is the first such script, and more are expected soon.
2. Release tarballs carry only the binaries, so a script in `examples/script-plugins/` is not available to an installed Cortex at all.
3. A tool that wants to install a script for its users (moca next) has nothing to call. It would have to know Cortex's file layout and YAML shape.

## Goal

- Cortex ships its script plugins inside the `agentop` binary.
- `agentop script install <name>` sets one up in a single step, and `agentop script remove <name>` takes it out.
- Any user, and any tool, uses the same command.

Out of scope:

- moca's side (`sbx up` calling this command), which is a later PR;
- installing scripts from anywhere but the binary: a URL, a path, a registry;
- choosing a script's version other than the one the installed release bundles.

## Model

The model is today's script plugin. Nothing in the plugin or the proxy changes.

- **A slot is a directory.** It is one `script` entry in the config, and its `dir` names a folder inside `~/.cortex/plugins/`.
- **A slot runs every executable file directly in its folder,** in byte order of the file names. A file starting with `_` or `.` is not run.
- **A slot's settings apply to all its scripts:** `fail_closed` and `timeout`.

The catalog adds one rule on top: **a catalog entry is a directory, and installing it makes one slot.**

- The entry's folder becomes `~/.cortex/plugins/<name>/`, and the slot gets `dir: <name>`.
- The entry decides its slot's settings, not the caller.
- An entry is never installed into another slot, including the default `~/.cortex/plugins/` one.

## The catalog

It lives at `core/plugins/script/catalog/`, next to the SDK, which is embedded the same way. `go:embed` cannot reach `examples/` from a Go module.

```
core/plugins/script/catalog/
  moca-sandbox/
    entry.yaml
    moca-sandbox.py
```

**An entry is one folder:**
- one or more scripts, run in file-name order, as in any slot;
- any `_`-prefixed helper files;
- `entry.yaml`, which is metadata and not a script.

`entry.yaml` holds:

```yaml
description: Keep Claude Code on moca's sandbox tools without a project settings file
fail_closed: true      # optional; default false
```

`timeout` may be set the same way. When it is absent, the slot uses the plugin's default (2s).

**The Go API,** in package `script`, is read-only:

```go
type CatalogEntry struct {
	Name        string            // the folder's name
	Description string
	FailClosed  bool
	Timeout     string            // "" when entry.yaml sets none
	Files       map[string][]byte // every file but entry.yaml, by name
}

func Catalog() ([]CatalogEntry, error) // sorted by Name
```

`examples/script-plugins/` is removed. `moca-sandbox.py` moves into the catalog with its git history, and its pytest and the docs follow it.

**One test covers every entry,** so adding a script is adding a folder:
- `entry.yaml` parses and has a description;
- the name is a valid `dir` (`script.SlotDir` accepts it);
- the folder holds at least one script, and each script's first line is `#!`;
- each script sends a valid handshake when run with the SDK on its path (skipped where `python3` is missing, as the SDK tests are).

## `agentop script`

```
agentop script list [--json]
agentop script install <name> [--config PATH]
agentop script remove <name> [--config PATH]
```

The config is `~/.cortex/config.yaml` unless `--config` names another, as for `agentop server`. Cortex's own state comes from the stats and session API addresses in that config.

### State of an entry

Each catalog entry is in exactly one state:

| State | Meaning |
|---|---|
| `not installed` | no slot with `dir: <name>`, and no folder |
| `installed` | the slot is there, and the folder holds exactly this release's files |
| `outdated` | as installed, but the files match what an earlier `install` wrote and differ from this release's |
| `modified` | the folder's files differ from what `install` wrote |
| `incomplete` | only one of the slot and the folder is there |

What `install` wrote is recorded in `~/.cortex/plugins/<name>/.agentop-install.json`: the Cortex version and each file's SHA-256. The leading `.` keeps the slot from running it.

With a running Cortex, `list` adds what the proxy reports for that slot: `scripts loaded` and `scripts not started`, from `GET /v1/pipeline`.

### `list`

```
NAME          STATE      LOADED  DESCRIPTION
moca-sandbox  installed  1/1     Keep Claude Code on moca's sandbox tools without a project settings file
```

`LOADED` is blank when Cortex is not running. `--json` gives the same rows: `name`, `description`, `state`, `failClosed`, and when Cortex answers, `loaded` and `notStarted`. Exit 0.

### `install <name>`

1. **Refuse** when the name is not in the catalog, or the state is `modified`. A `modified` entry may hold the user's own edits, so the message says to remove it first.
2. **Write the files** to `~/.cortex/plugins/<name>/`:
   - the folder is mode 0700, scripts 0700, other files 0600;
   - each file goes through a temporary file and a rename, so the reloader never sees half a script;
   - the record is written last.
3. **Add the slot,** if no outbound `script` entry has `dir: <name>`:
   - `{name: script, config: {dir: <name>, fail_closed: …, timeout: …}}`, with only the keys `entry.yaml` sets;
   - it goes in as the last outbound entry, through `edit.WritePluginConfig`, which keeps the file's comments, checks the result loads, waits for the reload, and puts the file back if the proxy refuses it.
4. **Check that it loaded.** The slot must show `scripts loaded` equal to the entry's script count and `scripts not started` 0, within 10 seconds of the reload.
5. **On any failure** after step 2, take back what this run added, in reverse order: the slot, then the files. A slot or files that were already there stay. Exit 1, with the reason, for example "moca-sandbox did not start: python3 not found".
6. **When Cortex is not running,** steps 2 and 3 still run and step 4 is skipped. The output says the slot loads when Cortex starts. Exit 0.

**Running it again is safe:**
- `installed`: prints "already installed" and changes nothing;
- `outdated`: rewrites the files and keeps the slot;
- `incomplete`: adds whichever half is missing.

### `remove <name>`

1. Refuse when the folder holds a file the record does not list. That file is the user's, so the message names it. A recorded file that was edited is still removed: `install` wrote it.
2. Remove the slot through `WritePluginConfig`. A file the proxy refuses is put back, and nothing else is removed.
3. Remove the recorded files, the record, then the folder.

Exit 0 when nothing was installed, saying so.

### Finding the slot

agentop's config editor (`edit.SetPluginConfig`) finds an entry by plugin name, and refuses when two entries share one. Every slot is named `script`, so it gains one optional field:

```go
type ConfigChange struct {
	...
	// Match, when set, picks the entry among those named Plugin whose config:
	// holds every key/value in it, such as {"dir": "moca-sandbox"}.
	Match map[string]string
}
```

- With `Match`, `findEntry` skips entries whose config does not match. Two matches is still an error.
- `CreatePlugin` writes the `Match` keys into the new entry's `config:`.
- A removal of the whole entry is new: `Value` nil and `Path` empty, with `Match` set.

The entry's `id` field is not used: nothing in Cortex reads it today.

## Errors

| Case | Result |
|---|---|
| no Cortex config | "no Cortex config at ~/.cortex/config.yaml; install Cortex", exit 1, as `agentop server` says |
| unknown name | lists the catalog's names, exit 2 |
| config changed while agentop worked | `WritePluginConfig`'s "changed while working", nothing written, exit 1 |
| proxy refuses the config | file put back, files removed, proxy's error shown, exit 1 |
| proxy stops answering mid-reload | as `WritePluginConfig` reports it; files removed, exit 1 |
| script does not start | slot and files removed, the slot's error from the pipeline API shown, exit 1 |
| not a local install (no `~/.cortex`) | the first row |

## Docs

- `docs/script-plugin.md`: "Examples" becomes "Bundled scripts". It says what `list`, `install` and `remove` do, and the moca-sandbox section's install steps become `agentop script install moca-sandbox`.
- `cmd/agentop/README.md`: the new subcommand.
- `CLAUDE.md`: the directory tree loses `examples/script-plugins/` and gains `core/plugins/script/catalog/`.
- How to add a catalog entry: one short section in `docs/script-plugin.md`.

## Testing

- **Catalog:** the per-entry test above, in `core/plugins/script`.
- **Editor:** `Match` finds the right one of three `script` entries; two matches is refused; create writes the match keys; whole-entry removal keeps the file's other comments.
- **Commands,** against a temp HOME, a config written by `cortex --local --write-config`, and a fake stats + session API:
  - each state is reported correctly;
  - install from each state, including the rollback on a refused reload and on a script that does not start;
  - install and remove with no proxy running;
  - remove refuses an unrecorded file;
  - `list --json`'s field names are pinned, since other tools will read them.
- **Live,** before the PR is opened: a scratch Cortex built from the branch, on ports 49700-49704. `install moca-sandbox` loads it with no restart, `list` shows it, and the local-call renaming works. A broken edit shows `modified`. `remove` leaves the config as it was before the install.
- `go vet`, `go test -race` and `gofmt -l` in `core` and `cmd/agentop`; `pytest tests/`.

## Size

One PR, about 800 lines of code and 1,250 of tests. The commands themselves are about 500 and 800 of that.

After it merges: cut v0.9.3, which the moca change will require.
