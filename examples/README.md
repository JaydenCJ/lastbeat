# lastbeat examples

Two runnable examples, both offline and self-contained.

## crontab.example

A realistic crontab wiring three jobs to lastbeat, in both styles:

- **serve mode** — jobs `curl` the loopback listener after each run, and
  the long-running `lastbeat serve` process detects silence;
- **daemonless mode** — jobs call `lastbeat ping` directly and a fifth
  cron line runs `lastbeat sweep` every few minutes, so no long-lived
  process is needed at all.

```bash
cat examples/crontab.example
```

## webhook-receiver.sh

A dependency-free webhook receiver for local experiments: it listens on
127.0.0.1 with `nc`, prints every alert payload lastbeat delivers, and
answers `200 OK`. Point an `[[alert]]` `url` at it to watch payloads live:

```bash
bash examples/webhook-receiver.sh 9090 &
lastbeat -c lastbeat.toml sweep
```

For quick tests without any receiver, a `command` alert works anywhere:

```toml
[[alert]]
name = "append-log"
command = ["/bin/sh", "-c", "cat >> /var/log/lastbeat-alerts.log"]
```

Every payload is a single line of versioned JSON, so `jq`, `grep` and
`tail -f` are all you need to build on top of it.
