#!/usr/bin/env bash
# End-to-end smoke test for lastbeat: builds the binary, walks a check
# through up -> down -> recovered with a deterministic clock, verifies a
# real command alert fires, then exercises serve mode over loopback HTTP.
# No external network, idempotent, finishes in seconds.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

fail() {
  echo "SMOKE FAIL: $*" >&2
  exit 1
}

BIN="$WORKDIR/lastbeat"
CFG="$WORKDIR/lastbeat.toml"
ALERTS="$WORKDIR/alerts.log"
PORT=18942

echo "1. build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/lastbeat) || fail "go build failed"

# Note: tool output is captured into variables before grepping — piping
# straight into `grep -q` can SIGPIPE the writer and trip pipefail.
echo "2. version matches manifest"
[ "$("$BIN" --version)" = "lastbeat 0.1.0" ] || fail "--version mismatch"

echo "3. init writes a loadable starter config"
"$BIN" init "$WORKDIR/starter.toml" >/dev/null || fail "init failed"
OUT="$("$BIN" -c "$WORKDIR/starter.toml" checks)"
echo "$OUT" | grep -q "nightly-backup" || fail "starter config does not load"

echo "4. write the test configuration"
cat > "$CFG" <<EOF
listen = "127.0.0.1:$PORT"
state_file = "lastbeat.state.json"
sweep_every = "1s"

[[check]]
name = "backup"
interval = "1h"
grace = "10m"

[[alert]]
name = "logfile"
command = ["/bin/sh", "-c", "cat >> $ALERTS"]
events = ["down", "recovered"]
EOF

echo "5. ping records a heartbeat (frozen clock)"
export LASTBEAT_NOW="2026-07-13T03:00:00Z"
[ "$("$BIN" -c "$CFG" ping backup)" = "backup: ok" ] || fail "ping failed"
OUT="$("$BIN" -c "$CFG" status)"
echo "$OUT" | grep -q "backup.*up" || fail "status should show up"

echo "6. sweep two hours later declares it down and fires the alert"
export LASTBEAT_NOW="2026-07-13T05:00:00Z"
OUT="$("$BIN" -c "$CFG" sweep)"
echo "$OUT" | grep -q "backup is down.*overdue by 1h" \
  || fail "sweep did not report down"
grep -q '"event":"down"' "$ALERTS" || fail "down alert payload missing"
grep -q '"check":"backup"' "$ALERTS" || fail "alert payload lacks check name"

echo "7. status --fail-on-down exits 1 while down"
if "$BIN" -c "$CFG" status --fail-on-down >/dev/null; then
  fail "--fail-on-down should exit 1"
fi
OUT="$("$BIN" -c "$CFG" status --format json)"
echo "$OUT" | grep -q '"down": 1' || fail "JSON status should count one down check"

echo "8. the next ping recovers and alerts again"
export LASTBEAT_NOW="2026-07-13T05:30:00Z"
OUT="$("$BIN" -c "$CFG" ping backup)"
echo "$OUT" | grep -q "recovered" || fail "recovery not reported"
grep -q '"event":"recovered"' "$ALERTS" || fail "recovered alert missing"
"$BIN" -c "$CFG" status --fail-on-down >/dev/null || fail "status should be clean again"

echo "9. serve mode answers pings over loopback HTTP"
unset LASTBEAT_NOW
# exec so $SERVER_PID is the server itself, not a wrapping subshell.
(cd "$WORKDIR" && exec "$BIN" -c "$CFG" serve >/dev/null 2>&1) &
SERVER_PID=$!
for _ in $(seq 1 50); do
  curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
[ "$(curl -sf "http://127.0.0.1:$PORT/healthz")" = "ok" ] || fail "healthz not answering"
[ "$(curl -sf "http://127.0.0.1:$PORT/ping/backup")" = "ok" ] || fail "HTTP ping failed"
OUT="$(curl -sf "http://127.0.0.1:$PORT/status/backup")"
echo "$OUT" | grep -q '"status": "up"' || fail "HTTP status wrong"
if curl -sf "http://127.0.0.1:$PORT/ping/no-such-check" >/dev/null 2>&1; then
  fail "unknown check should 404"
fi
kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true
SERVER_PID=""

echo "10. usage errors exit 2"
set +e
"$BIN" -c "$CFG" status --format yaml >/dev/null 2>&1
[ $? -eq 2 ] || fail "bad --format should exit 2"
"$BIN" frobnicate >/dev/null 2>&1
[ $? -eq 2 ] || fail "unknown command should exit 2"
set -e

echo "SMOKE OK"
