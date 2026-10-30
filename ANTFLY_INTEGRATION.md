# Native Antfly infrastructure for Gas City

This fork adds the compiled `antfly-beads` provider. Core work uses the
Antfly backend in antflydb/beads; graph, sessions, messaging, orders and nudges
use a separate Antfly workspace containing Gas City's coordination schema.
Both can run embedded with AntflyLite or against a remote Antfly endpoint.
There is no SQLite data path in this topology.

```toml
[beads]
provider = "exec:/absolute/path/to/gc-beads-antfly"

[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "antfly-beads"
config_ref = "infra"
```

Provision `<city>/.gc/storage/infra/antfly.json` before boot:

```json
{
  "path": "infra.aflite",
  "library": "/absolute/path/to/libantfly.dylib",
  "table": "gascity_infra",
  "workspace": "infra"
}
```

Paths resolve against the binding directory, never the supervisor's current
working directory. For remote operation replace `path` and `library` with
`url`, optionally `token_env`, `database` and `namespace`. Tokens remain in the
referenced environment variable. Native libraries must provide ABI 3. Work
must use a different workspace; the infrastructure opener refuses an existing
workspace carrying another schema or prefix.

## Transaction and capability contract

The binding composes Gas City's existing in-memory domain logic inside each
transaction. Persistent reads reload the current Antfly snapshot. Every mutation
stages immutable records and publishes one conditional workspace head. Guarded
batches, metadata CAS, revision-guarded terminal close, transaction callbacks and
formula graph pours therefore publish entirely or not at all. There is no SQLite mirror or subprocess in the infrastructure data path.
The deferred queue migrates legacy file records through the existing shim;
operational skip counters and runtime logs remain local diagnostics.

Known rejected snapshot publications can replay declarative operations against
a fresh snapshot. Unknown outcomes return an error for caller reconciliation;
they are never replayed automatically. User transaction callbacks execute once,
even on conflict. Revisions and ownership fences survive deletion and recreate.
Session write instrumentation runs after a confirmed commit. Clone-local strings
are handle-local scratch data, as permitted by Store's contract.

The provider implements EngineOpener and namespace fences for its assigned
classes. It refuses the core Work class, which has Beads' stronger lifecycle
policy. It also refuses exclusive physical migration fences, retained-guard
recovery and automatic cutover. Use a fresh born-split city; converting an
existing SQLite city requires a separately designed migration protocol.

## Scope and long-term direction

This is a native persistent infrastructure binding, not a claim of complete
Dolt SQL or Git-style merge compatibility. Whole-workspace reads and domain
operations currently cost O(number of records); writes reuse content-addressed
rows but retain unreachable immutable history. Large deployments need indexed
query paths, bounded snapshot loading and reachability-based garbage collection.
Core Beads knowledge outbox/memory/entity resolution remains in its own backend;
infrastructure records are not silently promoted into that knowledge graph.
A future shared domain SDK can remove the separate work executable provider and
support typed infrastructure search/projection without weakening lifecycle rules.

## Verified compatibility and limits

The provider passes Gas City's shared Store, ConditionalWriter, GuardedBatch,
metadata CAS, atomic terminal-close and pinned-ID suites. Real AntflyLite tests
open, write, close and reopen the production store and provider constructors.
Additional tests cover independent handles, callback rollback, ambiguous commit
acknowledgements, revision/fence reuse and atomic formula graph publication.
The exact fork CLI inventory is embedded alongside the upstream inventories;
adding it does not grant new commands or bypass the split-city command policy.

The real city has successfully cooked a formula-v2 graph with four graph nodes,
created a convoy in the work ledger, and persisted session mail and nudges.
The exact Antfly executable provider now admits normal `gc bd` forwarding under
the existing command inventory, class routing and lifecycle guards. Other exec
providers remain restricted. Beads supplies the shared boolean query role and
its conformance suite; default factory queries still need end-to-end acceptance.
Persistent legacy molecule records are admitted, while complete legacy formula
cooking and migration remain under qualification.

The current session reconciler is selected through an explicit provider
capability, then still verifies live revision, conditional-write and terminal
close capabilities before effects. Antfly declares these capabilities and owns
closed-session retention. The older coding trial used the legacy reconciler;
it does not prove current-reconciler factory acceptance.

This downstream module pins the reviewed immutable antflydb/beads SDK commit in
go.mod. The module-boundary test and replacement guard admit only that exact
remote pin; local checkout replacements and unreviewed fork updates still fail.

The deferred nudge queue also uses Antfly, rather than merely storing its
shadows there. `BeadsNudgeQueueMode` selects the canonical bead queue; atomic
Claim, conditional release and guarded repinning preserve ownership and delivery
batch identity. The poll probe reads eligibility without acquiring claims. The
existing shim transfers eligible whole legacy pending batches, while wait readers
use the queue's state-aware shadow view. A selected native queue fails closed.
Dispatch skip counters remain local diagnostics, outside delivery state.

The validated native runtime is the ABI-3 libantfly build from Antfly
`codex-main-ci-post1338-oct28`. An older post1326 build stalled during the longer
factory run inside `antfly_db_get_timestamp`; ABI compatibility alone is not a
soak-test guarantee. Keep the real concurrent-handle and close/reopen tests in
the runtime qualification gate. The generic transport cache measured warm
reads of the live 79-record workspace at 8 ms, 1 ms and below 1 ms.

The coding trial completed planning, implementation and independent review, with
45 artifact tests passing and all three tasks plus their convoy closed. During
that run the deferred delivery queue still used the upstream file queue. The
native queue was then wired and verified through real provider close/reopen,
CLI enqueue, claim, acknowledgement and terminal-shadow checks. This sequence
does not establish a fully unattended production soak.

## Reproducing the integration gate

Use Bazel with a qualified ABI-3 native library and an actual checkout root:

```sh
bazel test //internal/beads:beads_test \
  //internal/storebinding/antfly:antfly_test //cmd/gc:gc_test \
  //scripts:scripts_test --config=ci \
  '--test_filter=^Test(Antfly|NativeAntfly|NativeBinding|FactoryIsResource|NudgeQueueShim|CheckGomodReplaceGuard|StorageProviderBundle|CompiledStorageProviderRegistry|StorageSurfaceDeclares|ModuleGraphCarriesOnly)' \
  --test_env=ANTFLY_TEST_LIBRARY=/absolute/path/to/libantfly.dylib \
  --test_env=GC_TEST_REPO_ROOT=/absolute/path/to/gascity
```

This final focused gate passed all four targets. The full bdflags, telemetry
and provider targets also passed during implementation. This does not assert
that the complete upstream test suite passed on this host. Native tests skip
without `ANTFLY_TEST_LIBRARY`; keep it set when qualifying an embedded build.

## Stock housekeeping and production maintenance

Native infrastructure point and filtered reads use indexes published atomically
with their records. Type, status, assignment, parent, label and exact metadata
selectors retain the shared ListQuery ordering and tier semantics. Old snapshots
remain readable and acquire indexes on their next write. Broad scans and writes
still reconstruct the domain snapshot. Core work point reads have separate
native indexes in the Beads backend; broad work queries remain a scaling task.

`gc storage backup <binding> --file <new-file>` writes a checksummed logical
archive without overwriting another file. `gc storage restore <binding> --file
<archive>` validates schema/prefix and requires a fresh workspace, preserving
revision and claim-fence tombstones without rolling a live ledger backward.
Full native engine archives, including memory evidence outside these snapshots,
are exposed by the Beads SDK and the engine administrative backup API.

`gc storage gc <binding>` collects unreachable immutable documents under a
resumable head fence. Opening an interrupted workspace gives maintenance an
operator handle while ordinary reads/writes stay fenced. Before adoption,
collection validates the exact schema/prefix. Every delete and release checks
fence ownership. On the retained factory, collection removed 949 unreachable
documents and kept 320; pre/post logical archives matched byte-for-byte.
Branching core work requires a catalog-wide collection barrier and is refused
by this dedicated-workspace collector.

`gc storage retain <binding> --policy reap|compact|sessions --dry-run` previews
selection in an isolated view; without `--dry-run` it publishes once through
OCC. `--all` visits each dedicated infrastructure binding once. Queue deliveries,
operation journals and external messaging retain their domain lifecycle owners.
Session retention uses the existing conditional session collector, preserving
open sessions and sessions with children. A failed publication reports no
committed retention result.

`gc storage retain-work --work-rig <rig> --policy reap|compact` provides the typed
work-plane maintenance door; omit `--work-rig` for explicit city work. The
opaque `gc bd antfly provider` command remains refused on split cities. Native
pack paths use these typed commands instead of SQL selection. Foreign workflow
ownership remains held until cross-store traversal can prove it drained.
The JSONL order uses typed project/branch archive identities from `bd info` and
successfully exported both retained city/rig scopes in local-only mode.

Qualification includes native constructor/reopen/concurrency, indexed reads and
logical restore; the same indexed-read/archive oracle also passes remotely.
Thirty repeated embedded/remote cycles passed (a bounded stress run, not a long
factory soak). A full stock factory acceptance run with multiple rigs,
crash/restart/redelivery and real code merges remains outstanding, as do broad
workflow qualification and a long factory soak. Core immutable-history collection
now has the separate SDK catalog barrier described below.

Fresh stock-template startup selected the current reconciler and started a
managed session on native storage. That run exposed a missing closed-since
filter in the work query backend, now covered through the shared Querier, and
a SQLite-only builtin operation-journal default. Antfly now explicitly opts
into default journal recording while journal opening still probes conditional
write capabilities. Native persistence/replay and existing default sweeper/doctor
regressions pass. Full two-rig factory acceptance is still being exercised.

## Factory runtime routing and read admission

The vetted `gc-beads-antfly` work provider opens the native Beads SDK directly
for in-process work-store operations, eliminating a subprocess per controller
read. Its executable still owns provider lifecycle. The shared issue-role
adapter's policy-free close is overridden with the guarded Antfly work close;
blockers, assignments and revision checks remain engine-owned.

Stock role prompts also invoke bare `bd`. Install the Beads pack's `bin/bd`
router on agent PATH and pin the raw executable through `ANTFLY_BD_BIN`,
`BD_BIN` and `workspace.env.BD_BIN`. It calls the existing `gc bd` front door
without an explicit city scope override, preserving ID-prefix rig routing and
class ownership. The raw provider and inventory calls use the pin directly.
A recursion guard refuses a misconfigured pin. Explicit native operator
commands use the raw CLI.

Foreign work dependency references survive the core store. The split-store
ready/claim gate resolves their owning store. Native work CLI close checks
foreign blockers too, including explicit external references, and refuses
unknown or missing owner reads. An explicit force close remains an operator
override. These cross-store observations are not a distributed transaction;
the native work engine still performs local checks atomically. Actual two-rig
readiness and close-refusal acceptance passes in both embedded and remote modes.

Infrastructure constructor and health checks now validate only indexed schema
metadata. Committed reads may proceed during a pending publication; mutations
remain serialized per handle and publish through the native OCC fence. A
delayed-publication regression proves a reader sees the prior committed state
and then the new state after commit. Close and maintenance hold exclusive
lifetime admission. This addresses observed stock-factory open/read stalls.

The SDK additionally exposes `bd antfly gc` / `CollectWorkspaceGarbage` for
core history. It fences the main catalog and all completed forks, marks their
current snapshots plus recursive merge bases, and guards deletes with main
ownership. Pending fork creation is refused; interrupted fences can be resumed.
Embedded and remote tests preserve snapshots and later three-way merges;
ownership-loss and foreign-schema tests pass. Generic infrastructure collection
continues to refuse core workspaces. This requires the pinned maintenance-aware
clients and may make old in-flight readers retry. Live audit/outbox evidence
is retained independently of physical unreachable-history collection.


## Stock command inventory and worktree housekeeping

The reviewed native inventory contains all 331 registered Beads commands and
admits `antfly gc`; provider forwarding still applies the existing command
policy and class routing. Tests reject partial or malformed inventories.
Branch pruning accepts linked Git worktrees and each rig's configured default
branch, skips suspended rigs and the current branch, and retains Git's safe
merged-branch deletion check. Real worktree/custom-default tests pass.

Live stock factories still exhibit storage deadlines under concurrent
housekeeping and session activity. Command admission and focused maintenance
tests are not a full factory qualification; multi-rig code merges, crash and
redelivery acceptance and sustained soak results remain outstanding.


## Legacy graph batches and live gate qualification

The pinned native SDK implements atomic `BatchApplier` for work graphs and
advertises the compiler's `step` work type. Its staged operations retain
lifecycle guards and publish through one conditional head. The shared work
adapter recognizes the typed Antfly snapshot-conflict sentinel as a proved
abort and retries within its existing bound. Lookalike error text and unknown
commit outcomes remain fail-fast. Embedded and remote adapter tests cover the
actual legacy root/step shape, forward metadata references, parents, assignment,
blocker refusal, release and closure.

Live `gc formula cook` followed by a refused dependent close, blocker close,
dependent close and root close passes in both owned stock cities. Legacy step
scaffolding remains intentionally excluded from the normal federated ready
frontier. This does not establish legacy per-step worker dispatch compatibility.
Live `gc bd gate create` timer gates also pass block/sweep/release/close against
both engines, using stock `gc ready` for the complete city frontier.

The immutable snapshot cache now charges payloads, keys and entry overhead to
its 8 MiB budget. A 3,000-row regression that formerly reread 2,086 documents
after one row changed now reads only the head, changed page and changed row.
Fresh heads, hash validation, revisions and maintenance fences remain required.
These fixes do not substitute for the remaining full-factory merge, crash,
redelivery and sustained soak acceptance.


Infrastructure changes compare the complete canonical state before publishing.
A refused metadata predicate, no-op release, empty callback or already-closed
record leaves the head version unchanged. These operations previously published
identical heads, invalidating indexed readers and causing unrelated writers to
lose their fences. The regression reproduces a refused metadata CAS advancing
version 1 to 2 before the fix, proves no publications for those no-ops afterward,
and requires an actual metadata transition to publish once. Native and remote
infrastructure conformance tests pass. The SDK pin also includes bounded retry
of explicit remote read-admission refusals, without retrying writes or unknown
commit outcomes.


A separate persistence test verifies the no-op invariant against real embedded
and remote transports: refused metadata predicates and empty transaction
callbacks preserve both exact committed bytes and the head token; a successful
metadata transition changes that token. This supplements the transport-fixture
publication-count regression.
