package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/orders"
)

// S4b retires the relic-marked rig legs of the API's workflow and order
// readers once a rig is imported or proven clean: its store then holds only
// work, and none of these readers reads work.

func workflowStoreRefs(infos []workflowStoreInfo) map[string]bool {
	out := map[string]bool{}
	for _, info := range infos {
		out[info.ref] = true
	}
	return out
}

// TestWorkflowStoresDropARelicFreeRigLeg covers every reader built on
// workflowStores (convoys, runs, feed, workflow snapshot and delete, the
// event stream).
func TestWorkflowStoresDropARelicFreeRigLeg(t *testing.T) {
	state, _, _ := newS4APIState(t, true)
	if refs := workflowStoreRefs(workflowStores(state)); !refs["rig:myrig"] {
		t.Fatalf("a rig awaiting import lost its relic leg: %v", refs)
	}
	state.relicFree["myrig"] = true
	refs := workflowStoreRefs(workflowStores(state))
	if refs["rig:myrig"] {
		t.Errorf("a relic-free rig is still read as a workflow leg: %v", refs)
	}
	if !refs["graph:"+workflowCityScopeRef(state.CityName())] {
		t.Errorf("the binding leg is missing: %v", refs)
	}
}

// TestWorkflowStoresKeepEveryRigLegOnASingleStoreCity: no binding, no
// retirement, whatever the state reports.
func TestWorkflowStoresKeepEveryRigLegOnASingleStoreCity(t *testing.T) {
	state, _, _ := newS4APIState(t, false)
	state.relicFree["myrig"] = false
	if refs := workflowStoreRefs(workflowStores(state)); !refs["rig:myrig"] {
		t.Errorf("a single-store city lost its rig leg: %v", refs)
	}
}

// TestOrderStoresDropARelicFreeRigLeg: a rig order's history is read from the
// rig store only while the rig may hold pre-split tracking beads.
func TestOrderStoresDropARelicFreeRigLeg(t *testing.T) {
	state, _, _ := newS4APIState(t, true)
	order := orders.Order{Name: "nightly", Rig: "myrig"}
	infos, err := orderStoreInfosForState(state, order)
	if err != nil {
		t.Fatal(err)
	}
	if !workflowStoreRefs(infos)["rig:myrig"] {
		t.Fatalf("a rig awaiting import lost its order leg: %v", workflowStoreRefs(infos))
	}
	state.relicFree["myrig"] = true
	infos, err = orderStoreInfosForState(state, order)
	if err != nil {
		t.Fatal(err)
	}
	if workflowStoreRefs(infos)["rig:myrig"] {
		t.Errorf("a relic-free rig is still read for order history: %v", workflowStoreRefs(infos))
	}
}
