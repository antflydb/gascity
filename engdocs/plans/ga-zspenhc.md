# Label-aware ready queries plan

Root: ga-zspenhc. Priority: P0. Public issue: [#8361](https://github.com/gastownhall/gascity/issues/8361).
Architecture: accepted ga-e61klgk sections 2 and 3; ruling snapshot SHA256 `456bf761050dc2824e1eb69a80163ceece1d4a833524b0dc77591242ac98d0ab`. Decisions remain the architect's; this plan supplies delivery scopes and dependencies.

Custom label-routed work queries must keep selecting eligible work through storage splitting. The supported spelling must work on an unsplit city first so operators can verify conversion before cutover. Current gc accepts but ignores rig scope and does not split comma exclusions, so changing the executable name alone can widen results.

## Accepted behavior

Extend gc ready with label AND, label-any OR and explicit rig selection; normalize repeated/comma-separated label and type lists once in the domain reader. Exclusion wins, labels are exact/case-sensitive, invalid include-side empties and quoted comma syntax refuse before any store opens, and metadata-field remains a plain array. Preserve canonical ordering, first-leg ID deduplication and limit-after-filter/merge prefix behavior. The default remains unlimited; mechanical pack conversion makes bd's implicit 100 explicit.

Explicit --rig reads exactly that ledger, including a suspended rig. Unscoped reads retain the serving frame and never infer scope from cwd or rig environment variables. Unknown/storeless/unreadable scopes fail naming the problem; failed legs return nonzero with empty stdout. Rig-scoped equivalence covers retained work/graph rows; relocated classes use their own readers.

The typed API/client adds the same selectors and proves they were applied. An old controller returning unscoped results is discarded in favor of a local read or failure; a suspended store absent from controller memory is served by explicit local fallback. Regenerate wire/spec/clients and keep CLI/API projections over the same domain reader. Custom work_query text is never rewritten by core. Split-city gc bd ready stays refused and gains an argv-derived supported conversion hint.

## Core scopes

Each row is one needs-tests bead: validator writes RED and passes that same bead to builder. The original shared fixture is ga-c6sekn4.2 under the migration root; no duplicate fixture is created.

| Scope | Bead | Prerequisite |
|---|---|---|
| R1 | ga-zspenhc.1 | ga-zumsvdr (actual shared fixture main landing) |
| R3 | ga-zspenhc.2 | ga-zspenhc.1 |
| R2 | ga-zspenhc.3 | ga-zspenhc.1 and ga-zspenhc.2 |
| R4 | ga-zspenhc.4 | ga-zspenhc.3 and ga-zspenhc.2 |

R1 owns grammar/predicates/pushdown/prefix tests. R3 owns typed API/client, skew defense and wire/dashboard gates. R2 owns flags, actual rig scope/fallback and structured refusal mapping. R4 owns advisory preflight lint and the runbook/conversion table, including the current pipeline negative. These interdependent contracts ship together in ONE core-ready rollup; independent member review records and exact SHAs are required before arming its inert ship carrier. No individual member production PR/deploy bead is created. Last member reviewer coordinates exact picks and all review blockers, then the complete fresh consolidated gate, current exact-head CI, clearance and MPR.

The core rollup excludes actual pack conversion and runtime deployment. Making core shipment wait for packs would create a code → deploy → packs → code cycle.

## Pack and operational work

HQ pack scope: `gm-0qegy3`, needs-pack-work, explicitly routed to pack-author.pack-author. Its native blocker is HQ condition `gm-cyic3i`; raw duplicate-ID dependency readback verified both ends in HQ. Foreign ga-* references carry provenance only. Rig condition `ga-21lx6fz` mirrors the same actual-deployment fact. Mayor owns recording and closing both after evidence.

Re-inventory the baseline 29 definitions/24 pack files. S1–S4 use explicit rig scope; preserve S3 jq budget/defer stages and existing caps, writing --limit 100 when omitted before. S5 deliberately uses unscoped gc ready with limit 0 and existing filters. S6 changes per-store 5 to one global cap, so the pack owner records N and eligible-set/cap evidence rather than claiming a purely mechanical conversion. Add a pack-side capability probe requiring --label-any so old executables cannot accept S4/S5 while silently dropping exclusions/scope. Pipeline-exit-policy changes are separate work.

Operator milestone: `ga-a2vew07`, needs-mayor/routed mayor, gated by three rig-local actual-outcome conditions:
- `ga-qn2fdl5`: migration code actually on main, not a closed blocked artifact.
- `ga-21lx6fz`: ready code on main, gc deployed to controller and every agent PATH, controller restarted under authorized deploy-point/reaper rules, new capability and unsplit local/API query parity verified.
- `ga-p0gntwc`: HQ conversion actually landed/deployed and agents observed selecting/claiming work on converted queries while still unsplit.

Sequence: ready core lands → operator-authorized deployment and verification → pack conversion/observed dispatch → operator-authorized cutover after both feature main landings. Core migration and ready development can proceed independently after the fixture.

## Acceptance evidence

Architect T-R1–T-R14: grammar and inclusion matrices; empty/quoted values before store opens; exclusions/metadata/assignee; at least 150 included and 150 higher-ranked excluded rows for limit-prefix N=1/20/100/149 across sorts/legs; unknown/unreadable/suspended/unified scopes; cwd/env independence; local/API/skew parity; actual command/refusal mapping; S1–S6 unsplit-oracle and converted-split equivalence; real hook selection/claim store; advisory lint leaves exit status unchanged; existing swallowed-reader pipeline behavior is documented as a negative rather than fixed here.

Use fixture F and real CLI/bd oracle where required. Builders run appropriate Bazel unit/acceptance/integration, make check, source-generated docs/wire sync and API dashboard/serving gates. All member review PASS records must bind exact commits; the final assembled rollup gets the full current release gate.

This planning scope authorizes no gc install/deploy/restart, fleet stop, pack runtime/config swap, schema/database edit, migration or cutover. Existing operator authorization and deploy-point/reaper requirements apply. Root planning completion is not code shipment, and no condition clears from a build/review handoff close.

## Verified dependency graph

```text
📊 Dependency graph for ga-zspenhc (27 issues, 8 layers)

  Status: ○ open  ◐ in_progress  ● blocked  ✓ closed  ❄ deferred

  LAYER 0 (ready)
  ├── ○ ga-21lx6fz P0 Ready selector support is actually landed, deploy…
  ├── ○ ga-6un7nq2 P2 sling-ga-c6sekn4.5
  ├── ○ ga-71xsg2t P2 sling-ga-zspenhc.2
  ├── ○ ga-95db0z7 P2 sling-ga-c6sekn4.2
  ├── ◐ ga-c6sekn4 P0 Split storage: gc storage migrate carries rig-sco…
  │   ├── ◐ ga-c6sekn4.2 P0 As an operator, compare split-storage work and da…
  │   ├── ○ ga-c6sekn4.3 P0 As an operator, preflight every migration source …
  │   ├── ○ ga-c6sekn4.4 P0 As an operator, carry and clear rig infrastructur…
  │   └── ○ ga-c6sekn4.5 P0 As an operator, detect and recover rig strands th…
  ├── ◐ ga-c6sekn4.2 P0 As an operator, compare split-storage work and da…
  ├── ○ ga-e61klgk P0 Decide supported split-storage migration and labe…
  ├── ○ ga-fnrqwci P2 sling-ga-zspenhc.3
  ├── ○ ga-lzc1pe3 P2 sling-ga-zspenhc
  ├── ○ ga-p0gntwc P0 Converted pack queries are actually deployed and …
  ├── ○ ga-s3n8v44 P2 sling-ga-e61klgk
  ├── ○ ga-v5c9bsi P2 sling-ga-zspenhc.1
  ├── ○ ga-wfaafub P2 sling-ga-c6sekn4.3
  ├── ○ ga-zls14gy P2 sling-ga-c6sekn4
  ├── ○ ga-zr1woxd P2 sling-ga-c6sekn4.4
  └── ◐ ga-zspenhc P0 Split storage: label-filtered ready queries (cust…
      ├── ○ ga-zspenhc.1 P0 As an agent, select ready work with one consisten…
      ├── ○ ga-zspenhc.2 P0 As an API client, receive scoped ready results ev…
      ├── ○ ga-zspenhc.3 P0 As an operator, use label-ready CLI queries with …
      └── ○ ga-zspenhc.4 P0 As an operator, prepare custom work queries and f…

  LAYER 1
  └── ○ ga-zumsvdr P0 Shared split-storage fixture is actually landed o…

  LAYER 2
  ├── ○ ga-c6sekn4.3 P0 As an operator, preflight every migration source …
  └── ○ ga-zspenhc.1 P0 As an agent, select ready work with one consisten…

  LAYER 3
  ├── ○ ga-c6sekn4.4 P0 As an operator, carry and clear rig infrastructur…
  └── ○ ga-zspenhc.2 P0 As an API client, receive scoped ready results ev…

  LAYER 4
  ├── ○ ga-c6sekn4.5 P0 As an operator, detect and recover rig strands th…
  └── ○ ga-zspenhc.3 P0 As an operator, use label-ready CLI queries with …

  LAYER 5
  ├── ○ ga-si8uwhb P0 [ga-c6sekn4] Ship rig-infrastructure migration (r…
  └── ○ ga-zspenhc.4 P0 As an operator, prepare custom work queries and f…

  LAYER 6
  └── ○ ga-qn2fdl5 P0 Rig-infrastructure migration rollup is actually l…

  LAYER 7
  └── ○ ga-a2vew07 P0 Coordinate split-storage cutover only after code,…
```

Core rollup carrier: ga-mr6z8uh (inert). It waits for four independently reviewed core members and the already-landed fixture, and never waits for packs/deployment/cutover.
