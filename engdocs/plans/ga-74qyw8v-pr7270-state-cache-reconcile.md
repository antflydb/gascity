# PR #7270: reconcile refresh back-off after protect-first landing

PR [#7270](https://github.com/gastownhall/gascity/pull/7270) spaces failed
tmux state-cache refreshes to avoid repeatedly spawning tmux during an outage.
The architect sequenced it behind the live-holder protection in #7529.
That protection has landed, and mayor reports it is deployed. The remaining
work is one local merge reconcile, independent review and fresh clearance
for the existing PR before MPR resumes on 2026-10-12 at 17:00Z.

## Assignment and coordinates

| Item | Record |
|---|---|
| Mayor order | `gm-wisp-9c6f5xy` |
| PM packaging | `ga-74qyw8v` |
| One active delivery | `ga-v1xajlv`, `delegated-build`, target `gascity/codex` |
| Superseded builder vehicle | `ga-mz12d4g`, CLOSED, `ready-to-build` removed |
| Original reviewed/gated lineage and protect-first ruling | `ga-9s1sl8` |
| Existing PR branch | `quad341:deploy/ga-9s1sl8-gate` |
| Full published head and publication lease | `f6d93b2bd2c2c1c2c2731c076d9d18e85424c859` |
| Publication and MPR ordering-hold owner | mayor |
| Protect-first main commit | `a862cb243d72254ea090947baf623c8e3e2b2228` |
| PM intake main | `243efe565174f6024672c21a686af983ef5e6ff1` |

PM verified the live PR is OPEN/DIRTY at the unchanged full lease, and
the protect-first commit is an ancestor of fetched main. Read-only
`git merge-tree --write-tree --name-only` exits 1 with exactly two conflicts:
`internal/runtime/tmux/state_cache.go` and
`scripts/runtime_tmux_manifest_test.go`. Raw output is retained at
`/var/tmp/pr7270-pm-merge-tree.txt`. The builder re-verifies the lease and
fetches current main again; the PM pin is intake evidence.

## One delivery vehicle

Mayor explicitly permits Codex for this settled reconcile during the Claude
outage. The new delivery has `discovered-from` edges to PM packaging and
the original reconcile. The old builder vehicle was closed as superseded
and had `ready-to-build` removed in the same edit, with `gc.work_outcome=no-op`
and `superseded_by=ga-v1xajlv`. It has no implementation or shipment claim.

The old `mol-tdd-build` wrapper was inspected with all seven steps open
and no current step. It is retained on the historical closed vehicle and
does not govern the new flat delivery. The new scope is routed with
`--no-formula --nudge`; no duplicate builder/test/review vehicle is created.

## Settled resolution

The recipe comes from the architect's protect-first ruling on `ga-9s1sl8`
and mayor's `ga-mz12d4g` description. PM adds measurable acceptance and
handoff ownership rather than a new design.

1. Work in an owned `mktemp` checkout while the active session stays in its
   gc-visible directory. Start from the preserved published PR head and
   merge chosen current main as a merge commit. Preserve both parents;
   verify full coordinates and lease before mutation.
2. In `state_cache.go`, retain main's per-generation singleflight keying
   and omit the PR's dirty-read `Forget("refresh")` line. Preserve the
   reviewed back-off alongside current generation behavior.
3. Recompute manifest, untagged, integration-only and six-shard counts
   from real canonical discovery on the fully merged test tree. Use the
   existing discovery and guard machinery. Retain discovered names and
   observed counts; neither parent's constants nor the 10-07 recipe numbers
   are final targets. Preserve every legitimate merged test and build tag.
4. Insert exactly one `h.cache.Invalidate()` between the two reads in
   `TestStateCacheBackoff_OverlappingFailuresAreOneOutage` and
   `TestStateCacheBackoff_StaleFailureAfterANewerPublishOpensNoHold`.
   Preserve their assertions and timing requirements.
5. Re-check the current #7529 `Start`/`ensureRunning` protection against
   this back-off and record the code-path argument plus relevant regression
   execution. Preserve the accepted stale-window residual and live-holder
   protection. A changed interaction contract or conflict beyond the two
   settled files is reported on the same bead before scope expansion.

If main has landed the separate no-literal manifest redesign, report that
changed precondition rather than restoring literals. Separate liveness
follow-ups, host/runtime configuration, public API, new test targets,
resource budgets and back-off redesign remain outside this reconcile.

## Acceptance and retained evidence

| Requirement | Evidence |
|---|---|
| Preserve history | Full live PR/fork coordinates, final merge commit/tree, both-parent comparisons and ancestor proofs for the published head and chosen main |
| Derive manifest values | Raw canonical discovered names/counts, exact six-shard partition, repeatable recomputation and no drift |
| Manifest guards | Actual PASS for `TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory`, `TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce`, and relevant discovery/drift checks |
| Back-off and generation behavior | Fresh named `TestStateCacheBackoff_*` and state-cache results on the final committed source; both modified tests and every diff-owned test PASS with zero FAIL/SKIP |
| Protect-first interaction | Current #7529 behavior retained, concrete interaction argument and applicable regression results; no new destructive start path |
| Affected-package quality | Full `internal/runtime/tmux` Bazel coverage, relevant integration-tagged coverage where required, active hooks, nogo/vet, formatting and generated/BUILD/manifest checks |
| Reviewable local result | Clean committed source, self-contained verified bundle/hash, report/raw logs, exact base/head/tree and optional proposed same-PR body |

The delivery notes contain all eight acceptance groups. Builder TDD and
test-evidence rules apply to this same scope. A parse/conflict-marker or
loader error is setup failure, not RED; skipped or unexecuted owned tests
cannot satisfy PASS. Existing tests define the back-off schedule, stale
cliff clamp, observation semantics, overlapping failure handling, success
reset and lifecycle invalidation permit.

Iteration stays within state-cache, manifest and tmux coverage. Expensive
local commands use the existing load gate with threshold 15 and timeout
deferral, isolated runners and task-owned tmux sockets. A deferred command
returns this same bead with its real evidence. Focused results do not
certify full release criterion 3 or CI action-key parity.

## Review, publication and landing

1. Codex keeps the revision local. It sends no branch push, PR/comment/review
   mutation, CI rerun, clearance or main merge. This supersedes the old
   reconcile's builder-push and deployer-publication instructions.
2. After commit and exact-source clean verification, the author creates
   one independent local `needs-review` bead with `discovered-from:ga-v1xajlv`.
   A different session reviews it. Carry author identity, branch, complete
   reviewed base/head/tree, all original PR fields, full lease/publication
   owner, original gate and prior reconcile, and bundle/evidence pointers.
3. The passing reviewer creates one fresh standalone existing-PR
   `needs-deploy` re-gate with `build_bead=ga-v1xajlv`, review pointer and the
   same complete contract. Route with `--no-formula --nudge` and verify the
   actual target. Old `ga-9s1sl8` clearance does not transfer.
4. The builder returns `ga-v1xajlv` OPEN/unassigned, `needs-mayor`, stripping
   build intake labels and setting the singleton mayor route after finishing
   its own step/molecule if any. It mails mayor and PM the real local
   revision, independent review and bundle pointers.
5. Deployer runs a fresh complete gate on that exact local revision. Mayor
   alone publishes to the same branch using the full lease as a
   fast-forward preserving ancestry and verifies the remote/PR head.
   Deployer then clears the new exact published head and required CI;
   mayor lifts the existing MPR ordering hold and mayor/MPR merges.
6. Only mayor closes the delivery shipped after confirmed main landing.
   Local completion, review, gate or publication leaves it open. The PM
   packaging root closes only after this internal plan is committed and
   its exact tracked path is verified clean.

A proposed PR body is needed only if its claims change. Preserve `Refs #7170`,
the reviewed behavior and accepted residuals, and update historical
interaction/head/evidence statements honestly. Do not invent a closing issue
or claim a fresh full gate in the builder's focused report.
