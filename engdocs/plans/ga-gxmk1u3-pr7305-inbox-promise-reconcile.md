# PR #7305: verify supersession before reconciling the inbox promise

Mail previews must describe what remains unread after delivery, keep ordinary
mail visible alongside empty auto-handoffs, and retain omitted messages in the
read-only SessionStart blocks. PR [#7305](https://github.com/gastownhall/gascity/pull/7305)
addresses [#7208](https://github.com/gastownhall/gascity/issues/7208), but upstream
#7601 rewrote the same delivery paths. The first task is to establish whether
current main already satisfies the PR's own regression tests. A reconcile is
authorized only after those tests demonstrate a remaining behavioral defect.

## Assignment and coordinates

| Item | Record |
|---|---|
| Mayor order | `gm-wisp-xpj3jdn`, 2026-10-12 12:05:11Z |
| PM packaging | `ga-gxmk1u3` |
| One active delivery | `ga-nfx9oz9`, `delegated-build`, target `gascity/codex` |
| Superseded builder vehicle | `ga-sxeao3k`, CLOSED, `ready-to-build` removed |
| Existing branch | `quad341/gascity`, `deploy/ga-yw3kxx-gate` |
| Full published head and publication lease | `cfe397833d80ff2d400603cf1fceaf50e9cda221` |
| Original deploy / stale re-gate | `ga-yw3kxx` / `ga-lwoi6e` |
| PM intake main | `a8ef7eb134c467141f691b9ce7a5489becd16f29` |
| #7601 main commit | `515eb2b698f173042088ef9d38ad891a28cdb11b` |
| Publication, ordering hold and disposition owner | mayor |
| Main-landing guard for existing validator followups | `ga-1xtdl2` |
| Existing #7000 landing prerequisite for the gate | `ga-pvoy2u` |
| Target | Cleared replacement head or evidenced supersession before 17:00Z |

PM read the live PR OPEN/DIRTY at the unchanged lease and issue #7208 OPEN.
Fetched main matches the intake pin. Read-only `git merge-tree` exits 1 with
exactly `cmd/gc/cmd_mail.go` and `cmd/gc/prime_auto_handoff_inject.go` conflicting;
raw output is `/var/tmp/pr7305-pm-merge-tree.txt`. Execution fetches and pins main
again; this intake pin does not prescribe a later tested tree.

## One vehicle and preserved dependencies

The delivery has `discovered-from` edges to this PM root and `ga-sxeao3k`.
Before closing the old vehicle, PM added a real replacement `blocks` edge from
stale gate `ga-lwoi6e` to `ga-nfx9oz9`; its existing `ga-pvoy2u` blocker remains.
Closing the old vehicle and removing `ready-to-build` were one edit. The old
`mol-tdd-build` wrapper `ga-vtkdgg4` has seven OPEN steps and no current step;
it and sling tracker `ga-hrfxjmz` remain historical attachments. The replacement
is a flat `delegated-build` story, slung with `--no-formula --nudge`.

Mayor expressly authorized one Codex vehicle. No additional design, test-first
or implementation scope is created. Tracker import is a no-op because the
tracker-to-beads skill and sibling tracker skills are absent.

## Step 0: nine original regressions, current main without the fix

The worker uses an owned `mktemp` checkout while its active session stays in
the gc-visible directory. Before any production mutation or re-port, transplant
only the original regression tests from the lease onto pinned current main.
Preserve assertions and disclose any test-only harness/signature adaptation
required by #7601. Record original test-source SHA, baseline main/tree, commands,
actual executed names/results and raw evidence proving the production fix is
absent from this baseline.

The five `mail_inject_inbox_promise_test.go` tests are
`TestMailCheckInjectKeepsInboxPromiseForArchivedAutoHandoffs`,
`TestMailCheckInjectAutoHandoffsDoNotDisplaceOrdinaryMail`,
`TestMailInboxListsAutoHandoffsUntilInjectArchivesThem`,
`TestMailCheckInjectRendersAutoHandoffBacklogAsOneLine`, and
`TestFormatInjectOutputCollapsedAutoHandoffGroup`.

The four `cmd_prime_test.go` additions are
`TestPrimeUnreadMailInjectionNeverClaimsArchiveOnDelivery`,
`TestSessionStartAutoHandoffInjectionAssertsRulesAAndB`,
`TestSessionStartAutoHandoffInjectionRendersBacklogAsOneLine`, and
`TestSessionStartAutoHandoffOmittedBodyBearingStayVisible`.

If all nine actually PASS without the fix, stop. Close only this delivery
no-op with that evidence and mail mayor and PM `PR #7305 superseded by #7601`.
Mayor takes the public PR/issue disposition and stale-gate retirement to the
operator. No public PR closure, reconcile, review or gate is authorized on this
branch. A setup/compile error, SKIP, missing test or incomplete run is not
supersession; keep this same scope available with its exact continuation state.

## Conditional reconcile: existing recipe, local artifacts

On genuine behavioral RED, fork from the original published lease and merge
chosen fresh main as a merge commit. Preserve published ancestry and both
parents; never fork the revision from main or rebase the published history.
Keep #7601's one store, exact identity, single reads and timeout-safe delivery.
Keep the PR's truthful inbox promise, read-only SessionStart archive wording,
hidden unread mail visibility and omitted auto-handoff visibility. Change only
what the merge requires; report extra scope rather than redesigning.

All original regressions and relevant `cmd/gc` mail, prime and hook-injection
tests must actually PASS under applicable repository Bazel gates. Retain genuine
RED/GREEN, exact executed inventories, owned PASS evidence and any unchanged
skip attribution. Preserve merged tests, synchronize BUILD files if needed,
and run applicable hooks/nogo/policies using existing load/isolation recipes.
Do not change shared libraries, caches, providers, tmux servers or test budgets.

Deliver a clean local revision with exact chosen base, branch, head, tree,
ordered parents and lease-ancestry proof. Provide a self-contained bundle with
SHA256 and reconstruction verification, report and raw evidence/hash manifest.
A proposed SAME-PR body is local only and required only if it must change;
retain `Closes #7208`. Later-main read-only merge evidence does not certify a
later tree was executed.

## Review, publication, clearance and exit

Create one independent LOCAL review by a different actual session from the
author. Carry all original PR, publication, source and artifact fields through
every handoff and read back routes and context mail. Review PASS returns the
exact revision to mayor as `needs-mayor`; reviewer/mayor coordinates one fresh
standalone existing-PR deploy gate replacing `ga-lwoi6e`. No old clearance
transfer or second PR. Deployer independently gates the exact reviewed source.

Mayor alone lease-publishes the same fork branch by fast-forward and verifies
actual fork/PR head. Deployer clears that NEW exact published head and current
required CI. Mayor/MPR own ordering-hold release, queue re-entry and merge.
Workers and reviewers take no public push, PR/body/comment/review/CI, clearance,
hold, issue/PR closure or merge action.

On the reconcile branch, delivery remains OPEN/unassigned `needs-mayor`, routed
mayor, until mayor verifies actual MAIN LANDING and closes it shipped. Remove
`delegated-build` on return. Step 0's evidenced no-op is the explicit alternate
exit and claims no shipment. This PM root closes only after this exact plan is
committed and its scoped status re-verified clean.

Preserve `ga-1xtdl2` and validator followups `ga-rdjgnp`/`ga-cmt6iq`. Supersession
requires mayor to wire replacement prerequisites before retiring their landing
guard. Mayor explicitly confirmed in `gm-wisp-y1wf79q` that the replacement
gate MUST retain OPEN #7000 landing prerequisite `ga-pvoy2u`: earlier full
#7305 gates failed without that fix. STEP 0, local reconcile and independent
review proceed now; only the gate waits. This may prevent clearance by 17:00Z.
PM's risk notice `gm-wisp-gx6cx1v` was full peek-verified; mayor's response was
read in full. A supersession result returns disposition directly to mayor.
