package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
)

// TestRigConvergenceLivesInTheBindingOnASplitCity is W1/W2: a rig's loops are
// created in the binding stamped with their scope, its wisps follow, every
// scope lists only its own loops, and a v1 poured formula is refused naming
// the supported alternative.
func TestRigConvergenceLivesInTheBindingOnASplitCity(t *testing.T) {
	f := newS4RigFixture(t, true)
	f.markImported(t, "rb", true) // rb is relic-free; ra is awaiting import
	scopes := f.runtime().buildConvergenceScopes()
	city, ra, rb := scopes[""], scopes["ra"], scopes["rb"]
	if city == nil || ra == nil || rb == nil {
		t.Fatalf("buildConvergenceScopes = %v, want city, ra and rb", scopes)
	}
	for name, scope := range map[string]*convergenceScope{"ra": ra, "rb": rb} {
		if !sameStorePtr(scope.store, f.binding) || !scope.adapter.relocated {
			t.Errorf("rig %s convergence scope is not served from the binding (relocated=%v)", name, scope.adapter.relocated)
		}
		if scope.storePath != f.rigPath(name) {
			t.Errorf("rig %s storePath = %q, want the rig root (gate commands run there)", name, scope.storePath)
		}
	}
	if ra.adapter.relic == nil {
		t.Error("rig ra awaits import but has no relic leg")
	}
	if rb.adapter.relic != nil {
		t.Error("rig rb is proven relic-free but still has a relic leg")
	}

	cityRoot := convergenceRootIn(t, city, "city loop")
	raRoot := convergenceRootIn(t, ra, "ra loop")
	rbRoot := convergenceRootIn(t, rb, "rb loop")
	for _, tc := range []struct {
		root beads.Bead
		want string
	}{{cityRoot, "city:s4-city"}, {raRoot, "rig:ra"}, {rbRoot, "rig:rb"}} {
		got, err := f.binding.Get(tc.root.ID)
		if err != nil {
			t.Fatalf("loop %s is not in the binding: %v", tc.root.ID, err)
		}
		if ref := got.Metadata[beadmeta.RootStoreRefMetadataKey]; ref != tc.want {
			t.Errorf("loop %s gc.root_store_ref = %q, want %q", tc.root.ID, ref, tc.want)
		}
		for name, rig := range f.rigs {
			if _, err := rig.Get(tc.root.ID); !errors.Is(err, beads.ErrNotFound) {
				t.Errorf("loop %s resolves in rig %s's work store (err=%v); rig stores hold only work", tc.root.ID, name, err)
			}
		}
	}

	// Isolation: one binding, three scopes, no scope sees another's loops.
	for name, tc := range map[string]struct {
		scope *convergenceScope
		want  string
	}{"city": {city, cityRoot.ID}, "ra": {ra, raRoot.ID}, "rb": {rb, rbRoot.ID}} {
		listed, err := tc.scope.adapter.listConvergence()
		if err != nil {
			t.Fatal(err)
		}
		if ids := controlLegIDs(listed); len(ids) != 1 || ids[0] != tc.want {
			t.Errorf("scope %s lists %v, want only %s", name, ids, tc.want)
		}
	}
	for _, id := range []string{cityRoot.ID, raRoot.ID, rbRoot.ID} {
		if err := f.binding.SetMetadata(id, convergence.FieldTarget, "worker"); err != nil {
			t.Fatal(err)
		}
		if err := f.binding.SetMetadata(id, convergence.FieldState, convergence.StateActive); err != nil {
			t.Fatal(err)
		}
	}
	for name, scope := range map[string]*convergenceScope{"city": city, "ra": ra, "rb": rb} {
		if n, err := scope.adapter.CountActiveConvergenceLoops("worker"); err != nil || n != 1 {
			t.Errorf("scope %s counts %d active loop(s) for worker (err=%v), want 1; concurrency limits must not cross scopes", name, n, err)
		}
	}

	// W2: a vapor wisp follows its root into the binding, carrying the scope.
	assertConvergencePours(t, ra, raRoot, convergenceVaporFormula, 1, f.binding, "rig ra")
	children, err := f.binding.List(beads.ListQuery{ParentID: raRoot.ID})
	if err != nil || len(children) != 1 {
		t.Fatalf("ra root children = %d (err=%v), want the one wisp", len(children), err)
	}
	if ref := children[0].Metadata[beadmeta.RootStoreRefMetadataKey]; ref != "rig:ra" {
		t.Errorf("rig wisp gc.root_store_ref = %q, want rig:ra", ref)
	}

	// D6: a v1 poured formula is refused in a rig scope, naming the alternative.
	before := countBeads(t, f.binding)
	_, err = ra.adapter.PourWisp(raRoot.ID, convergencePouredFormula, "converge:"+raRoot.ID+":iter:2", nil, "")
	if err == nil {
		t.Fatal("a rig scope on a split city poured a v1 poured (work-class) formula")
	}
	for _, want := range []string{"work-class", "v1 poured", `phase = "vapor"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "move the loop to a rig scope") {
		t.Errorf("refusal %q still names a rig scope as the remedy; on a split city a rig scope refuses too", err)
	}
	if after := countBeads(t, f.binding); after != before {
		t.Errorf("the refused pour wrote to the binding: %d -> %d", before, after)
	}
	if n := countBeads(t, f.rigs["ra"]); n != 0 {
		t.Errorf("rig ra's work store holds %d bead(s) after rig convergence; it must hold only work", n)
	}
}

// TestRigConvergenceKeepsAPreS4LoopRunningUntilImport is the relic leg: a loop
// the rig created in its own store before S4 is listed by its scope and every
// by-id write follows it there; once the rig is imported, its store is not read.
func TestRigConvergenceKeepsAPreS4LoopRunningUntilImport(t *testing.T) {
	f := newS4RigFixture(t, true)
	legacy, err := f.rigs["ra"].Create(beads.Bead{Title: "pre-S4 loop", Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatal(err)
	}
	ra := f.runtime().buildConvergenceScopes()["ra"]
	listed, err := ra.adapter.listConvergence()
	if err != nil {
		t.Fatal(err)
	}
	if !containsID(listed, legacy.ID) {
		t.Fatalf("rig ra lists %v, missing its pre-S4 loop %s", controlLegIDs(listed), legacy.ID)
	}
	if err := ra.adapter.SetMetadata(legacy.ID, convergence.FieldState, convergence.StateActive); err != nil {
		t.Fatalf("writing the pre-S4 loop through the adapter: %v", err)
	}
	if got := beadByID(t, f.rigs["ra"], legacy.ID); got.Metadata[convergence.FieldState] != convergence.StateActive {
		t.Error("the write on a rig-resident loop did not land in the rig store that holds it")
	}
	assertConvergencePours(t, ra, legacy, convergenceVaporFormula, 1, f.rigs["ra"], "rig ra (relic)")
	if err := ra.adapter.CloseBead(legacy.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if got := beadByID(t, f.rigs["ra"], legacy.ID); got.Status != "closed" {
		t.Errorf("relic loop status = %q, want closed in the rig store", got.Status)
	}

	// After the import the rig store is never read.
	f.markImported(t, "ra", true)
	f.rigs["ra"] = failingReadStore{Store: f.rigs["ra"]}
	ra = f.runtime().buildConvergenceScopes()["ra"]
	if ra.adapter.relic != nil {
		t.Fatal("an imported rig still has a relic leg")
	}
	if _, err := ra.adapter.listConvergence(); err != nil {
		t.Errorf("listing an imported rig's loops read its work store: %v", err)
	}
}

// TestSingleStoreConvergenceIsUnchanged is the single-store control: every rig
// keeps its own store, relocated=false, no stamp, and a v1 poured formula
// still pours.
func TestSingleStoreConvergenceIsUnchanged(t *testing.T) {
	f := newS4RigFixture(t, false)
	ra := f.runtime().buildConvergenceScopes()["ra"]
	if !sameStorePtr(ra.store, f.rigs["ra"]) || ra.adapter.relocated || ra.adapter.scopeRef != "" || ra.adapter.relic != nil {
		t.Fatalf("single-store rig scope = (store %T, relocated %v, scopeRef %q, relic %v), want its own store and nothing else",
			ra.store, ra.adapter.relocated, ra.adapter.scopeRef, ra.adapter.relic)
	}
	root := convergenceRootIn(t, ra, "ra loop")
	if _, ok := root.Metadata[beadmeta.RootStoreRefMetadataKey]; ok {
		t.Errorf("a single-store convergence root carries gc.root_store_ref=%q; no new metadata on single-store cities", root.Metadata[beadmeta.RootStoreRefMetadataKey])
	}
	assertConvergencePours(t, ra, root, convergencePouredFormula, 1, f.rigs["ra"], "rig ra")
}

// TestConvergeCLIReadsTheScopeTheControllerWrites is the reader half of W1:
// `gc converge` lists and gets a rig scope from the binding (scope-filtered),
// unions the relic leg until the rig is imported, and reads nothing else after.
func TestConvergeCLIReadsTheScopeTheControllerWrites(t *testing.T) {
	f := newS4RigFixture(t, true)
	scopes := f.runtime().buildConvergenceScopes()
	raRoot := convergenceRootIn(t, scopes["ra"], "ra loop")
	convergenceRootIn(t, scopes["rb"], "rb loop")
	convergenceRootIn(t, scopes[""], "city loop")
	legacy, err := f.rigs["ra"].Create(beads.Bead{Title: "pre-S4 loop", Type: "convergence"})
	if err != nil {
		t.Fatal(err)
	}

	reader := convergeScopeStore(f.cityPath, "ra", f.rigPath("ra"), f.rigs["ra"])
	listed, err := reader.List(beads.ListQuery{Type: "convergence"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := controlLegIDs(listed); len(ids) != 2 || !containsID(listed, raRoot.ID) || !containsID(listed, legacy.ID) {
		t.Errorf("gc converge list --rig ra = %v, want exactly %s (binding) and %s (relic)", ids, raRoot.ID, legacy.ID)
	}
	for _, id := range []string{raRoot.ID, legacy.ID} {
		if _, err := reader.Get(id); err != nil {
			t.Errorf("gc converge status %s: %v", id, err)
		}
	}

	f.markImported(t, "ra", true)
	reader = convergeScopeStore(f.cityPath, "ra", f.rigPath("ra"), failingReadStore{Store: f.rigs["ra"]})
	listed, err = reader.List(beads.ListQuery{Type: "convergence"})
	if err != nil {
		t.Fatalf("an imported rig's converge list read its work store: %v", err)
	}
	if ids := controlLegIDs(listed); len(ids) != 1 || ids[0] != raRoot.ID {
		t.Errorf("gc converge list --rig ra after import = %v, want only %s", ids, raRoot.ID)
	}

	single := newS4RigFixture(t, false)
	if got := convergeScopeStore(single.cityPath, "ra", single.rigPath("ra"), single.rigs["ra"]); got != convergeScopeReader(single.rigs["ra"]) {
		t.Errorf("single-store converge reader = %T, want the rig store itself", got)
	}
}
