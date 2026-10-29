# Antfly infrastructure binding

- Serve through EngineOpener. Never claim exclusive migration fences or a
  physical migration census without an implementation that excludes all writers.
- Resolve every target from this binding's explicit antfly.json under CityRoot;
  ambient BEADS_* variables must not select the infrastructure engine.
- Infrastructure uses its own schema/workspace, separate from core Beads work.
- All persistent mutations, including guarded batches and graph pours, publish
  through one conditional Antfly snapshot head. Do not embed MemStore or promote
  methods that can bypass persistence.
- Retry only a positively rejected snapshot CAS. Never replay user callbacks or
  unknown commit outcomes. Preserve revision/fence tombstones across deletion.
- AntflyStore must expose atomic Claim as well as conditional release. Graph
  dispatch and the native deferred-nudge queue require both. A nudge class
  assignment does not by itself move the upstream file queue: retain the
  BeadsNudgeQueueMode capability, queue-mode shim and state-aware shadow view.
