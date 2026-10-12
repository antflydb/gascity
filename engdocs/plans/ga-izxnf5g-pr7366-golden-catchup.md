# PR #7366: preserve query behavior through the golden catch-up

PR [#7366](https://github.com/gastownhall/gascity/pull/7366) excludes exact
lower-case `human` work from routed readiness and ordinary worker-pool demand.
It passed its prior release gate, but upstream #7601 changed the generated
workquery scripts and left the PR with 18 conflicting snapshots. The goal is
one merge catch-up that retains both changes and receives fresh clearance
before MPR resumes on 2026-10-12 at 17:00Z.

## Assignment and evidence

| Item | System of record |
|---|---|
| Mayor order | `gm-wisp-nkjp6eq`; reminder `gm-wisp-d79hbkl` |
| PM packaging | `ga-izxnf5g` |
| Single delivery scope | `ga-ik5v7uu`, `delegated-build`, target `gascity/codex` |
| Published PR branch | `quad341:deploy/ga-t34icc-gate` |
| Full current head and publication lease | `ee0a0e9363f07d3853cecc5b4ec7242e5e63939e` |
| Original lineage | `ga-t34icc` |
| Previous release gate | `ga-zr2miu3`, PASS and clearance at the published head |
| Mayor-owned main-landing guard | `ga-5gwr2e` |
| PM intake main | `5a0322d46d3316afcd7b094f24057edfe3f265e6` |

PM verified the live PR is OPEN/DIRTY at the full lease. A read-only
`git merge-tree --write-tree --name-only` against the intake main exits 1
and names exactly 18 golden conflicts, with no conflicting code file.
Its raw output is `/var/tmp/pr7366-pm-merge-tree.txt`. These are intake
coordinates; the builder fetches and pins fresh main again before merging.

The known conflict matrix is `legacy`, `normal`, and `pool`, each with
`RoutedPool_bd104` and `Work_bd104`/`Work_bd105`, with their federated
counterparts, under `internal/config/testdata/workquery/`. Upstream #7601
is commit `515eb2b698`. A fresh conflict outside this generated-fixture
scope requires reporting on the same bead before changing scope.

## Delivery contract

Mayor authorized one Codex build scope during the Claude outage.
`delegated-build` replaces `ready-to-build` so the same work does not enter
both queues. The delivery bead has a `discovered-from` edge to PM packaging;
the PM root traces to `ga-t34icc` and `ga-5gwr2e`. There is no additional
test or implementation bead and no dependency on an unlanded parent that
would prevent this catch-up from running.

The builder keeps its session in the gc-visible work directory and performs
the local merge in an owned `mktemp` checkout. It verifies the live PR and
fork branch still equal the full lease, then merges chosen main into that
published head. The final merge preserves both parents. A moved lease or
new code conflict is reported to mayor before proceeding.

Conflicting goldens are regenerated from the fully merged source through
`TestWorkQueryGolden`'s existing `-update` path in
`internal/config/workquery_parity_test.go`. A generator invocation such as
`go test ./internal/config -run '^TestWorkQueryGolden$' -args -update` is
an update/inner-loop operation; affected-package Bazel checks provide the
gate evidence. Inventory all output, including automatically merged fixtures
that need regeneration, and justify any output beyond the known 18.

The resolution changes generated fixtures rather than production query
behavior. It preserves merged main's unrelated changes, the published PR's
reviewed feature, priority order, holds, native/compatibility query forms,
and assigned recovery. It does not implement the two deferred human-label
paths or redesign query generation.

## Acceptance and evidence

| Requirement | Measurable evidence |
|---|---|
| Preserve published history | Full coordinates, both-parent comparisons, final commit/tree, and ancestor checks for the published head and chosen main |
| Regenerate from merged source | Recorded update command, complete fixture inventory, no conflict markers, and a second update producing no diff |
| Snapshots match the generator | Fresh `TestWorkQueryGolden` and `TestEffectiveQueryParity` PASS without `-update` |
| Retain #7601 query behavior | `TestEffectiveWorkQueryNoWorkBdInvocationBudget`, `TestEffectiveWorkQueryVisitsEachDistinctIdentityOnce`, and `TestEffectiveWorkQueryPoolDemandReusesTheOpenEphemeralScan` PASS |
| Retain #7366 human behavior | Exact `human` excluded from readiness/counting; eligible `Human` and `HUMAN` served and counted; clean/hold/assigned/priority guards PASS |
| Preserve demand/serve agreement | Fresh `TestDemandCountsExactlyTheClaimableRows`, `TestGoPredicateAndGeneratedQueryAgreeRowByRow`, `TestTierThreeServeRulesMatchTheGeneratedQuery`, and applicable prior routed/human guards PASS |
| Honest execution | Required named checks report actual PASS with zero FAIL/SKIP; commands, exits and raw logs retained; setup errors are not RED |
| Reviewable handoff | Committed clean local revision, verified bundle, evidence report, proposed same-PR body, author identity, and exactly one independent local review |

The delivery bead holds the full seven acceptance groups. Golden-update
success alone cannot show that either query change survived. Verification
must exercise both behaviors on the final merged source across the affected
single-store/federated and agent-shape variants. Any necessary verification
assertion change follows test-first evidence rules within this same scope.

Builder iteration stays within affected config and CLI tests. Applicable
hooks, formatting and drift checks remain active. Expensive local checks use
the existing load gate with threshold 15 and timeout deferral. A deferred
run returns this same scope with its actual evidence; it does not create a
replacement build, review or gate. Focused evidence is never presented as
full release criterion 3 or CI parity.

## Ownership and sequencing

1. Codex completes the local merge, regenerates snapshots, runs focused
   checks and retains the committed revision and verified bundle. It does
   not push or change the PR, GitHub reviews/comments, CI, clearance, main,
   host services, caches, or provider configuration.
2. The author creates one independent local `needs-review` bead with
   `discovered-from:ga-ik5v7uu`, full original PR coordinates and lease,
   reviewed base/head, author identity and evidence pointers. Codex review
   is allowed during the outage; the author cannot review its own work.
3. On PASS, the reviewer creates one standalone existing-PR `needs-deploy`
   re-gate with `build_bead=ga-ik5v7uu`, the review bead and exact local
   revision. Every original PR field, publication owner/lease, original
   lineage and landing guard survives this handoff. Route with
   `--no-formula --nudge` and verify the recorded target.
4. The builder returns `ga-ik5v7uu` OPEN/unassigned, `needs-mayor`, stripping
   build intake labels and setting the singleton route to mayor after
   finishing its own step/molecule. It sends mayor and PM the real local
   revision, review and bundle pointers.
5. Deployer runs a fresh complete release gate. Mayor publishes its gated
   revision to the same PR branch using the full lease and verifies the
   remote and PR head. Deployer then clears that new exact published head;
   required CI precedes mayor/MPR ordering and merge. Prior clearance is
   evidence for the previous head only.
6. Only mayor closes the delivery bead shipped after confirmed main
   landing. `ga-5gwr2e` stays open through review, gate and publication.
   `ga-gw831h` and `ga-ir0y7p` retain their validator routes and blocks edges
   on that guard until the same landing. Broader #6026/`ga-n03flr` and
   assigned-human recovery `ga-5k0ehr` remain separate work.

The proposed PR body preserves `Closes #7367`, sjarmak's contributor review
credit, and the two deferred scopes. The PM root closes for packaging only
after this plan is committed and its exact tracked path is verified clean;
that closure makes no implementation, gate or shipment claim.
