# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-13

### Added

- Heartbeat monitoring engine with edge-triggered status transitions
  (`waiting → up → late → down → recovered`): alerts fire exactly once per
  outage, sweeps are idempotent, and never-pinged checks stay silent until
  their first heartbeat.
- TOML configuration (`lastbeat.toml`) with per-check `interval`, `grace`,
  `tags` and alert routing, a `[defaults]` table, strict unknown-key
  detection, and line-numbered error messages — parsed by a built-in,
  dependency-free TOML subset parser.
- Human schedule durations with day and week units (`"90s"`, `"1h30m"`,
  `"1d"`, `"2w"`), validated at config load.
- Single-file JSON state (`schema_version: 1`) with atomic temp-file +
  rename writes, corruption and future-schema detection, and pruning of
  removed checks.
- `serve` mode: loopback-bound HTTP listener with `GET|POST /ping/<name>`,
  `/ping/<name>/fail` (explicit failure reports), `/status`,
  `/status/<name>` and `/healthz`, optional shared `ping_key` (header or
  query), request-body notes, and a periodic background sweep.
- Daemonless mode: `lastbeat ping` / `fail` / `sweep` write the state file
  directly, so a cron entry can run the whole monitor without any
  long-lived process.
- Webhook alerts (versioned JSON POST with overdue seconds, previous
  status and tags) and local command alerts (payload on stdin, `{check}` /
  `{event}` / `{status}` / `{note}` placeholders in argv), with per-alert
  event subscriptions and timeouts.
- `status` subcommand with an aligned text table or `--format json`, plus
  `--fail-on-down` (exit 1) for scripting; `checks` lists configured
  schedules; `init` writes a working starter config.
- `LASTBEAT_NOW` frozen-clock override for rehearsing alert pipelines and
  deterministic testing.
- Runnable examples (`examples/crontab.example`, `examples/webhook-receiver.sh`)
  and a configuration reference (`docs/config.md`).
- 91 deterministic offline tests (unit + in-process HTTP and CLI
  integration) and `scripts/smoke.sh`.

[0.1.0]: https://github.com/JaydenCJ/lastbeat/releases/tag/v0.1.0
