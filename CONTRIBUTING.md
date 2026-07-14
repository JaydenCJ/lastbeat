# Contributing to lastbeat

Issues, discussions and pull requests are all welcome.

## Getting started

You need Go ≥1.22; nothing else. Tests and the smoke script are fully
offline (loopback only).

```bash
git clone https://github.com/JaydenCJ/lastbeat && cd lastbeat
go build ./...
go test ./...
bash scripts/smoke.sh
```

`scripts/smoke.sh` builds the binary, walks a check through
up → down → recovered with a frozen clock (`LASTBEAT_NOW`), asserts a real
command alert fired, and exercises serve mode over 127.0.0.1; it must
finish by printing `SMOKE OK`.

## Before you open a pull request

1. `gofmt -l .` reports nothing (formatting is enforced).
2. `go vet ./...` passes with no findings.
3. `go test ./...` passes (91 deterministic tests, no network).
4. `bash scripts/smoke.sh` prints `SMOKE OK`.
5. Add tests for behavior changes; keep logic in pure, unit-testable
   modules (the monitor state machine never touches the clock, disk or
   network — time is always an explicit argument).

## Ground rules

- Keep dependencies at zero (standard library only); adding one needs
  strong justification in the PR.
- No telemetry, ever. The only outbound traffic lastbeat may produce is
  the webhooks the user configured.
- The state file schema and the alert payload schema are versioned and
  append-only within a major version; breaking either needs a
  `schema_version` bump and a CHANGELOG entry.
- Status transitions are edge-triggered by design: an alert fires when a
  check changes state, never repeatedly for the same outage.
- Code comments and doc comments are written in English.
- Determinism first: identical config + state + `LASTBEAT_NOW` must
  produce byte-identical output, including all orderings.

## Reporting bugs

Include the output of `lastbeat version`, your `lastbeat.toml` (redact
webhook URLs), the relevant slice of the state file, and — for missed or
spurious alerts — the timestamps involved, since the monitor's behavior
is a pure function of config, state and time.

## Security

Please do not open public issues for security problems; use GitHub's
private vulnerability reporting on this repository instead.
