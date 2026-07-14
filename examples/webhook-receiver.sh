#!/usr/bin/env bash
# webhook-receiver.sh — a zero-dependency local webhook sink for testing
# lastbeat alerts. Listens on 127.0.0.1:<port> (default 9090), prints
# each delivered payload, and answers 200 OK. Ctrl-C to stop.
#
# Usage:
#   bash examples/webhook-receiver.sh [port]
#
# Then point an alert at it in lastbeat.toml:
#   [[alert]]
#   name = "local"
#   url = "http://127.0.0.1:9090/hooks/lastbeat"
set -euo pipefail

PORT="${1:-9090}"

if ! command -v nc >/dev/null 2>&1; then
  echo "this example needs nc (netcat)" >&2
  exit 1
fi

echo "listening on http://127.0.0.1:${PORT} — deliver alerts, Ctrl-C to stop"
while true; do
  # Read one request, echo the body (the JSON payload is the last line
  # after the blank header separator), and respond 200.
  printf 'HTTP/1.1 200 OK\r\nContent-Length: 3\r\nConnection: close\r\n\r\nok\n' \
    | nc -l 127.0.0.1 "$PORT" \
    | awk 'body { print; next } /^\r?$/ { body = 1 }'
done
