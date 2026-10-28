package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestRigControlDispatchOnARealSplitCity is W4/W5 on the real fixture: a rig
// dispatcher's primary ledger is the SQLite binding, a binding-resident tree is
// dispatched there, a pre-S4 rig tree is dispatched in place, and an imported
// rig reads only the binding.
func TestRigControlDispatchOnARealSplitCity(t *testing.T) {
	f := newS4RigFixture(t, true)
	rigPath, rig := f.rigPath("ra"), f.rigs["ra"]

	newCanceledTree := func(t *testing.T, store beads.Store) beads.Bead {
		t.Helper()
		root, err := store.Create(beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.CancelRequestedMetadataKey: "operator",
			beadmeta.RootStoreRefMetadataKey:    "rig:ra",
		}})
		if err != nil {
			t.Fatal(err)
		}
		return newControlBead(t, store, root.ID)
	}
	dispatch := func(t *testing.T, scope beads.Store, id string) {
		t.Helper()
		var stdout, stderr strings.Builder
		if err := runControlDispatcherWithStoreAndConfig(f.cityPath, rigPath, scope, id, f.cfg, &stdout, &stderr); err != nil {
			t.Fatalf("rig-scoped dispatch of %s: %v\n%s", id, err, stderr.String())
		}
	}

	inBinding := newCanceledTree(t, f.binding)
	dispatch(t, rig, inBinding.ID)
	if got := beadByID(t, f.binding, inBinding.ID); got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeCanceled {
		t.Errorf("binding-resident control = (%s, %q), want closed and canceled in the binding", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}

	inRig := newCanceledTree(t, rig)
	dispatch(t, rig, inRig.ID)
	if got := beadByID(t, rig, inRig.ID); got.Status != "closed" {
		t.Errorf("pre-S4 rig control status = %q, want closed in the rig store", got.Status)
	}
	if _, err := f.binding.Get(inRig.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("dispatching a rig-resident tree copied %s into the binding (err=%v)", inRig.ID, err)
	}

	if sources, _, err := controlReadyLegSources(rigPath, f.cityPath, f.cfg); err != nil || len(sources) != 2 || !sameStorePtr(sources[0], f.binding) {
		t.Errorf("ready legs for a rig awaiting import = %d (err=%v), want the binding then the rig store", len(sources), err)
	}
	f.markImported(t, "ra", true)
	if controlGraphRelicLeg(f.cityPath, rigPath) {
		t.Error("an imported rig keeps its relic leg")
	}
	if sources, owned, err := controlReadyLegSources(rigPath, f.cityPath, f.cfg); err != nil || len(sources) != 1 || !sameStorePtr(sources[0], f.binding) || len(owned) != 0 {
		t.Errorf("ready legs for an imported rig = %d leg(s), %d owned (err=%v), want the binding only, opening nothing", len(sources), len(owned), err)
	}
}
