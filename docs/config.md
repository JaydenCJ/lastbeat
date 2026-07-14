# lastbeat.toml reference

One file configures everything. `lastbeat init` writes a working starter
copy. Relative paths (like `state_file`) resolve against the directory
containing the config file, so absolute `-c` paths work from any cwd.

## Top level

| Key | Default | Effect |
|---|---|---|
| `listen` | `"127.0.0.1:8377"` | address for `serve` mode; loopback by default, set a LAN address deliberately |
| `state_file` | `"lastbeat.state.json"` | the single JSON file all runtime state lives in |
| `ping_key` | unset | when set, pings must carry it (`X-Lastbeat-Key` header or `?key=`); wrong/missing key → 403 |
| `sweep_every` | `"1m"` | how often `serve` mode evaluates checks for silence |

## `[defaults]`

| Key | Default | Effect |
|---|---|---|
| `grace` | `"5m"` | grace applied to every check that does not set its own |

## `[[check]]` — one per monitored job

| Key | Default | Effect |
|---|---|---|
| `name` | required | identifier used in ping URLs and alerts; `[A-Za-z0-9._-]`, must start alphanumeric |
| `interval` | required | maximum silence before the check is overdue; `1s` minimum |
| `grace` | from `[defaults]` | extra slack after the deadline before `down` fires |
| `alerts` | all alerts | restrict this check to the named `[[alert]]` channels |
| `tags` | `[]` | free-form strings, passed through to alert payloads |

## `[[alert]]` — one per notification channel

| Key | Default | Effect |
|---|---|---|
| `name` | required | identifier that checks reference via `alerts` |
| `url` | — | webhook: the event payload is POSTed as JSON |
| `command` | — | argv to execute; payload arrives on stdin, `{check}` `{event}` `{status}` `{note}` expand in arguments |
| `events` | `["down", "failed", "recovered"]` | which events this channel receives; add `"late"` for early warnings |
| `timeout` | `"10s"` | delivery timeout per event; `1s` minimum |

Exactly one of `url` or `command` must be set.

## Durations

Schedules use human units: `s`, `m`, `h`, plus `d` (24h) and `w` (7d).
Components combine largest-first, each unit at most once: `"90s"`,
`"1h30m"`, `"1d12h"`, `"2w"`. Bare numbers, fractions, and out-of-order
units are load-time errors — a typo in a schedule must never parse.

## Events

| Event | Fires when | Repeat behavior |
|---|---|---|
| `down` | a check passes `interval + grace` without pinging | once per outage (edge-triggered) |
| `late` | a check passes `interval` but is still within `grace` | once per lateness |
| `failed` | the job itself reports failure (`lastbeat fail`, `/ping/<name>/fail`) | once per report |
| `recovered` | a down check pings successfully again | once per recovery |

A check that has **never pinged** stays `waiting` and never alerts: there
is no baseline to be late against until the first heartbeat arrives.

## The TOML subset

The built-in parser accepts the subset that configuration files need:
comments, bare and quoted keys, basic (`"..."` with `\n \t \" \\ \uXXXX`
escapes) and literal (`'...'`) strings, integers, booleans, arrays
(including multi-line), `[tables]` and `[[arrays of tables]]` with dotted
header names. Floats, dates, inline tables, dotted keys in assignments and
nested arrays are **rejected with an explicit error**, never misread. All
syntax errors carry line numbers.

## State file

Everything lastbeat remembers lives in one JSON document
(`schema_version: 1`): per-check status, last ping time, ping/fail
counters, and the last note. Writes are atomic (temp file + rename), so a
crash mid-write cannot corrupt it. Delete the file to reset all history —
checks simply return to `waiting`.
