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
created a convoy in the work ledger, and persisted session mail and nudges. A
legacy v1 standalone cook routes to work and remains unsupported there. Gas
City's session reconciler v2 currently requires its SQLite-specific binding;
this provider uses the legacy reconciler. `gc bd` forwarding to an exec work
provider remains restricted; use `bd` directly for work and a supported explicit
work_query, for example:

```toml
work_query = "bd ready --metadata-field gc.routed_to=report/implementer --unassigned --json --limit 1"
```

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
