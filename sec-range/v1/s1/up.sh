#!/usr/bin/env bash
# S1 tenancy harness: real manifold serve (fs backend, single process) with
# synthetic orgs. design: gascity adversarial-range.md §4 S1.
# usage: up.sh [--seed N] | down.sh
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
SEC_RANGE_HOME="${SEC_RANGE_HOME:-$(cd "$HERE/../.." && pwd)}"
MANIFOLD_BIN="${MANIFOLD_BIN:-/data/projects/manifold/bazel-bin/crates/manifold-server/manifold}"
PORT="${S1_PORT:-18890}"
RUNTIME="$SEC_RANGE_HOME/v1/s1/runtime"
PIDFILE="$RUNTIME/serve.pid"

cmd_up() {
  SEED=1
  while [ $# -gt 0 ]; do
    case "$1" in
      --seed) SEED="${2:?}"; shift 2 ;;
      *) shift ;;
    esac
  done
  [ -x "$MANIFOLD_BIN" ] || { echo "error: manifold binary missing at $MANIFOLD_BIN (build or set MANIFOLD_BIN)" >&2; exit 2; }
  if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "error: S1 serve already running (pid $(cat "$PIDFILE"))" >&2; exit 1
  fi
  if ss -tln 2>/dev/null | grep -q ":$PORT "; then
    echo "error: port $PORT already bound" >&2; exit 1
  fi
  mkdir -p "$RUNTIME/creds"
  python3 "$HERE/seed_gen.py" --seed "$SEED" \
    --out "$RUNTIME/seed.toml" --tokens-out "$RUNTIME/tokens.json"

  MANIFOLD_CREDS_BACKEND=fs \
  MANIFOLD_ALLOW_FS_CREDS_BACKEND=1 \
  MANIFOLD_SEED="$RUNTIME/seed.toml" \
  MANIFOLD_ADMIN_TOKEN="adm_range_s1_canary" \
  MANIFOLD_CREDS_ROOT="$RUNTIME/creds" \
  MANIFOLD_CURSOR_BROKER_SIGNING_SECRET="range-s1-canary-signing-secret-00000000000000" \
  MANIFOLD_ALLOW_LEGACY_KEYS=1 \
    nohup "$MANIFOLD_BIN" serve --host 127.0.0.1 --port "$PORT" \
      >"$RUNTIME/serve.log" 2>&1 &
  echo $! > "$PIDFILE"

  # wait for readiness (bounded)
  for _ in $(seq 1 60); do
    if curl -fsS "http://127.0.0.1:$PORT/readyz" >/dev/null 2>&1; then break; fi
    if ! kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
      echo "error: serve exited during startup:" >&2; tail -20 "$RUNTIME/serve.log" >&2; exit 1
    fi
    sleep 0.5
  done
  curl -fsS "http://127.0.0.1:$PORT/readyz" >/dev/null || { echo "error: serve never became ready" >&2; tail -20 "$RUNTIME/serve.log" >&2; exit 1; }
  smoke
  echo "S1 up: seed=$SEED port=$PORT pid=$(cat "$PIDFILE") tokens=$RUNTIME/tokens.json"
}

smoke() {
  local ok=0
  # admin state reachable with the admin token
  curl -fsS -H "x-manifold-admin: adm_range_s1_canary" \
    "http://127.0.0.1:$PORT/admin/api/state" >/dev/null || { echo "smoke FAIL: admin state"; ok=1; }
  # tenant key of org A is refused on admin (tenant material must never admin)
  local tokA
  tokA=$(python3 -c "import json;print(json.load(open('$RUNTIME/tokens.json'))['orgs']['org_victim_a']['token'])")
  if curl -fsS -H "x-manifold-admin: $tokA" \
       "http://127.0.0.1:$PORT/admin/api/state" >/dev/null 2>&1; then
    echo "smoke FAIL: tenant token accepted on admin API"; ok=1
  fi
  [ "$ok" = 0 ] && echo "smoke: OK (admin ok; tenant-refused-admin ok)" || exit 1
}

cmd_down() {
  if [ -f "$PIDFILE" ]; then
    local pid; pid="$(cat "$PIDFILE")"
    if kill -0 "$pid" 2>/dev/null; then kill "$pid"; sleep 1; kill -9 "$pid" 2>/dev/null || true; fi
    rm -f "$PIDFILE"
  fi
  # bounded wait for the listener to actually release (serve drains on shutdown)
  for _ in $(seq 1 30); do
    ss -tln 2>/dev/null | grep -q ":$PORT " || break
    sleep 0.5
  done
  # verify: no listener on the serve port, workdir secrets removed
  if ss -tln 2>/dev/null | grep -q ":$PORT "; then
    echo "error: port $PORT still bound after teardown" >&2; exit 1
  fi
  rm -rf "$RUNTIME/creds" "$RUNTIME/seed.toml" "$RUNTIME/tokens.json"
  echo "S1 down: process stopped, port free, seed material removed"
}

case "${1:-}" in
  up) shift; cmd_up "$@" ;;
  down) cmd_down ;;
  *) echo "usage: $0 up [--seed N] | down" >&2; exit 2 ;;
esac
