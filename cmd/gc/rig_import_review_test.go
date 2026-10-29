package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// Review of #8486: the importer's data-safety must-fixes, each on real stores
// (a real SQLite binding, real file rig stores, and a real bd rig where the
// reviewer used one).

// crashOnceAt makes the import stop at step on its first pass only.
func crashOnceAt(t *testing.T, step string) {
	t.Helper()
	crashed := false
	prev := rigImportCrashPoint
	rigImportCrashPoint = func(s string) error {
		if s == step && !crashed {
			crashed = true
			return errors.New("killed at " + s)
		}
		return nil
	}
	t.Cleanup(func() { rigImportCrashPoint = prev })
}

func twoStepRig(t *testing.T) *rigImportFixture {
	t.Helper()
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "open", nil), graphRow("gp-b", "gp-a", "open", nil)},
		[]beads.Dep{{IssueID: "gp-b", DependsOnID: "gp-a", Type: "blocks"}})
	return f
}

// MF1: a crash after the rows and before their edges leaves rows the binding
// would serve as ready. The in-flight record holds boot and every CLI route
// (storageBootGate) until the import finishes.
func TestReviewHalfFinishedCopyIsNeverServed(t *testing.T) {
	f := twoStepRig(t)
	crashOnceAt(t, "rows")
	if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "killed at rows") {
		t.Fatalf("the crash did not stop the import: %s", stderr)
	}
	routes, err := storageBootGate(f.request.CityPath, f.cfg, "gc start", nil, io.Discard)
	if err == nil {
		_ = routes.close()
		t.Fatal("boot served a binding holding a half-finished rig copy (rows without their edges)")
	}
	if !strings.Contains(err.Error(), "in flight") || !strings.Contains(err.Error(), rigImportInstruction("packs")) {
		t.Errorf("the refusal does not say the import is in flight and name the command: %v", err)
	}
	if f.relicFree("packs") {
		t.Error("a rig with an import in flight reads as relic-free")
	}
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("the resume = %d: %s %s", code, o, e)
	}
	routes, err = storageBootGate(f.request.CityPath, f.cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("boot refuses a finished import: %v", err)
	}
	_ = routes.close()
}

// MF1: a bd CLI rig cannot report edge payloads. The plan refuses before
// anything is written, rather than copying every row and then failing at the
// first edge, forever.
func TestReviewBdRigWithEdgesRefusesBeforeAnyWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("real bd rig")
	}
	rig, dir := newRealBdRigStore(t)
	root, err := rig.Create(beads.Bead{Title: "a pre-S4 workflow", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow", beadmeta.RootBeadIDMetadataKey: "self"}})
	if err != nil {
		t.Fatal(err)
	}
	step, err := rig.Create(beads.Bead{Title: "a step", Type: "task", Metadata: beads.StringMap{"gc.kind": "task", beadmeta.RootBeadIDMetadataKey: root.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.DepAdd(step.ID, root.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	f := newRigImportFixture(t, config.Rig{Name: "bdrig", Prefix: "tst", Path: dir})
	f.fixed["bdrig"] = rig
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "cannot report the payload") {
		t.Fatalf("a bd rig with an edge was not refused up front: %d %s", code, stderr)
	}
	if n := len(rigTestList(t, f.binding())); n != 0 {
		t.Errorf("the refused import wrote %d binding row(s)", n)
	}
	if fileExists(f.loc("bdrig").InFlightPath()) || fileExists(f.loc("bdrig").MarkerPath()) {
		t.Error("the refused import left a record")
	}
}

// MF1: a resume verifies the interrupted attempt's copies instead of deleting
// and re-copying them blindly: a binding copy that was written to since is
// named, and kept.
func TestReviewResumeKeepsAnUpdatedBindingCopy(t *testing.T) {
	f := twoStepRig(t)
	crashOnceAt(t, "rows")
	_, _, _ = f.importRigs()
	binding, err := openInfraDestination(f.target)
	if err != nil {
		t.Fatal(err)
	}
	title := "updated in the binding"
	if err := binding.Update("gp-a", beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "gp-a") {
		t.Fatalf("the resume did not refuse over the changed binding copy: %d %s", code, stderr)
	}
	if got, err := f.binding().Get("gp-a"); err != nil || got.Title != title {
		t.Errorf("the resume discarded the binding copy's update: %q %v", got.Title, err)
	}
}

// MF2: prepare never deletes a stamped binding row the rig no longer holds as
// the same bead: it may be the binding's only copy.
func TestReviewPrepareKeepsTheBindingsOnlyCopy(t *testing.T) {
	f := twoStepRig(t)
	crashOnceAt(t, "rows")
	_, _, _ = f.importRigs()
	if err := f.rig("packs").Delete("gp-b"); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "gp-b") || !strings.Contains(stderr, "only copy") {
		t.Fatalf("prepare did not refuse over the binding's only copy: %d %s", code, stderr)
	}
	if _, err := f.binding().Get("gp-b"); err != nil {
		t.Errorf("prepare deleted the binding's only copy of gp-b: %v", err)
	}
}

// MF2: a cleared note with no marker means rows may already be gone from the
// rig; the import refuses. The marker-without-manifest remedy never says to
// remove the marker.
func TestReviewClearedNoteWithoutMarkerRefuses(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	loc := f.loc("packs")
	if err := writeInfraClearedNoteAt(loc.NotePath(), infraClearedNote{Complete: true}); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "no import marker") {
		t.Fatalf("a cleared note without a marker did not refuse: %d %s", code, stderr)
	}
	if n := len(rigTestList(t, f.binding())); n != 0 {
		t.Errorf("the refused import wrote %d binding row(s)", n)
	}
	_ = os.Remove(loc.NotePath())
	if err := os.WriteFile(loc.MarkerPath(), rigTestRecord(f.request.CityPath, f.rigPaths["packs"]), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := loc.readManifest()
	if err == nil || strings.Contains(err.Error(), "or remove the marker") || !strings.Contains(err.Error(), "Do not remove the marker") {
		t.Errorf("the marker-without-manifest remedy = %v", err)
	}
}

// MF3: a resumed clear re-proves every row against the binding first: a rig
// row whose binding copy is gone is named and not deleted.
func TestReviewResumedClearKeepsRowsTheBindingLacks(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil), graphRow("gp-b", "gp-a", "closed", nil)}, nil)
	crashOnceAt(t, "pending")
	_, _, _ = f.importRigs()
	binding, err := openInfraDestination(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.Delete("gp-a"); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "gp-a") || !strings.Contains(stderr, "only copy") {
		t.Fatalf("the resumed clear did not refuse over a row the binding lacks: %d %s", code, stderr)
	}
	if _, err := f.rig("packs").Get("gp-a"); err != nil {
		t.Errorf("the resumed clear deleted gp-a, whose binding copy is gone: %v", err)
	}
}

// MF4: an open blocker held in another rig's store is encoded as an S3b gate,
// never dropped; with a store unreadable the import refuses; a blocker no
// store holds is dropped as proven gone.
func TestReviewBlockerHeldInAnotherStoreIsGated(t *testing.T) {
	t.Run("held by another rig: gate", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "other", Prefix: "go"})
		f.seed("other", []beads.Bead{workRow("go-x", "open")}, nil)
		f.seed("packs", []beads.Bead{graphRow("gp-d", "gp-d", "open", nil)}, []beads.Dep{{IssueID: "gp-d", DependsOnID: "go-x", Type: "blocks"}})
		if code, o, e := f.importRigs("packs"); code != 0 {
			t.Fatalf("import = %d: %s %s", code, o, e)
		}
		binding := f.binding()
		gates, _ := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if len(gates) != 1 || gates[0].Metadata[beadmeta.CrossStoreBlockerMetadataKey] != "go-x" {
			t.Fatalf("gates = %+v, want one gate holding gp-d on go-x", gates)
		}
		deps, _ := binding.DepList("gp-d", "down")
		if len(deps) != 1 || deps[0].DependsOnID != gates[0].ID {
			t.Errorf("gp-d's edges = %v, want its gate", deps)
		}
		assertNoCrossStoreEdge(t, binding)
	})
	t.Run("a store unreadable: refuse", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "other", Prefix: "go"})
		f.open["other"] = func() (beads.Store, error) { return beads.NewMemStore(), errors.New("dolt server unreachable") }
		f.seed("packs", []beads.Bead{graphRow("gp-d", "gp-d", "open", nil)}, []beads.Dep{{IssueID: "gp-d", DependsOnID: "go-x", Type: "blocks"}})
		code, _, stderr := f.importRigs("packs")
		if code == 0 || !strings.Contains(stderr, "go-x") || !strings.Contains(stderr, "could not be read") {
			t.Fatalf("an unprovable blocker was not refused: %d %s", code, stderr)
		}
	})
	t.Run("absent from every store: drop", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "other", Prefix: "go"})
		f.seed("packs", []beads.Bead{graphRow("gp-d", "gp-d", "open", nil)}, []beads.Dep{{IssueID: "gp-d", DependsOnID: "go-gone", Type: "blocks"}})
		if code, o, e := f.importRigs("packs"); code != 0 {
			t.Fatalf("import = %d: %s %s", code, o, e)
		}
		d, _ := f.binding().Get("gp-d")
		if !strings.Contains(d.Metadata[beadmeta.MigratedCrossEdgesMetadataKey], "absent from every store") {
			t.Errorf("the dropped edge's record = %q", d.Metadata[beadmeta.MigratedCrossEdgesMetadataKey])
		}
	})
}

// MF5: the clear removes the boundary edges before it deletes, so bd's delete
// finds no surviving neighbor and rewrites no work row's text to
// "[deleted:<id>]".
func TestReviewClearLeavesSurvivingWorkTextUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("real bd rig")
	}
	rig, dir := newRealBdRigStore(t)
	d, err := rig.Create(beads.Bead{Title: "a drained step", Type: "task", Metadata: beads.StringMap{"gc.kind": "task", beadmeta.RootBeadIDMetadataKey: "self"}})
	if err != nil {
		t.Fatal(err)
	}
	desc := "drain unit for " + d.ID + "; see " + d.ID + " for the run"
	w, err := rig.Create(beads.Bead{Title: "a drain-unit convoy", Type: "task", Description: desc})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.DepAdd(w.ID, d.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	// And a work row the moved row points AT (D -> W2): bd treats both ends
	// of an edge as neighbors.
	desc2 := "context for " + d.ID
	w2, err := rig.Create(beads.Bead{Title: "a note", Type: "task", Description: desc2})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.DepAdd(d.ID, w2.ID, "related"); err != nil {
		t.Fatal(err)
	}
	if err := rig.Close(d.ID); err != nil {
		t.Fatal(err)
	}
	if err := rig.Close(w.ID); err != nil {
		t.Fatal(err)
	}
	f := newRigImportFixture(t, config.Rig{Name: "bdrig", Prefix: "tst", Path: dir})
	f.fixed["bdrig"] = rig
	prevHash := rigImportHashConfigOpener
	rigImportHashConfigOpener = func(string, *config.City, config.Rig) (rigHashLengthConfig, error) { return rig, nil }
	t.Cleanup(func() { rigImportHashConfigOpener = prevHash })
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("import = %d: %s %s", code, o, e)
	}
	got, err := rig.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != desc {
		t.Errorf("the clear rewrote the surviving work row's text:\n got %q\nwant %q", got.Description, desc)
	}
	if got2, err := rig.Get(w2.ID); err != nil || got2.Description != desc2 {
		t.Errorf("the clear rewrote the text of the work row the moved row pointed at: %q %v", got2.Description, err)
	}
}

// MF8: a rig store another registered city also uses is refused, by scope
// root and by Dolt database; rows scoped to another rig, and open rows
// another city's agent claims, are refused too.
func TestReviewSharedRigStoreRefuses(t *testing.T) {
	seedPacks := func(f *rigImportFixture) {
		f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	}
	t.Run("same scope root in another city", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
		seedPacks(f)
		other := t.TempDir()
		toml := fmt.Sprintf("[workspace]\nname = \"other\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"theirs\"\npath = %q\n", f.rigPaths["packs"])
		if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		f.otherCities = []string{other}
		code, _, stderr := f.importRigs()
		if code == 0 || !strings.Contains(stderr, "also city") {
			t.Fatalf("a shared rig store was imported: %d %s", code, stderr)
		}
		if n := len(rigTestList(t, f.binding())); n != 0 {
			t.Errorf("the refused import wrote %d binding row(s)", n)
		}
	})
	t.Run("same Dolt database in another city", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
		seedPacks(f)
		other := t.TempDir()
		toml := fmt.Sprintf("[workspace]\nname = \"other\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"theirs\"\npath = %q\n", filepath.Join(other, "elsewhere"))
		if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		f.otherCities = []string{other}
		prev := rigImportScopeDoltTarget
		rigImportScopeDoltTarget = func(string, string) (string, bool) { return "127.0.0.1:3399/packs", true }
		t.Cleanup(func() { rigImportScopeDoltTarget = prev })
		if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "Dolt database") {
			t.Fatalf("a rig on another city's Dolt database was imported: %d %s", code, stderr)
		}
	})
	t.Run("a row scoped to another rig", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
		f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:other"})}, nil)
		if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "rig:other") {
			t.Fatalf("a row scoped to another rig was imported: %d %s", code, stderr)
		}
	})
	t.Run("an open row another city claims", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
		claimed := graphRow("gp-a", "gp-a", "open", nil)
		claimed.Assignee = "qcore/gc.run-operator-36"
		f.seed("packs", []beads.Bead{claimed}, nil)
		if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "claimed by qcore/gc.run-operator-36") {
			t.Fatalf("a row another city claims was imported: %d %s", code, stderr)
		}
	})
}

// Should-fix: a writer that changes a rig row between the copy and the clear
// stops the clear; the row is not deleted.
func TestReviewClearRefusesARowChangedSinceTheCensus(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	prev := rigImportCrashPoint
	rigImportCrashPoint = func(step string) error {
		if step == "reserved" {
			title := "written by a straggler"
			return f.rig("packs").Update("gp-a", beads.UpdateOpts{Title: &title})
		}
		return nil
	}
	t.Cleanup(func() { rigImportCrashPoint = prev })
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "changed after the census") {
		t.Fatalf("a row changed under the clear was not refused: %d %s", code, stderr)
	}
	if got, err := f.rig("packs").Get("gp-a"); err != nil || got.Title != "written by a straggler" {
		t.Errorf("the clear removed or reverted the changed row: %+v %v", got, err)
	}
}

// Should-fix: max_hash_length is raised when the floor is above it.
func TestReviewHashFloorRaisesMaxHashLength(t *testing.T) {
	conf := &fakeRigHashConfig{values: map[string]string{bdMaxHashLengthKey: "5"}}
	floor, err := rigImportReserveIDs(conf, &infraCopyManifest{IDs: map[string]bool{"gp-k0nvi": true}})
	if err != nil || floor != 6 {
		t.Fatalf("floor = %d %v, want 6", floor, err)
	}
	if conf.values[bdMinHashLengthKey] != "6" || conf.values[bdMaxHashLengthKey] != "6" {
		t.Errorf("config = %v, want min and max 6", conf.values)
	}
}

// Should-fix: an exec provider is refused through the production config
// opener before anything is written (not after the marker).
func TestReviewExecProviderRefusesBeforeAnyWrite(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	rigImportHashConfigOpener = defaultRigImportHashConfigOpener
	if err := os.WriteFile(filepath.Join(f.request.CityPath, "city.toml"), []byte("[workspace]\nname = \"s4b\"\n\n[beads]\nprovider = \"exec:/bin/true\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "exec:") {
		t.Fatalf("an exec provider rig was not refused: %d %s", code, stderr)
	}
	if fileExists(f.loc("packs").MarkerPath()) || len(rigTestList(t, f.binding())) != 0 {
		t.Error("the exec refusal came after writes")
	}
}

// Should-fix: --abandon-rig-store records the clear complete only once the
// binding is proven to hold every row the manifest records.
func TestReviewAbandonRigStore(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil), graphRow("gp-b", "gp-a", "closed", nil)}, nil)
	crashOnceAt(t, "pending")
	_, _, _ = f.importRigs()
	if err := rigBootRefusal(f.request.CityPath, f.cfg, f.target); err == nil {
		t.Fatal("precondition: boot does not refuse the pending clear")
	}
	binding, err := openInfraDestination(f.target)
	if err != nil {
		t.Fatal(err)
	}
	gone, _ := binding.Get("gp-b")
	if err := binding.Delete("gp-b"); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	var stdout, stderr bytes.Buffer
	if code := doStorageAbandonRigStore(context.Background(), f.request, "packs", &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "gp-b") {
		t.Fatalf("abandon proceeded with a row the binding lacks: %d %s", code, stderr.String())
	}
	binding, err = openInfraDestination(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binding.(beads.ForeignIDCreator).CreateWithForeignID(gone); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	stderr.Reset()
	if code := doStorageAbandonRigStore(context.Background(), f.request, "packs", &stdout, &stderr); code != 0 {
		t.Fatalf("abandon = %d: %s", code, stderr.String())
	}
	if err := rigBootRefusal(f.request.CityPath, f.cfg, f.target); err != nil {
		t.Errorf("boot still refuses after the abandon: %v", err)
	}
}

// Should-fix: records speak for the store they name; a new store under a
// reused rig name is not relic-free, and the import refuses until the old
// records are moved aside.
func TestReviewReusedRigNameIsNotRelicFree(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("import = %d: %s %s", code, o, e)
	}
	if !f.relicFree("packs") {
		t.Fatal("precondition: the imported rig is not relic-free")
	}
	newPath := t.TempDir()
	f.cfg.Rigs[0].Path = newPath
	f.rigPaths["packs"] = newPath
	if f.relicFree("packs") {
		t.Error("a new store under a reused rig name reads as relic-free")
	}
	if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "reused") {
		t.Errorf("the import ran over another store's records: %d %s", code, stderr)
	}
}

// The same for a clean record: it speaks only for the store it censused.
func TestReviewReusedRigNameCleanRecordIsNotRelicFree(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "quiet", Prefix: "gq"})
	f.seed("quiet", []beads.Bead{workRow("gq-1", "open")}, nil)
	if code, o, e := f.importRigs(); code != 0 || !f.relicFree("quiet") {
		t.Fatalf("precondition: clean record = %d %s %s", code, o, e)
	}
	newPath := t.TempDir()
	f.cfg.Rigs[0].Path = newPath
	f.rigPaths["quiet"] = newPath
	if f.relicFree("quiet") {
		t.Error("a new store under a reused rig name reads as relic-free on the old store's clean record")
	}
}

// Should-fix: preflight words a rig refusal as the rig import's, not the
// city migration's.
func TestReviewPreflightWordsARigRefusalAsTheRigImports(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-d", "gp-d", "open", nil), workRow("gp-w", "open")}, []beads.Dep{{IssueID: "gp-w", DependsOnID: "gp-d", Type: "tracks"}})
	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(f.request, &stdout, &stderr); code == 0 {
		t.Fatalf("preflight cleared an uncodable rig edge: %s", stdout.String())
	}
	if out := stdout.String(); strings.Contains(out, "the migration would refuse") || !strings.Contains(out, "would refuse the rig(s) named above") {
		t.Errorf("preflight wording = %q", out)
	}
}

// Should-fix: the importer through the production open path: a file-provider
// city whose rig store the storage-admin opener opens, no seam.
func TestReviewImportThroughTheProductionOpenPath(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil), workRow("gp-w", "open")}, nil)
	if err := os.WriteFile(filepath.Join(f.request.CityPath, "city.toml"), []byte("[workspace]\nname = \"s4b\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := rigImportStoreOpener
	rigImportStoreOpener = func(cityPath string, rig config.Rig) (beads.Store, error) {
		return openPhysicalScopeStore(rig.Path, cityPath)
	}
	t.Cleanup(func() { rigImportStoreOpener = prev })
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("import through openStoreAtForCity = %d: %s %s", code, o, e)
	}
	if got := rigTestIDs(rigTestList(t, f.rig("packs"))); strings.Join(got, ",") != "gp-w" {
		t.Errorf("the rig holds %v, want only gp-w", got)
	}
	if _, err := f.binding().Get("gp-a"); err != nil {
		t.Errorf("the binding does not hold gp-a: %v", err)
	}
}
