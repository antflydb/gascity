# Split-storage migration plan

Root: ga-c6sekn4. Priority: P0. Public issue: [#7727](https://github.com/gastownhall/gascity/issues/7727).
Architecture: ga-e61klgk, sections 1 and 3, accepted ruling snapshot SHA256 `456bf761050dc2824e1eb69a80163ceece1d4a833524b0dc77591242ac98d0ab`. The architect records decisions in beads; this committed PM plan carries their delivery scope.

An existing city with multiple rig stores must migrate through documented gc commands. Closed records and suspended rigs remain part of the data-preservation obligation. The current hand-move remedy has no command behind it.

## Accepted behavior

Extend `gc storage migrate --from-work --fleet-stopped`. One source enumerator supplies the city store and every bound rig store to preflight, copy, proof, clear and operator-invoked detection/recovery. Include suspended rigs. Boot deliberately retains a city-only extent. One disposition rule ignores work, leaves graph/graft rows in their rig ledger, absorbs equal copies and carries messaging, sessions, orders and nudges into the binding with IDs preserved. Divergent same IDs and unreadable rigs fail before writes with a supported remedy.

Bidirectional row/edge proof precedes marker and clear. Preserve all data, timestamps, bits and dependency payloads. Retained graph rows blocked by carried wait gates remain blocked until those gates close. Unsupported edge shapes refuse by name; moving graph is excluded. Backup provenance records every source copy; pending clear tracks store plus ID. V1 backups remain readable, while rig provenance writes v2 so old binaries refuse before moving rows. Completed reruns are no-ops, pre-marker rollback leaves source stores unchanged, and post-clear backup recovery targets the binding.

## Scopes and dependencies

Each scope is one needs-tests bead: validator authors RED and hands that same bead to builder. Each child contains self-contained Build and acceptance criteria; there is no separate implementation successor.

| Scope | Bead | Prerequisite |
|---|---|---|
| X1 | ga-c6sekn4.2 | none; lands as a standalone test-support PR |
| C1 | ga-c6sekn4.3 | ga-zumsvdr (actual fixture main landing) |
| C2 | ga-c6sekn4.4 | ga-c6sekn4.3 plus fixture landing |
| C3 | ga-c6sekn4.5 | ga-c6sekn4.4 plus fixture landing |

The shared fixture F contains three rigs (one suspended), both tiers, every class/open/closed state, exact timestamps and metadata, duplicate/divergent IDs, retained/cross-store edges and wait payloads. It lands independently and is reused by the ready-query companion. Condition `ga-zumsvdr` closes only after exact main commit/PR and meaningful fixture verification; a blocked artifact or build/review close does not release it. It is a prerequisite already in the base, not a migration cherry-pick.

C1–C3 are one coupled feature. Allowing rig rows before widening copy/proof/clear and detectors would strand data, so their reviewed commits ship in one migration rollup. The placeholder ship carrier is recorded on root/members after this graph step. It stays unarmed until explicit resolvable SHAs and all independent review PASS records exist. Member reviewers add review blockers to that same carrier; no member opens an individual production PR or deploy bead. The final member reviewer coordinates exact SHA population and arming, followed by the complete consolidated fresh gate, exact-head CI and MPR.

## Acceptance evidence

Use ruling T-M1–T-M18. Prove real mail/nudge/session/order reachability and unchanged order cooldown; graph locality and readiness before/after; carried-gate closure releasing retained work; no lost dependency payloads; duplicate and divergent-ID behavior; unreadable sources and pending-source recovery. Inject stops across copy/proof/marker/manifest/clear and prove every original row remains in binding, backup or source before deleting any copy. Pin existing controller/fleet/served/destination/edge-payload guards and every remedy against the real command tree. Measure the approximately 10k-row/seven-store case and ratchet from evidence under TESTING.md.

Builders run the applicable Bazel unit, acceptance and integration targets, make check, docs/generated checks and bazel-sync when files/imports change. Each member is independently reviewed and the final assembled rollup gets the full current release gate. PM writes no implementation or tests.

## Operational ordering

Production changes for the two features can be built independently after the shared fixture. USE is ordered: ready selectors land → authorized deployment to controller and every agent PATH → unsplit local/API query parity → pack conversion and observed dispatch → operator cutover. Ready companion: [#8361](https://github.com/gastownhall/gascity/issues/8361), root ga-zspenhc. Its durable cross-store conditions and HQ pack scope gate the operation through actual main landing and deployment, never handoff close.

This plan authorizes no fleet stop, config/database/schema edit, migration, gc installation, deployment or restart. Existing deploy-point and reaper-override requirements apply to a later operator-authorized rollout. Planning completion is not implementation shipment.

## Verified graph

```text
📊 Dependency graph for ga-c6sekn4 (14 issues, 5 layers)

  Status: ○ open  ◐ in_progress  ● blocked  ✓ closed  ❄ deferred

  LAYER 0 (ready)
  ├── ○ ga-95db0z7 P2 sling-ga-c6sekn4.2
  ├── ◐ ga-c6sekn4 P0 Split storage: gc storage migrate carries rig-sco…
  │   ├── ○ ga-c6sekn4.2 P0 As an operator, compare split-storage work and da…
  │   ├── ○ ga-c6sekn4.3 P0 As an operator, preflight every migration source …
  │   ├── ○ ga-c6sekn4.4 P0 As an operator, carry and clear rig infrastructur…
  │   └── ○ ga-c6sekn4.5 P0 As an operator, detect and recover rig strands th…
  ├── ○ ga-c6sekn4.2 P0 As an operator, compare split-storage work and da…
  ├── ○ ga-e61klgk P0 Decide supported split-storage migration and labe…
  ├── ○ ga-lzc1pe3 P2 sling-ga-zspenhc
  ├── ○ ga-s3n8v44 P2 sling-ga-e61klgk
  ├── ○ ga-wfaafub P2 sling-ga-c6sekn4.3
  ├── ○ ga-zls14gy P2 sling-ga-c6sekn4
  ├── ○ ga-zr1woxd P2 sling-ga-c6sekn4.4
  └── ○ ga-zspenhc P0 Split storage: label-filtered ready queries (cust…

  LAYER 1
  └── ○ ga-zumsvdr P0 Shared split-storage fixture is actually landed o…

  LAYER 2
  └── ○ ga-c6sekn4.3 P0 As an operator, preflight every migration source …

  LAYER 3
  └── ○ ga-c6sekn4.4 P0 As an operator, carry and clear rig infrastructur…

  LAYER 4
  └── ○ ga-c6sekn4.5 P0 As an operator, detect and recover rig strands th…
```

Rollup carrier: `ga-si8uwhb` (inert). It depends on all production members, the standalone fixture and actual-main condition; only the C1–C3 reviewed commits are cherry-picked.
