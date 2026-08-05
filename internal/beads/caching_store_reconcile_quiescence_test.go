package beads

import (
	"encoding/json"
	"testing"
	"time"
)

// Tier 5: quiescence. Every other tier asks whether the seam DETECTS a change.
// This one asks the complementary question the grid never puts to it: when
// nothing changed, does the seam stay SILENT?
//
// That property is load-bearing in production. cache-reconcile runs the full
// scan roughly every 90 seconds and turns each returned notification into a
// recorded event (cmd/gc/api_state.go, actor "cache-reconcile"). A merge that
// re-notifies an unchanged row does not just waste a write — it republishes
// the entire active universe on every cycle. Measured on the gc-management
// city 2026-08-05: 136,733 bead.updated events across 3,488 distinct beads in
// a 50-minute window (~39 re-emissions per bead), 211 MB of events.jsonl, with
// every re-emission of a given bead carrying a byte-identical payload.
//
// ga-ocypq2 (8c9d4d2eb, #4191) fixed one instance of this — labels, needs and
// dependencies are sets whose order carries no meaning, and an order-sensitive
// compare misfired on every pass. These tests pin that fix and extend the
// property to the whole compared-field surface, including the fields
// beadChanged compares but the event payload never serializes (Description,
// Needs, Ephemeral, DeferUntil, From, Ref) — which is precisely why the
// production symptom presented as "identical payloads, emitted forever".

// quiescentState builds a steady-state cache: rows present, deps known, no
// pending fence, no recent local mutation. Any notification out of a merge
// against a matching snapshot is therefore a spurious one.
func quiescentState(rows []Bead, deps map[string][]Dep) storeState {
	beadMap := make(map[string]Bead, len(rows))
	for _, b := range rows {
		beadMap[b.ID] = b
	}
	return storeState{
		beads:        beadMap,
		deps:         deps,
		depsComplete: true,
		// dirty/beadSeq/localBeadAt/deletedSeq intentionally nil: a quiescent
		// cache has no in-flight local writes. mutationSeq 0 matches startSeq 0
		// below, so no row is fenced and none is skipped as recently-local —
		// every row reaches the change-detection arm on its merits.
		mutationSeq: 0,
	}
}

// quiescentInputs is the snapshot a scan returns when the backing store has
// not changed since the cache was primed.
func quiescentInputs(rows []Bead, depMap map[string][]Dep, useFreshDeps bool) snapshotInputs {
	fresh := make(map[string]Bead, len(rows))
	for _, b := range rows {
		fresh[b.ID] = b
	}
	return snapshotInputs{
		freshByID:       fresh,
		confirmedClosed: map[string]Bead{},
		depMap:          depMap,
		useFreshDeps:    useFreshDeps,
		startSeq:        0,
		now:             fxNow,
	}
}

// depsOf primes the cache's deps map the way production does — via the same
// depsFromBeadFields derivation the reconcile path uses. That derivation is not
// a passthrough of Dependencies: when a row carries no structured dependencies,
// Needs is expanded into synthetic Dep rows. A fixture that primes only from
// Dependencies would leave the cache's deps map empty for a Needs-only row and
// manufacture a diff the production cache never has.
func depsOf(rows ...Bead) map[string][]Dep {
	out := make(map[string][]Dep, len(rows))
	for _, b := range rows {
		if d := depsFromBeadFields(b); len(d) > 0 {
			out[b.ID] = d
		}
	}
	return out
}

func ptrInt(v int) *int { return &v }

// TestReconcileQuiescence_UnchangedSnapshotIsSilent drives one merge whose
// snapshot is identical to the cache and requires zero notifications.
//
// The shapes below walk the compared-field surface of beadChanged. A shape
// that emits here is a field whose compare misfires on every reconcile pass in
// production, for every bead carrying it.
func TestReconcileQuiescence_UnchangedSnapshotIsSilent(t *testing.T) {
	deferAt := fxNow.Add(24 * time.Hour)

	cases := []struct {
		name         string
		rows         []Bead
		useFreshDeps bool
	}{
		{
			name: "plain row",
			rows: []Bead{bead("ga-1", "open")},
		},
		{
			name: "every scalar populated",
			rows: []Bead{beadWith("ga-2", "in_progress", func(b *Bead) {
				b.Priority = ptrInt(1)
				b.Assignee = "gascity/builder"
				b.From = "mayor"
				b.ParentID = "ga-parent"
				b.Ref = "mol-tdd-build.green"
				b.Description = "step instructions that the scan projection may not carry"
				b.Ephemeral = true
				b.DeferUntil = &deferAt
			})},
		},
		{
			// Needs and Description are compared by beadChanged but are
			// `omitempty` on the wire, so a misfire here is invisible in the
			// emitted payload — the exact shape of the production symptom.
			name: "needs set",
			rows: []Bead{beadWith("ga-3", "open", func(b *Bead) {
				b.Needs = []string{"step-a", "step-b", "step-c"}
			})},
		},
		{
			name: "metadata map",
			rows: []Bead{beadWith("ga-4", "open", func(b *Bead) {
				b.Metadata = StringMap{
					"gc.kind":         "workflow-finalize",
					"gc.outcome":      "skipped",
					"gc.root_bead_id": "ga-mrkql",
				}
			})},
		},
		{
			name: "dependencies carried on the row",
			rows: []Bead{beadWith("ga-5", "open", func(b *Bead) {
				b.Dependencies = []Dep{dep("ga-5", "ga-a"), dep("ga-5", "ga-b")}
			})},
		},
		{
			name:         "dependencies with useFreshDeps",
			rows:         []Bead{beadWith("ga-6", "open", func(b *Bead) { b.Dependencies = []Dep{dep("ga-6", "ga-a")} })},
			useFreshDeps: true,
		},
		{
			name: "mixed population, many rows",
			rows: []Bead{
				bead("ga-7", "open"),
				beadWith("ga-8", "in_progress", func(b *Bead) { b.Needs = []string{"x"} }),
				beadWith("ga-9", "open", func(b *Bead) { b.Dependencies = []Dep{dep("ga-9", "ga-7")} }),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := depsOf(tc.rows...)
			st := quiescentState(tc.rows, deps)
			in := quiescentInputs(tc.rows, deps, tc.useFreshDeps)

			res := runNewMerge(st, in)

			assertSilent(t, res, "unchanged snapshot")
		})
	}
}

// TestReconcileQuiescence_SecondPassIsSilent is the idempotence property
// stated directly: whatever the first merge does, a second merge over the same
// unchanged snapshot must add nothing. This is the invariant the production
// loop actually depends on, because it runs the same scan every ~90s forever.
func TestReconcileQuiescence_SecondPassIsSilent(t *testing.T) {
	rows := []Bead{
		bead("ga-1", "open"),
		beadWith("ga-2", "in_progress", func(b *Bead) {
			b.Needs = []string{"step-a", "step-b"}
			b.Metadata = StringMap{"gc.kind": "workflow-finalize"}
			b.Description = "instructions"
		}),
		beadWith("ga-3", "open", func(b *Bead) {
			b.Dependencies = []Dep{dep("ga-3", "ga-1"), dep("ga-3", "ga-2")}
		}),
	}
	deps := depsOf(rows...)

	for _, useFresh := range []bool{false, true} {
		name := "cachedDeps"
		if useFresh {
			name = "freshDeps"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := newMergeHarnessStore(quiescentState(rows, deps))
			in := quiescentInputs(rows, deps, useFresh)

			c.mu.Lock()
			first := c.mergeSnapshotLocked(in.freshByID, in.confirmedClosed, in.depMap, in.useFreshDeps, in.startSeq, in.now)
			second := c.mergeSnapshotLocked(in.freshByID, in.confirmedClosed, in.depMap, in.useFreshDeps, in.startSeq, in.now)
			c.mu.Unlock()

			// The first pass is allowed to settle representation drift. The
			// second has nothing left to settle: if it still speaks, the loop
			// never converges and every subsequent cycle republishes the row.
			if len(second.notifications) != 0 {
				t.Errorf("second pass over an unchanged snapshot emitted %d notification(s), want 0 — the reconcile loop does not converge",
					len(second.notifications))
				for _, n := range second.notifications {
					t.Errorf("  spurious: %s %s", n.eventType, n.bead.ID)
				}
			}
			if second.updates != 0 || second.adds != 0 || second.removes != 0 {
				t.Errorf("second pass counters = adds:%d updates:%d removes:%d, want all 0",
					second.adds, second.updates, second.removes)
			}
			// Reported so a regression that moves noise from pass 2 into pass 1
			// is still visible in the failure output.
			t.Logf("first pass: adds:%d updates:%d removes:%d notifications:%d",
				first.adds, first.updates, first.removes, len(first.notifications))
		})
	}
}

// TestReconcileQuiescence_SetOrderDrift pins ga-ocypq2 (8c9d4d2eb, #4191).
// The Dolt rig store does not guarantee a stable order across scans, so the
// same logical row can come back with its sets permuted. Order carries no
// meaning for needs, labels or dependencies; a permutation must not read as a
// change. Without the order-insensitive compare this fires on every pass, for
// every bead holding a set — which is how the original flood was found.
func TestReconcileQuiescence_SetOrderDrift(t *testing.T) {
	cached := beadWith("ga-1", "open", func(b *Bead) {
		b.Needs = []string{"step-a", "step-b", "step-c"}
		b.Dependencies = []Dep{dep("ga-1", "ga-x"), dep("ga-1", "ga-y"), dep("ga-1", "ga-z")}
		b.Labels = []string{"ci", "p0"}
	})
	// Same multisets, different order — a permuted re-read, not an edit.
	permuted := beadWith("ga-1", "open", func(b *Bead) {
		b.Needs = []string{"step-c", "step-a", "step-b"}
		b.Dependencies = []Dep{dep("ga-1", "ga-z"), dep("ga-1", "ga-x"), dep("ga-1", "ga-y")}
		b.Labels = []string{"p0", "ci"}
	})

	st := quiescentState([]Bead{cached}, depsOf(cached))
	in := quiescentInputs([]Bead{permuted}, depsOf(permuted), false)

	res := runNewMerge(st, in)

	assertSilent(t, res, "permuted sets")
}

// TestReconcileQuiescence_NilVersusEmptyCollections covers the other
// representation drift a projection introduces: a store that returns an absent
// collection as nil where the cache holds an allocated-but-empty one, and vice
// versa. Both encode "no members" and must compare equal.
func TestReconcileQuiescence_NilVersusEmptyCollections(t *testing.T) {
	nilSide := beadWith("ga-1", "open", func(b *Bead) {
		b.Needs = nil
		b.Dependencies = nil
		b.Metadata = nil
	})
	emptySide := beadWith("ga-1", "open", func(b *Bead) {
		b.Needs = []string{}
		b.Dependencies = []Dep{}
		b.Metadata = StringMap{}
	})

	for _, tc := range []struct {
		name          string
		cached, fresh Bead
	}{
		{"cache nil, scan empty", nilSide, emptySide},
		{"cache empty, scan nil", emptySide, nilSide},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := quiescentState([]Bead{tc.cached}, depsOf(tc.cached))
			in := quiescentInputs([]Bead{tc.fresh}, depsOf(tc.fresh), false)

			res := runNewMerge(st, in)

			assertSilent(t, res, "nil/empty drift")
		})
	}
}

// TestReconcileQuiescence_FreshDepsOmitsKnownRow guards the second change
// gate, depsChanged(c.deps[id], freshDeps), which compares the cache's
// separate deps map rather than the row's own Dependencies field.
//
// Under useFreshDeps, depsForReconcileLocked returns depMap[id] unconditionally
// (caching_store_reconcile.go). A scan that returns the row but omits it from
// the dep map therefore yields empty fresh deps against non-empty cached deps
// — a difference that absorbing the row does not resolve, because the row it
// absorbs still carries the dependencies. If that reads as a change, the bead
// re-notifies on every cycle forever.
//
// Whether the scan can legitimately omit a known row is the open design
// question; this test states the consequence either way, so a fix has to
// answer it deliberately rather than by accident.
func TestReconcileQuiescence_FreshDepsOmitsKnownRow(t *testing.T) {
	row := beadWith("ga-1", "open", func(b *Bead) {
		b.Dependencies = []Dep{dep("ga-1", "ga-x")}
	})
	st := quiescentState([]Bead{row}, depsOf(row))

	// Row present in the snapshot, absent from the dep map.
	in := quiescentInputs([]Bead{row}, map[string][]Dep{}, true)

	c, _ := newMergeHarnessStore(st)
	c.mu.Lock()
	first := c.mergeSnapshotLocked(in.freshByID, in.confirmedClosed, in.depMap, in.useFreshDeps, in.startSeq, in.now)
	second := c.mergeSnapshotLocked(in.freshByID, in.confirmedClosed, in.depMap, in.useFreshDeps, in.startSeq, in.now)
	c.mu.Unlock()

	t.Logf("first pass emitted %d notification(s)", len(first.notifications))
	if len(second.notifications) != 0 {
		t.Errorf("dep-map omission still emitting on pass 2: %d notification(s), want 0 — this row re-notifies on every reconcile cycle",
			len(second.notifications))
		for _, n := range second.notifications {
			t.Errorf("  spurious: %s %s", n.eventType, n.bead.ID)
		}
	}
}

// TestReconcileQuiescence_IsBlockedProjectionDrift covers the one compared
// field that is explicitly allowed to be absent: IsBlocked carries bd's
// denormalized ready-work projection, and nil means "the store did not provide
// the projection" — NOT "false".
//
// beadChanged compares it with boolPtrEqual, for which nil and &false are
// different. So whenever the cache holds a row whose projection was populated
// and a scan returns the same row without it (or the reverse), the row reads as
// changed on every pass, forever, with a byte-identical payload.
//
// This is the shape that fits the production capture: of 3,466 distinct beads
// churning on the gc-management city 2026-08-05, 1,692 carried NO dependencies
// and NO needs, so the deps gate cannot account for them. IsBlocked is the
// field that varies by which read path produced the row.
func TestReconcileQuiescence_IsBlockedProjectionDrift(t *testing.T) {
	yes, no := true, false

	for _, tc := range []struct {
		name            string
		cached, scanned *bool
	}{
		{"cache has projection, scan omits it", &no, nil},
		{"cache omits projection, scan has it", nil, &no},
		{"cache blocked, scan omits it", &yes, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cached := beadWith("ga-1", "open", func(b *Bead) { b.IsBlocked = tc.cached })
			scanned := beadWith("ga-1", "open", func(b *Bead) { b.IsBlocked = tc.scanned })

			c, _ := newMergeHarnessStore(quiescentState([]Bead{cached}, depsOf(cached)))
			in := quiescentInputs([]Bead{scanned}, depsOf(scanned), false)

			// Two passes: an absent projection must be settled by the first
			// merge, not rediscovered on every subsequent one.
			c.mu.Lock()
			first := c.mergeSnapshotLocked(in.freshByID, in.confirmedClosed, in.depMap, in.useFreshDeps, in.startSeq, in.now)
			second := c.mergeSnapshotLocked(in.freshByID, in.confirmedClosed, in.depMap, in.useFreshDeps, in.startSeq, in.now)
			c.mu.Unlock()

			t.Logf("first pass emitted %d notification(s)", len(first.notifications))
			if len(second.notifications) != 0 {
				t.Errorf("projection drift still emitting on pass 2: %d notification(s), want 0 — "+
					"this row re-notifies on every reconcile cycle", len(second.notifications))
				for _, n := range second.notifications {
					t.Errorf("  spurious: %s %s", n.eventType, n.bead.ID)
				}
			}
		})
	}
}

// TestReconcileQuiescence_SelfEmittedEventDoesNotSustainChurn is the whole bug
// in one test: the reconcile loop is fed its OWN output and never converges.
//
// The cycle, all of it production code:
//
//  1. A bead.updated event arrives for a row that carries dependencies (or the
//     Needs shorthand that derives them). applyBeadEventLocked cannot trust the
//     hook payload's dependency coverage, so it DELETES c.deps[id]
//     (caching_store_events.go, the !refreshedFromBacking arm).
//  2. The next reconcile compares absent cached deps against the deps derived
//     from the unchanged row. depsChanged reports a difference, so the merge
//     emits bead.updated — for a row where nothing changed.
//  3. cmd/gc/api_state.go feeds every recorded event back through
//     ApplyEvent, including events whose actor is cache-reconcile. That call
//     site already special-cases cache-reconcile to skip Poke(); it does not
//     skip ApplyEvent.
//  4. Which deletes c.deps[id] again. Goto 2.
//
// The emitted payload is byte-identical every cycle because nothing about the
// bead changed — matching the production capture, where 49 emissions of one
// bead carried exactly one distinct payload.
//
// The assertion is convergence, not silence-on-pass-one: the loop is allowed
// to take a cycle to settle. It is not allowed to run forever.
func TestReconcileQuiescence_SelfEmittedEventDoesNotSustainChurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  Bead
	}{
		{"needs shorthand", beadWith("ga-1", "open", func(b *Bead) {
			b.Needs = []string{"step-a", "step-b"}
		})},
		{"structured dependencies", beadWith("ga-1", "open", func(b *Bead) {
			b.Dependencies = []Dep{dep("ga-1", "ga-x")}
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := depsOf(tc.row)
			c, _ := newMergeHarnessStore(quiescentState([]Bead{tc.row}, deps))
			in := quiescentInputs([]Bead{tc.row}, deps, false)

			// Seed the loop the way production does: one bead.updated event
			// through the hook path, carrying the row unchanged.
			seed, err := json.Marshal(tc.row)
			if err != nil {
				t.Fatalf("marshal seed event: %v", err)
			}
			c.ApplyEvent("bead.updated", seed)

			const maxCycles = 5
			for cycle := 1; cycle <= maxCycles; cycle++ {
				c.mu.Lock()
				res := c.mergeSnapshotLocked(in.freshByID, in.confirmedClosed, in.depMap, in.useFreshDeps, in.startSeq, in.now)
				c.mu.Unlock()

				if len(res.notifications) == 0 {
					return // converged — the loop went quiet
				}
				t.Logf("cycle %d emitted %d notification(s)", cycle, len(res.notifications))

				// Replay the recorder → ApplyEvent hop from cmd/gc/api_state.go.
				for _, n := range res.notifications {
					payload, err := json.Marshal(n.bead)
					if err != nil {
						t.Fatalf("marshal emitted event: %v", err)
					}
					c.ApplyEvent(n.eventType, payload)
				}
			}
			t.Errorf("reconcile never converged: still emitting after %d cycles over an unchanged row — "+
				"every cycle republishes this bead forever", maxCycles)
		})
	}
}

func assertSilent(t *testing.T, res mergeImplResult, what string) {
	t.Helper()
	if len(res.notifications) == 0 {
		return
	}
	t.Errorf("%s produced %d notification(s), want 0", what, len(res.notifications))
	for _, n := range res.notifications {
		t.Errorf("  spurious: %s %s", n.eventType, n.bead.ID)
	}
}
