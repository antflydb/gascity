# sec-range v0 — S3 untrusted-content ingestion (prompt injection)

Continuous adversarial emulation, phase v0. See `docs/design/adversarial-range.md`
for the full design. This directory is the runnable v0: the Injector persona
attacking a victim coding agent through a fake issue tracker, with a sinkhole
recorder, an adjudicator, and a local ledger.

## Safety rails (read before running)

- Every network endpoint the victim agent can reach is loopback (recorder,
  fake tracker). The victim's `run_command` tool executes under
  `bwrap --unshare-net` (no network at all from commands; that surface is
  S2's job, not v0's).
- The victim agent's key and `.env.canary` are canary fakes. Nothing real.
- Red and victim models are reached through the Manifold gateway with a
  range-scoped key; spend is metered like any other lane.
- Kill switch: `pkill -f sec-range` and delete the runtime dir.

## Layout

- `episode.py` — one episode, end to end
- `recorder.py` — sinkhole recorder + fake issue tracker (one process, two ports)
- `victim_agent.py` — the target agent (tools, transcript shapes, canaries)
- `red_injector.py` — attacker persona loop (Injector)
- `adjudicator.py` — outcome classification + technique tagging (opus tier)
- `ledger.py` — sqlite ledger (episodes / attempts / findings)
- `prompts/` — red, victim system, adjudicator prompts
- `config.toml` — endpoints, models, budgets
- `run_calibration.sh` — batch of episodes

## Run

    cp config.example.toml config.toml   # edit models/key path
    python3 recorder.py &                # ports 127.0.0.1:8899 (tracker), :8898 (sink)
    python3 episode.py --seed 1          # one episode
    ./run_calibration.sh 100             # calibration batch

Ledger lands in `runtime/ledger.sqlite`; reports in `runtime/reports/`.
