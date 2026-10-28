package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// TestSlingResolvesARigPrefixedBindingBeadByResidence is gc sling's by-prefix
// read: the prefix picks the scope, residence picks the store.
func TestSlingResolvesARigPrefixedBindingBeadByResidence(t *testing.T) {
	f := newS4RigFixture(t, true)
	writeS4FileCityTOML(t, f.cityPath)
	// The raw SQLite engine the binding is served by accepts a foreign-prefix
	// id, which is how an imported rig row arrives (S4b keeps ids).
	raw, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.(interface{ CloseStore() error }).CloseStore() })
	seedCLIStorageRoutes(t, f.cityPath, splitEnvRoutes(raw))
	creator, ok := raw.(beads.ForeignIDCreator)
	if !ok {
		t.Fatal("the SQLite engine does not accept foreign ids")
	}
	imported, cerr := creator.CreateWithForeignID(beads.Bead{ID: "ra-imported1", Title: "imported rig row", Type: "task", Metadata: map[string]string{
		beadmeta.RootStoreRefMetadataKey: "rig:ra",
	}})
	if cerr != nil {
		t.Fatalf("seeding a rig-prefixed row in the binding: %v", cerr)
	}
	got, perr := probeExistingSlingSourceBead(f.cfg, f.cityPath, imported.ID)
	if perr != nil {
		t.Fatal(perr)
	}
	if !got.exists || got.storeDir != resolveStoreScopeRoot(f.cityPath, f.rigPath("ra")) || got.prefix != "ra" {
		t.Errorf("probe = %+v, want exists in the binding with the scope still rig ra", got)
	}

	single := newS4RigFixture(t, false)
	writeS4FileCityTOML(t, single.cityPath)
	got, perr = probeExistingSlingSourceBead(single.cfg, single.cityPath, "ra-imported1")
	if perr != nil {
		t.Fatal(perr)
	}
	if got.exists {
		t.Error("a single-store city found a bead that only a binding could hold")
	}
}

// writeS4FileCityTOML declares the file bead provider, so opening a scope
// store in a sling probe never starts a Dolt server.
func writeS4FileCityTOML(t *testing.T, cityPath string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"s4-city\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
}

// TestCompletionBackstopSweepsOnlyRigsThatMayHoldRoots is
// CompletionBackstop.ensureStoreRoots' store list: on a split city a rig's
// workflow roots live in the binding, so a rig store is swept only while it may
// still hold pre-S4 roots. A single-store city sweeps every rig as before.
func TestCompletionBackstopSweepsOnlyRigsThatMayHoldRoots(t *testing.T) {
	f := newS4RigFixture(t, true)
	f.markImported(t, "rb", true)
	cs := &controllerState{
		cfg:           f.cfg,
		cityPath:      f.cityPath,
		cityBeadStore: f.work,
		beadStores:    map[string]beads.Store{"ra": f.rigs["ra"], "rb": f.rigs["rb"]},
		eventProv:     events.NewFake(),
		storageRoutes: f.routes,
	}
	_, fan := cs.completionReconcileInputs(reconcilePlane)
	var sawRA, sawRB bool
	for _, leg := range fan {
		sawRA = sawRA || sameStorePtr(leg.Store, f.rigs["ra"])
		sawRB = sawRB || sameStorePtr(leg.Store, f.rigs["rb"])
	}
	if !sawRA {
		t.Error("the backstop dropped rig ra, which awaits import and may still hold pre-S4 roots")
	}
	if sawRB {
		t.Error("the backstop still sweeps rig rb, which is proven relic-free; its store holds only work")
	}
	if !cs.RigGraphRelicFree("rb") || cs.RigGraphRelicFree("ra") {
		t.Errorf("RigGraphRelicFree(ra, rb) = (%v, %v), want (false, true)", cs.RigGraphRelicFree("ra"), cs.RigGraphRelicFree("rb"))
	}

	single := newS4RigFixture(t, false)
	scs := &controllerState{
		cfg:           single.cfg,
		cityPath:      single.cityPath,
		cityBeadStore: single.work,
		beadStores:    map[string]beads.Store{"ra": single.rigs["ra"], "rb": single.rigs["rb"]},
		eventProv:     events.NewFake(),
	}
	if _, fan := scs.completionReconcileInputs(reconcilePlane); len(fan) != 3 {
		t.Errorf("single-store backstop fans out to %d store(s), want 3 (city + both rigs)", len(fan))
	}
	if scs.RigGraphRelicFree("ra") {
		t.Error("a single-store city reports a rig relic-free to the API; there the rig store holds the rig's graph rows")
	}
}
