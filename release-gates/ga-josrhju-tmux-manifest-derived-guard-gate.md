# Release gate: derive tmux inventory expectations

**Verdict:** **PASS**

Final audit: all four uncached remote phases and all 16 required native floor steps completed. The original failures remain recorded and are attributed under the standing four-clause rule below. No waiver or failed-test retry.

- Deploy bead: ga-josrhju; build bead: ga-hnat6x; review bead: ga-trald65.
- Reviewed source: `7f6b8cda6934b29918957c9f13196210ef27835a` (resolved against this repository).
- DEPLOY_MODE: remote; BASE_REF: origin/main; PUSH_REMOTE: resolved again at publication; the earlier origin dry-run exited 1.
- Pinned current main: `f3d85ea3d1f639009d8107777bdca2928f1986a8`.
- merge_state: conflicts (origin/main@f3d85ea3d1f6; files: scripts/AGENTS.md, scripts/runtime_tmux_manifest_test.go).
- gate_base_kind: own-base; own base: `7c12d13bd0aef20a9ddefea5e77eee22228c76cf`.
- Canonical scratch: `/var/tmp/gc-merge-validate.dxMJrlA1`; scratch commit: `1426de8fcb61c6236f92dbd258defe52752a1d92`.
- Scratch tree equals the reviewed source tree exactly: `d289d4e451c2a3517e7a7926402d6db31509ed1f`.
- merge_validation: deferred to merge time — gascity main merge queue tests the merged result; MPR must first arrange conflict resolution. MPR's queue skips a PR born CONFLICTING (deployer pack docs/merge-time-guarantee.md, Gap 1). The deployer did not rebase.
- Preflight: reviewed commit's GitHub commits/pulls lookup returned [] both before launch and at recovery.

## Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-trald65 independently reviewed the exact source; no carryover or reviewer-added-source adjustment. |
| 2 | Acceptance criteria | PASS | Source and fixture tests, migration recount, independent three mutations and disposable merge experiments below; PR-body requirements prepared in the draft. |
| 3 | Full-scope tests | PASS | Four remote phases plus native floor complete: 109004 PASS, 3 FAIL, 335 SKIP. Three failed lines represent two unrelated conditions, attributed below; no raw verdict overwritten. |
| 4 | No unresolved high-severity findings | PASS | Reviewer PASS; no unresolved HIGH/blocking findings. |
| 5 | Final branch clean | PASS | Scratch clean; final deploy branch contains only the reviewed source and this committed gate record. Cleanliness verified before push. |
| 6 | Merge state recorded; build/vet | PASS | Whole-tree Bazel build //... and make vet/nogo passed on the own-base tree. Live current-main conflicts recorded above. |
| 7 | Single feature theme | PASS | Seven scripts paths: tmux inventory, lane-record guard and adjacent instructions/build inputs; executable shard runners, manifest names/order and CODEOWNERS unchanged. |

## Acceptance evidence

| Cases | Independent evidence |
|-------|----------------------|
| AC1 | TestRuntimeTmuxGuardAddingATestCostsOnlyManifestLines PASS: add untagged and recorded integration-file tests without count edits. |
| AC2 | TestRuntimeTmuxLaneDriftDiagnostics PASS: missing/lost/absent/empty integration record and !integration drift diagnostics; allowed tagged-empty and default XTest fixtures. |
| AC3 | Existing canonical discovery, TestMain distinction and drift checks PASS in the scripts unit output; runtimeTmuxManifestDrift unchanged. |
| AC4 | Header-only manifest mutation fails the real guard with “runtime-tmux manifest contains no tests” (Bazel exit3). |
| AC5 | Controlled grep 710/541/169/119/118 over the two guard files: four matching base lines, no reviewed-source matches (exit1). The bead's earlier count literals are stale. |
| AC6 | Real runner checked for every shard1..6. Shift stripe ==shard_index-1 to ==shard_index: all six shard subtests FAIL. Drop TestPokeActivityRealTmux from shard1: “selected by 0 shards, want exactly one”; Bazel exit3 each. Redundant partition test removed. |
| AC7 | Lane parser and real-tree match PASS; rejects unsorted/duplicate/unsafe/not-_test.go/surrounding-space entries. |
| AC8 | Manifest header, actionable failure messages, scripts/AGENTS.md note present. |
| AC9 | Disposable A/B untagged additions and C/D integration-file additions each pass alone; merge-tree A+B, A+C,C+D exit0 and merged-tree guards PASS. Same-gap A+E exits1 as expected. Before-control A+B also merges exit0, but its old guard FAILS “contains712tests,want711”; observed outcome corrects the AC's claimed universal textual conflict. |
| AC10 | Recount:17 recorded integration-only files,169 integration-only tests,541 default-lane tests,710 total; record equals source tree. Full make check plus all three other CI tiers executed fresh; native floor complete and attributions independently checked. |

Mutation probes are supplemental AC evidence, never a substitute for the full suite. Each modified scratch view was restored byte-for-byte and removed. Raw commands/exit codes retained in mutation-proof.json and merge-proof.json under the evidence root.

## Full-scope execution

- test_cmd_scope: full-suite.
- test_cmd: gate-criterion3.sh on the exact own-base scratch. Unit=make check; acceptance=bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; integration-packages=bazel test --config=integration //test:integration_packages; full integration=bazel test --config=integration //test/integration:integration_test (no smoke filter).
- Each phase: --config=remote-exec --config=ci --keep_going --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1; full integration also --test_arg=-test.v. Private retained BEP per phase.
- test_exec_mode: remote-exec (derived from farm BEP; native floor recorded separately).
- test_exec_bep: unit365,acceptance24,integration-packages77,full-integration14; all480 remote, no cached locally/remotely, no retries, attempt1, starts inside each phase window and terminal result for every attempt. All960 retained raw log/XML digests and derived normalized logs reverified at recovery.
- farm_test_seconds: 7459.363.
- rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0", "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"}.
- fallback_reason: none; no stitched farm/local fallback.
- admission: remote floor25GiB/cap2; suite {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 76.38873672485352, "waited_seconds": 0.001}. Native floor retains its independent admission record.
- heavy_mode: none (classify returned none).
- policy_lane: PASS — full-tree nogo/vet, required shell guards, nine fresh remote policy/format targets; format PASS. Runs in fresh views before the suite.
- drift_lane: PASS — make bazel-sync then git diff --exit-code and empty tracked/untracked status, in a separate fresh view.
- ci_lane_run: n/a (no CI job/matrix/timeout/required-check change).
- test_log_dir: /var/tmp/deploy-ga-josrhju.cxup46z1/c3-run/evidence/remote/logs (retained, never deleted).
- test_counts (raw Go top-level result lines, per-job sum before native floor):108983PASS,3FAIL,321SKIP. These are not unique tests or target verdicts.
- Target summaries: unit280PASSED;acceptance7PASSED;integration-packages28PASSED/1FAILED;full-integration1FAILED (13/14 attempts PASS). Both failed phase exits and all failed result events remain immutable.
- owned_topup: not needed (all seven tests PASS within full unit output).
- floor_topup: complete=true, 16/16 steps exit 0. All environment-absence skips selected by package, parent for subtests, plus beadstest module check, Darwin compile, and both runner self-tests. Native logs remain in c3-run/evidence/remote/floor-logs.
- skip_inventory: final 357 events including subtests (100 environment-absence, 257 other), 246 unique sites; all UNCHANGED with shared_hunks_in_file=0. Per-event reasons, source sites and safety arguments: not-owned-skip-evidence.json; raw tool output: skip-sites-evidence.txt. Original 321-event farm-only snapshot remains in skip-review.json. No owned test skipped.
- waiver_ref: none; gascity has no waiver path. Attribution below follows the standing four-clause protocol.

Diff-owned results (gate-test-evidence.py by name):

- TestRuntimeTmuxGuardAddingATestCostsOnlyManifestLines: PASS in full unit output.
- TestRuntimeTmuxIntegrationShardUsesCheckedManifestOnLinux: PASS in full unit output.
- TestRuntimeTmuxLaneDriftDiagnostics: PASS in full unit output.
- TestRuntimeTmuxLaneRecordMatchesTree: PASS in full unit output.
- TestRuntimeTmuxLaneRecordParser: PASS in full unit output.
- TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory: PASS in full unit output.
- TestRuntimeTmuxPerFileDiscoveryOrdersFilesThenTests: PASS in full unit output.

## Failure attribution (original failures preserved)

1. TestAPIContractSuite/agents and parent TestAPIContractSuite -> ga-1fv057s. Agent-template resume HTTP200 but suspended=true remains visible at its deadline. Clause1: official failure site UNCHANGED, zero shared hunks. Clause3(a): computed tagged+untagged launched gc binary import closures (166 module directories), no changed non-test input reachable. Clause4: cmd/gc vs scripts, no package overlap. Changed Go files are scripts tests; executable shard scripts/order unchanged; no new target/census bump. Tracker created during this run, accepted only through gm-sf3238 with this landed mechanism proof, not as evidence of pre-existence. failure-mechanism.json retains the exact proof.
2. TestGCLiveContract_BeadsAndEvents -> ga-zcs86v6. Exact previously tracked generic POST/beads order-run fixture422 instead of201; existing tracker and comments opened, sighting appended. Clause1: official test/integration/gc_live_contract_test.go:277 UNCHANGED, zero shared hunks. Clause3(a): integration test closure61 plus launched gc binary closure166 reach no changed non-test input; executable runner scripts unchanged. Clause4: test/integration vs scripts, no package overlap. First hit for this deploy. The remedy is on current main, but this conflicting reviewed branch is gated on its own older base; no claim of regression on the fixed main tree. live-contract-mechanism.json retains the exact proof.

These are outside-diff attributions, never converted PASS test events. No failed test was retried. The native checks and final SKIP review have now finished; every step exit is 0 and every site is proven unchanged. Criterion3 PASS uses these two explicit condition attributions, never a converted test result.

## Retained evidence

Evidence root: `/var/tmp/deploy-ga-josrhju.cxup46z1`. recovery-verification.json retains BEP predicates and960hash checks; original execution.json remains criterion3_valid=false because the first command stopped on its original failure. Any final gate verdict is a separately audited four-clause decision with native floor evidence; the original record is never overwritten.

Builder disclosure: the scripts/AGENTS.md documentation commit used a mayor-authorized pre-commit bypass for missing ICU74 on this host. The independent gate ran docsync and resourcecensus on RBE successfully. Final gate-only commit must run the active pre-commit hook.

## Final retained-reader audit

test_counts: PASS=109004 FAIL=3 SKIP=335
diff_tests_executed:
  TestRuntimeTmuxGuardAddingATestCostsOnlyManifestLines PASS (unit-b0c13e82fd961ff4f637.log)
  TestRuntimeTmuxIntegrationShardUsesCheckedManifestOnLinux PASS (unit-9f907e0fea7ca680f21c.log)
  TestRuntimeTmuxLaneDriftDiagnostics PASS (unit-9f907e0fea7ca680f21c.log)
  TestRuntimeTmuxLaneRecordMatchesTree PASS (unit-ee115c844954da53d34d.log)
  TestRuntimeTmuxLaneRecordParser PASS (unit-81d8a9af827e251f6b12.log)
  TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory PASS (unit-db0feb4ad70da6248282.log)
  TestRuntimeTmuxPerFileDiscoveryOrdersFilesThenTests PASS (unit-271bc77ad6715453dfe1.log)
test_log_dir: /var/tmp/deploy-ga-josrhju.cxup46z1/c3-run/evidence/remote/logs

Native floor load, copied from retained output:
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=44.70 run_max_load=74.32 run_mean_load=59.84 run_readings=11 wait_first_load=23.41 wait_max_load=44.70 wait_mean_load=28.38 wait_readings=61 read_errors=0

The ordinary bounded wait expired before proceeding; every floor step exited 0. Remote suite admission has no local load wait. 357 events include skipped subtests, whereas 335 is the top-level per-job total. All seven owned tests report PASS; no skip event is owned. Original execution.json and all phase failures remain unchanged. All 960 retained raw log/XML SHA-256 digests were checked again at final audit. No evidence root was deleted.
