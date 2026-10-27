#!/usr/bin/env bash
# Calibration batch: N episodes, sequential. Requires config.toml.
set -euo pipefail
cd "$(dirname "$0")"
N="${1:-100}"
START="${2:-1}"
mkdir -p runtime/reports runtime/logs
for ((i=START; i<START+N; i++)); do
  echo "== episode $i"
  python3 episode.py --seed "$i" 2>&1 | tail -1 | tee -a runtime/logs/batch.log
done
echo "== summary:"
python3 - <<'PY'
import sqlite3, json
db = sqlite3.connect("runtime/ledger.sqlite")
print("outcomes:", dict(db.execute(
    "SELECT outcome, COUNT(*) FROM episodes GROUP BY outcome").fetchall()))
print("findings:", db.execute(
    "SELECT COUNT(*) FROM findings").fetchone()[0])
PY
