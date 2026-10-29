package main

// `gc storage migrate --from-rigs`: move one rig's graph and infrastructure
// rows into the city binding (S4b).
//
// Per rig, independently, and resumable at every step:
//
//  1. Census: list the rig store (both tiers, closed included) and read its
//     edges once; plan the import (rig_import_plan.go). A refusal stops this
//     rig only and writes nothing.
//  2. Prepare: delete binding rows this rig's interrupted attempt stamped
//     (gc.infra_migrated_from=rig:<rig>) and its manifest does not record.
//  3. Copy rows with their ids, the import stamp on every row (wisps and
//     steps included) and gc.root_store_ref=rig:<rig> on any row without
//     one; copy edges inside the moved set; mint S3b gates.
//  4. Verify against a reopened binding: the rig's stamped population equals
//     the manifest's rows plus this run's, field-equal, with the planned
//     edges.
//  5. Write the manifest (S1's (id, created_at) format, plus #gc:scope,
//     #gc:scope-annotated and #gc:gate), then the marker.
//  6. Raise bd's min_hash_length in the rig store, so a short id minted
//     after the clear can never re-mint an imported one.
//  7. Back up, prove the backup, mark the cleared note pending, delete the
//     rig copies in batches, mark the note complete. No edge is re-added.
//
// A completed run on an unchanged rig prints "nothing to import". A rig with
// no infrastructure row at all gets a clean record. Both retire the rig's
// relic legs (rigGraphRelicFree).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// storageFromRigsFlag names the migrate verb's third source: the city's rig
// stores.
const storageFromRigsFlag = "from-rigs"

// storageRigFlag narrows --from-rigs to named rigs.
const storageRigFlag = "rig"

// rigImportInstruction is the command that imports one rig.
func rigImportInstruction(rig string) string {
	return "gc storage migrate --" + storageFromRigsFlag + " --" + storageRigFlag + " " + rig + " --" + storageFleetStoppedFlag
}

// rigImportAllInstruction is the command that imports every rig.
func rigImportAllInstruction() string {
	return "gc storage migrate --" + storageFromRigsFlag + " --" + storageFleetStoppedFlag
}

// rigImportDeleteChunk bounds one batched delete. The design measured about
// 30 ms per row for one Dolt commit per row and 1.0 s for 1,000 rows deleted
// in chunks of 250.
const rigImportDeleteChunk = 250

// rigImportLocation is where one rig's import records live: the manifest,
// marker, clean record and backup beside the city's infra.migrated at the
// binding root, and the cleared note in the city's .gc.
type rigImportLocation struct {
	Rig         string
	BindingRoot string
	CityPath    string
}

func newRigImportLocation(target infraBindingTarget, cityPath, rig string) rigImportLocation {
	return rigImportLocation{Rig: rig, BindingRoot: target.Root, CityPath: cityPath}
}

func (l rigImportLocation) MarkerPath() string {
	return filepath.Join(l.BindingRoot, rigGraphImportMarkerName(l.Rig))
}

func (l rigImportLocation) ManifestPath() string {
	return filepath.Join(l.BindingRoot, rigGraphImportMarkerName(l.Rig)+".beads")
}

func (l rigImportLocation) CleanPath() string {
	return filepath.Join(l.BindingRoot, rigGraphCleanMarkerName(l.Rig))
}

func (l rigImportLocation) BackupPath() string {
	return filepath.Join(l.BindingRoot, "infra.rig."+l.Rig+".retained-source.jsonl")
}

func (l rigImportLocation) NotePath() string { return rigGraphClearedNotePath(l.CityPath, l.Rig) }

// readManifest reads the rig's manifest. It is trusted only behind the
// marker: before it, an interrupted attempt is re-copied.
func (l rigImportLocation) readManifest() (*infraCopyManifest, bool, error) {
	if !fileExists(l.MarkerPath()) {
		return nil, false, nil
	}
	m, recorded, err := readInfraCopyManifestRecordAt(l.ManifestPath(), l.MarkerPath())
	if err != nil {
		return nil, true, err
	}
	if !recorded {
		return nil, true, fmt.Errorf("rig %s carries an import marker %s with no manifest %s, so nothing says which rows the import carried. Restore the manifest, or remove the marker to import the rig again", l.Rig, l.MarkerPath(), l.ManifestPath())
	}
	return &m, true, nil
}

// writeRecord writes a one-line record file atomically.
func writeRigImportRecord(path, line string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := fmt.Fprintln(tmp, line); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := infraMigrationRename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	syncInfraDir(dir)
	return nil
}

// rigImportCrashPoint is a test seam: called at every step boundary of an
// import with the step's name; an error stops the import there, as a crash
// would.
var rigImportCrashPoint = func(string) error { return nil }

// rigImportStoreOpener opens one rig's store for the importer and the census.
// Tests override it with real file, bd or Dolt stores.
var rigImportStoreOpener = func(cityPath string, rig config.Rig) (beads.Store, error) {
	return openStorageScopeStore(rig.Path, cityPath)
}

// rigImportBindingOpener opens the city binding read-write for the importer.
var rigImportBindingOpener = openInfraDestination

// rigHashLengthConfig is the bd config surface the id reservation needs.
type rigHashLengthConfig interface {
	ConfigGet(key string) (string, error)
	ConfigSet(key, value string) error
}

// rigImportHashConfigOpener returns the rig's bd config surface, or nil for a
// rig whose store mints from a monotonic sequence (the file store), which a
// clear cannot rewind and so needs no reservation.
var rigImportHashConfigOpener = func(cityPath string, cfg *config.City, rig config.Rig) (rigHashLengthConfig, error) {
	root := resolveStoreScopeRoot(cityPath, rig.Path)
	switch provider := authoritativeBeadsProviderForScope(root, cityPath); {
	case provider == "file":
		return nil, nil
	case strings.HasPrefix(provider, "exec:"):
		return nil, fmt.Errorf("rig %s is served by provider %s, whose id minting this build cannot configure", rig.Name, provider)
	}
	return bdStoreForRig(root, cityPath, cfg), nil
}

const bdMinHashLengthKey = "min_hash_length"

// bdMaxHashLength is bd's default ceiling for adaptive ids.
const bdMaxHashLength = 8

// rigImportEnv is what one run of the importer needs.
type rigImportEnv struct {
	CityPath string
	CityName string
	Cfg      *config.City
	Target   infraBindingTarget
	Stdout   io.Writer
	Stderr   io.Writer
	// Started is the run's start, recorded in the marker.
	Started time.Time
}

// rigImportOutcome names what an import did for one rig.
type rigImportOutcome string

const (
	rigImportImported rigImportOutcome = "imported"
	rigImportClean    rigImportOutcome = "clean"
	rigImportNothing  rigImportOutcome = "nothing to import"
	rigImportSkipped  rigImportOutcome = "skipped"
	rigImportRefused  rigImportOutcome = "refused"
	rigImportFailed   rigImportOutcome = "failed"
)

// rigImportResult is one rig's import.
type rigImportResult struct {
	Rig        string
	Outcome    rigImportOutcome
	Why        string
	Copied     int
	Edges      int
	Gates      int
	Annotated  int
	Recorded   int
	Cleared    int
	OpenRouted int
	HashFloor  int
	Refusals   []rigImportRefusal
	Err        error
	Elapsed    time.Duration
}

func (r rigImportResult) ok() bool {
	switch r.Outcome {
	case rigImportImported, rigImportClean, rigImportNothing, rigImportSkipped:
		return true
	default:
		return false
	}
}

// importRig runs one rig's import end to end.
func importRig(env rigImportEnv, rig config.Rig) (result rigImportResult) {
	started := time.Now()
	result = rigImportResult{Rig: rig.Name}
	defer func() { result.Elapsed = time.Since(started) }()
	fail := func(err error) rigImportResult {
		result.Outcome, result.Err = rigImportFailed, err
		return result
	}
	if !rigNameIsFileSafe(rig.Name) {
		result.Outcome, result.Err = rigImportRefused, fmt.Errorf("rig name %q cannot name its import records", rig.Name)
		return result
	}
	if strings.TrimSpace(rig.Path) == "" {
		result.Outcome, result.Why = rigImportSkipped, "the rig is unbound and has no store of its own"
		return result
	}
	if samePath(resolveStoreScopeRoot(env.CityPath, rig.Path), resolveStoreScopeRoot(env.CityPath, env.CityPath)) {
		result.Outcome, result.Why = rigImportSkipped, "the rig's store is the city work store, which `"+storageMigrationCommand+"` moves"
		return result
	}
	loc := newRigImportLocation(env.Target, env.CityPath, rig.Name)
	manifest, _, err := loc.readManifest()
	if err != nil {
		return fail(err)
	}
	note, notePresent, err := readInfraClearedNoteAt(loc.NotePath())
	if err != nil {
		return fail(err)
	}
	resuming := notePresent && !note.Complete

	rigStore, err := rigImportStoreOpener(env.CityPath, rig)
	if err != nil {
		result.Outcome = rigImportRefused
		result.Err = fmt.Errorf("opening rig %s's store: %w (an unreachable rig blocks only itself; it stays awaiting import and its relic legs keep it readable)", rig.Name, err)
		return result
	}
	defer closeBeadStoreHandle(rigStore) //nolint:errcheck // best-effort close
	snap, err := readRigSnapshot(rigStore)
	if err != nil {
		result.Outcome, result.Err = rigImportRefused, fmt.Errorf("rig %s: %w", rig.Name, err)
		return result
	}
	binding, err := rigImportBindingOpener(env.Target)
	if err != nil {
		return fail(fmt.Errorf("opening the city binding: %w", err))
	}
	bindingOpen := true
	closeBinding := func() {
		if bindingOpen {
			_ = closeBeadStoreHandle(binding)
			bindingOpen = false
		}
	}
	defer closeBinding()

	gen := coordclass.CurrentGeneration
	if manifest != nil {
		gen = manifest.Generation
	}
	held, err := beads.GetMany(binding, bindingIDsToProbe(snap, gen))
	if err != nil {
		return fail(fmt.Errorf("reading the city binding: %w", err))
	}
	plan := planRigImport(rig.Name, env.CityName, snap, manifest, held)
	result.OpenRouted = plan.OpenRouted
	if len(plan.Refusals) > 0 {
		result.Outcome, result.Refusals = rigImportRefused, plan.Refusals
		return result
	}
	if len(plan.Move) == 0 && len(plan.Retained) == 0 && !resuming {
		closeBinding()
		if manifest != nil {
			result.Outcome = rigImportNothing
			if notePresent && note.Complete {
				return result
			}
			// A marker whose clear never started: nothing left to clear.
			if err := writeInfraClearedNoteAt(loc.NotePath(), infraClearedNote{Binding: env.Target.Binding, Database: env.Target.Database, Backup: loc.BackupPath(), Complete: true}); err != nil {
				return fail(err)
			}
			return result
		}
		if plan.Infra == 0 {
			if err := writeRigImportRecord(loc.CleanPath(), fmt.Sprintf("rig %s holds no infrastructure bead (census %s)", rig.Name, time.Now().UTC().Format(time.RFC3339))); err != nil {
				return fail(fmt.Errorf("recording rig %s clean: %w", rig.Name, err))
			}
			result.Outcome = rigImportClean
			return result
		}
		// Every infrastructure row is already carried as the city's (a rig
		// on the city's database): nothing of this rig's to move.
		result.Outcome, result.Why = rigImportNothing, fmt.Sprintf("the binding already holds all %d of the rig's infrastructure row(s) as the city's copy", len(plan.Shared))
		return result
	}

	if len(plan.Move) > 0 {
		if err := rigImportCopy(loc, binding, rigStore, manifest, plan, &result); err != nil {
			return fail(err)
		}
		closeBinding()
		if err := rigImportVerify(env, loc, manifest, plan); err != nil {
			return fail(err)
		}
		if err := rigImportCrashPoint("verified"); err != nil {
			return fail(err)
		}
		if err := rigImportWriteManifest(loc, env.Target, manifest, plan); err != nil {
			return fail(err)
		}
		if err := rigImportCrashPoint("manifest"); err != nil {
			return fail(err)
		}
		if !fileExists(loc.MarkerPath()) {
			if err := writeRigImportRecord(loc.MarkerPath(), fmt.Sprintf("rig %s graph rows imported into binding %s (%s) %s", rig.Name, env.Target.Binding, env.Target.Database, time.Now().UTC().Format(time.RFC3339))); err != nil {
				return fail(fmt.Errorf("writing rig %s's import marker: %w", rig.Name, err))
			}
		}
		if fileExists(loc.CleanPath()) {
			// A rig recorded clean that acquired rows since: the import record
			// now speaks for it.
			_ = os.Remove(loc.CleanPath())
		}
		if err := rigImportCrashPoint("marker"); err != nil {
			return fail(err)
		}
	} else {
		closeBinding()
	}

	reread, _, err := loc.readManifest()
	if err != nil {
		return fail(err)
	}
	if reread == nil {
		return fail(fmt.Errorf("rig %s: the import marker is missing after the copy", rig.Name))
	}
	floor, err := rigImportReserveIDs(env, rig, reread)
	if err != nil {
		return fail(fmt.Errorf("raising rig %s's minimum id length before the clear: %w (nothing was removed from the rig; re-run to finish)", rig.Name, err))
	}
	result.HashFloor = floor
	if err := rigImportCrashPoint("reserved"); err != nil {
		return fail(err)
	}
	cleared, err := rigImportClear(env, loc, rigStore, snap, reread, note, resuming, plan)
	result.Cleared = cleared
	if err != nil {
		return fail(err)
	}
	if len(plan.Move) == 0 && len(plan.Retained) == 0 {
		result.Outcome = rigImportNothing
		return result
	}
	result.Outcome = rigImportImported
	return result
}

// rigImportCopy is phases 2 and 3: prepare, then copy rows, edges and gates.
func rigImportCopy(loc rigImportLocation, binding, rigStore beads.Store, manifest *infraCopyManifest, plan rigImportPlan, result *rigImportResult) error {
	creator, ok := binding.(beads.ForeignIDCreator)
	if !ok {
		return fmt.Errorf("binding store %T cannot preserve bead ids", binding)
	}
	stamp := rigImportStamp(loc.Rig)
	stale, err := binding.List(beads.ListQuery{Metadata: map[string]string{infraMigrationStampKey: stamp}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return fmt.Errorf("listing the binding's rows from rig %s: %w", loc.Rig, err)
	}
	var drop []string
	for _, b := range stale {
		if manifest != nil && (manifest.IDs[b.ID] || manifest.Gates[b.ID]) {
			continue
		}
		drop = append(drop, b.ID)
	}
	if err := deleteInBatches(binding, drop); err != nil {
		return fmt.Errorf("clearing rig %s's interrupted attempt from the binding: %w", loc.Rig, err)
	}
	if err := rigImportCrashPoint("prepared"); err != nil {
		return err
	}
	for i, b := range plan.Move {
		if _, err := creator.CreateWithForeignID(rigImportRow(b, loc.Rig, plan.Records[b.ID])); err != nil {
			return fmt.Errorf("importing bead %s: %w", b.ID, err)
		}
		result.Copied++
		if i == len(plan.Move)/2 {
			if err := rigImportCrashPoint("copying"); err != nil {
				return err
			}
		}
	}
	result.Annotated = len(plan.Annotated)
	if err := rigImportCrashPoint("rows"); err != nil {
		return err
	}
	for _, edge := range plan.Copy {
		var err error
		if strings.HasPrefix(edge.DependsOnID, "external:") {
			err = binding.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type)
		} else {
			err = infraCopyDepEdge(binding, rigStore, edge.IssueID, edge.DependsOnID, edge.Type)
		}
		if err != nil {
			return fmt.Errorf("importing dep %s -> %s: %w", edge.IssueID, edge.DependsOnID, err)
		}
		result.Edges++
	}
	for _, g := range plan.Gates {
		gate, err := createRigImportGate(binding, loc.Rig, g)
		if err != nil {
			return err
		}
		if err := binding.DepAdd(g.Owner.ID, gate.ID, "blocks"); err != nil {
			return fmt.Errorf("holding %s on its cross-store gate %s: %w", g.Owner.ID, gate.ID, err)
		}
		result.Gates++
	}
	for _, recs := range plan.Records {
		result.Recorded += len(recs)
	}
	return nil
}

// createRigImportGate mints one S3b gate for g in the binding, stamped as
// this import's and scoped as its owner.
func createRigImportGate(binding beads.Store, rig string, g rigImportGate) (beads.Bead, error) {
	gate := dispatch.NewCrossStoreGate(binding, g.Owner, g.Blocker)
	gate.Metadata[infraMigrationStampKey] = rigImportStamp(rig)
	scope := strings.TrimSpace(g.Owner.Metadata[beadmeta.RootStoreRefMetadataKey])
	if scope == "" {
		scope = rigScopeRef(rig)
	}
	gate.Metadata[beadmeta.RootStoreRefMetadataKey] = scope
	created, err := binding.Create(gate)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("minting the cross-store gate for %s on %s: %w", g.Owner.ID, g.Blocker, err)
	}
	return created, nil
}

// rigImportVerify is phase 4: against a reopened binding, the rig's stamped
// population must be exactly the manifest's rows still held, plus this run's
// rows and gates; every row this run copied field-equal and in its class; and
// every copied row's edges exactly the planned ones.
func rigImportVerify(env rigImportEnv, loc rigImportLocation, manifest *infraCopyManifest, plan rigImportPlan) error {
	binding, err := openInfraBindingReadOnly(env.Target)
	if err != nil {
		return fmt.Errorf("reopening the binding to verify rig %s's copy: %w", loc.Rig, err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // best-effort close
	stamped, err := binding.List(beads.ListQuery{Metadata: map[string]string{infraMigrationStampKey: rigImportStamp(loc.Rig)}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return fmt.Errorf("listing the binding's rows from rig %s: %w", loc.Rig, err)
	}
	got := make(map[string]beads.Bead, len(stamped))
	for _, b := range stamped {
		got[b.ID] = b
	}
	gateOf := map[string]string{} // gate id -> owner
	for _, b := range stamped {
		if owner := b.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]; owner != "" && hasLabel(b.Labels, dispatch.CrossStoreGateLabel) {
			gateOf[b.ID] = owner
		}
	}
	moved := make(map[string]bool, len(plan.Move))
	for _, b := range plan.Move {
		moved[b.ID] = true
		g, ok := got[b.ID]
		if !ok {
			return fmt.Errorf("verifying rig %s's copy: the binding does not hold %s", loc.Rig, b.ID)
		}
		want := rigImportRow(b, loc.Rig, plan.Records[b.ID])
		if diff := beadCopyDifference(want, g); diff != "" {
			return fmt.Errorf("verifying rig %s's copy: %s: %s", loc.Rig, b.ID, diff)
		}
		if diff := infraCopyClassDifference(b, g); diff != "" {
			return fmt.Errorf("verifying rig %s's copy: %s %s", loc.Rig, b.ID, diff)
		}
	}
	gatesWanted := 0
	for id := range got {
		switch {
		case moved[id]:
		case gateOf[id] != "":
			gatesWanted++
		case manifest != nil && (manifest.IDs[id] || manifest.Gates[id]):
		default:
			return fmt.Errorf("verifying rig %s's copy: the binding holds %s under this rig's import stamp, which neither this run nor an earlier import wrote", loc.Rig, id)
		}
	}
	priorGates := 0
	if manifest != nil {
		for id := range manifest.Gates {
			if gateOf[id] != "" {
				priorGates++
			}
		}
	}
	if gatesWanted-priorGates != len(plan.Gates) {
		return fmt.Errorf("verifying rig %s's copy: the binding holds %d new cross-store gate(s), the plan minted %d", loc.Rig, gatesWanted-priorGates, len(plan.Gates))
	}
	wantEdges := map[string][]string{}
	for _, e := range plan.Copy {
		wantEdges[e.IssueID] = append(wantEdges[e.IssueID], e.DependsOnID+" ("+e.Type+")")
	}
	for gateID, owner := range gateOf {
		if moved[owner] && (manifest == nil || !manifest.Gates[gateID]) {
			wantEdges[owner] = append(wantEdges[owner], gateID+" (blocks)")
		}
	}
	for _, b := range plan.Move {
		deps, err := binding.DepList(b.ID, "down")
		if err != nil {
			return fmt.Errorf("verifying rig %s's copy: listing deps of %s: %w", loc.Rig, b.ID, err)
		}
		if diff := stringSetDifference("dependency of "+b.ID, wantEdges[b.ID], censusEdges(deps)); diff != "" {
			return fmt.Errorf("verifying rig %s's copy: %s", loc.Rig, diff)
		}
	}
	return nil
}

// rigImportWriteManifest is phase 5's manifest: the earlier import's record
// (when the marker exists) plus this run's rows and gates.
func rigImportWriteManifest(loc rigImportLocation, target infraBindingTarget, manifest *infraCopyManifest, plan rigImportPlan) error {
	ids := map[string]bool{}
	created := map[string]time.Time{}
	annotated := map[string]bool{}
	gates := map[string]bool{}
	gen := coordclass.CurrentGeneration
	var writtenAt time.Time
	if manifest != nil {
		for id := range manifest.IDs {
			ids[id] = true
		}
		for id, at := range manifest.Created {
			created[id] = at
		}
		for id := range manifest.ScopeAnnotated {
			annotated[id] = true
		}
		for id := range manifest.Gates {
			gates[id] = true
		}
		gen = manifest.Generation
		writtenAt = manifest.WrittenAt
	}
	for _, b := range plan.Move {
		ids[b.ID] = true
		created[b.ID] = b.CreatedAt
	}
	for _, id := range plan.Annotated {
		annotated[id] = true
	}
	binding, err := openInfraBindingReadOnly(target)
	if err != nil {
		return fmt.Errorf("reading rig %s's gates for the manifest: %w", loc.Rig, err)
	}
	minted, err := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, Metadata: map[string]string{infraMigrationStampKey: rigImportStamp(loc.Rig)}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	_ = closeBeadStoreHandle(binding)
	if err != nil {
		return fmt.Errorf("reading rig %s's gates for the manifest: %w", loc.Rig, err)
	}
	for _, g := range minted {
		gates[g.ID] = true
	}
	if writtenAt.IsZero() {
		writtenAt = time.Now().UTC()
	}
	extra := []string{infraManifestScopePrefix + rigScopeRef(loc.Rig)}
	for _, id := range sortedMapKeys(annotated) {
		extra = append(extra, infraManifestScopeAnnotatedPrefix+id)
	}
	for _, id := range sortedMapKeys(gates) {
		extra = append(extra, infraManifestGatePrefix+id)
	}
	return writeInfraCopyManifestFileAt(loc.BindingRoot, loc.ManifestPath(), sortedMapKeys(ids), created, writtenAt, writtenAt, gen, extra)
}

// rigImportReserveIDs is phase 6: raise the rig's bd min_hash_length past
// every imported id, never lowering it. It returns the floor it requires (0
// when no id needs one or the store mints from a monotonic sequence).
func rigImportReserveIDs(env rigImportEnv, rig config.Rig, manifest *infraCopyManifest) (int, error) {
	floor := rigImportHashFloor(sortedMapKeys(manifest.IDs))
	if floor == 0 {
		return 0, nil
	}
	if floor > bdMaxHashLength {
		floor = bdMaxHashLength
	}
	conf, err := rigImportHashConfigOpener(env.CityPath, env.Cfg, rig)
	if err != nil {
		return 0, err
	}
	if conf == nil {
		return 0, nil
	}
	current, err := conf.ConfigGet(bdMinHashLengthKey)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", bdMinHashLengthKey, err)
	}
	if n, perr := strconv.Atoi(strings.TrimSpace(current)); perr == nil && n >= floor {
		return n, nil
	}
	if err := conf.ConfigSet(bdMinHashLengthKey, strconv.Itoa(floor)); err != nil {
		return 0, fmt.Errorf("setting %s=%d: %w", bdMinHashLengthKey, floor, err)
	}
	return floor, nil
}

// rigImportClear is phase 7: back up, prove, mark pending, delete in
// batches, mark complete. It clears every row the manifest records as
// carried that the rig still holds. No edge is re-added.
func rigImportClear(env rigImportEnv, loc rigImportLocation, rigStore beads.Store, snap rigSnapshot, manifest *infraCopyManifest, note infraClearedNote, resuming bool, plan rigImportPlan) (int, error) {
	var toClear []beads.Bead
	for _, b := range snap.rows {
		if coordclass.ClassifyAt(b, manifest.Generation) == coordclass.ClassWork {
			continue
		}
		if manifest.Carried(b) {
			toClear = append(toClear, b)
		}
	}
	session := map[string]bool{}
	if resuming {
		for _, id := range note.Pending {
			session[id] = true
		}
	}
	clearing := map[string]bool{}
	for _, b := range toClear {
		session[b.ID] = true
		clearing[b.ID] = true
	}
	if len(session) == 0 {
		if !note.Complete {
			return 0, writeInfraClearedNoteAt(loc.NotePath(), infraClearedNote{Binding: env.Target.Binding, Database: env.Target.Database, Backup: loc.BackupPath(), Complete: true})
		}
		return 0, nil
	}
	previous, _, err := readInfraRetainedBackupAt(loc.BackupPath())
	if err != nil {
		return 0, err
	}
	sessionBackup := map[string]infraBackupEntry{}
	if resuming {
		for _, e := range previous {
			if session[e.Bead.ID] {
				sessionBackup[e.Bead.ID] = e
			}
		}
	}
	up := map[string][]beads.Dep{}
	for _, b := range snap.rows {
		for _, d := range snap.down[b.ID] {
			if d.IssueID == "" {
				d.IssueID = b.ID
			}
			up[d.DependsOnID] = append(up[d.DependsOnID], d)
		}
	}
	disposition := rigEdgeDispositions(plan)
	entries := make([]infraBackupEntry, 0, len(toClear))
	for _, b := range toClear {
		if prior, ok := sessionBackup[b.ID]; ok {
			entries = append(entries, prior)
			continue
		}
		entry, err := rigBackupEntry(rigStore, b, snap.down[b.ID], up[b.ID], clearing, disposition)
		if err != nil {
			return 0, err
		}
		entries = append(entries, entry)
	}
	merged := mergeInfraBackupEntries(previous, entries)
	label := env.Target.Binding + " " + rigImportStamp(loc.Rig)
	if len(entries) > 0 {
		if err := writeInfraRetainedBackupAt(loc.BindingRoot, loc.BackupPath(), label, merged); err != nil {
			return 0, err
		}
		if err := verifyInfraRetainedBackupAt(loc.BackupPath(), entries); err != nil {
			return 0, err
		}
	}
	if err := rigImportCrashPoint("backup"); err != nil {
		return 0, err
	}
	pending := sortedMapKeys(session)
	if err := writeInfraClearedNoteAt(loc.NotePath(), infraClearedNote{Binding: env.Target.Binding, Database: env.Target.Database, Backup: loc.BackupPath(), Pending: pending}); err != nil {
		return 0, err
	}
	if err := rigImportCrashPoint("pending"); err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(toClear))
	for _, b := range toClear {
		ids = append(ids, b.ID)
	}
	if err := deleteInBatchesWithCrash(rigStore, ids); err != nil {
		return 0, fmt.Errorf("removing rig %s's imported copies: %w (the cleared note is pending; boot refuses until `%s` finishes the toClear)", loc.Rig, err, rigImportInstruction(loc.Rig))
	}
	after, err := rigStore.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return 0, fmt.Errorf("re-reading rig %s after the toClear: %w", loc.Rig, err)
	}
	var left []string
	for _, b := range after {
		if session[b.ID] && manifest.Carried(b) {
			left = append(left, b.ID)
		}
	}
	if len(left) > 0 {
		sort.Strings(left)
		return 0, fmt.Errorf("rig %s still holds %d imported cop(ies) after the toClear: %s", loc.Rig, len(left), infraStrandedIDList(left))
	}
	if err := writeInfraClearedNoteAt(loc.NotePath(), infraClearedNote{Binding: env.Target.Binding, Database: env.Target.Database, Backup: loc.BackupPath(), Complete: true}); err != nil {
		return 0, err
	}
	return len(toClear), nil
}

// rigEdgeDispositions maps each planned edge to what the import did with it,
// keyed "from\x00to".
func rigEdgeDispositions(plan rigImportPlan) map[string]string {
	out := map[string]string{}
	for _, e := range plan.Copy {
		out[e.IssueID+"\x00"+e.DependsOnID] = rigEdgeCopied
	}
	for id, recs := range plan.Records {
		for _, r := range recs {
			key := id + "\x00" + r.Other
			if r.Dir == "in" {
				key = r.Other + "\x00" + id
			}
			out[key] = r.Action
		}
	}
	return out
}

// rigBackupEntry is one cleared row with its out-edges and its in-edges from
// rows that stay, each marked with what the import did with it.
func rigBackupEntry(rigStore beads.Store, b beads.Bead, down, up []beads.Dep, clearing map[string]bool, disposition map[string]string) (infraBackupEntry, error) {
	entry := infraBackupEntry{Bead: b, Class: coordclass.Classify(b).String()}
	for _, d := range down {
		if d.IssueID == "" {
			d.IssueID = b.ID
		}
		edge, err := rigBackupEdge(rigStore, d, disposition)
		if err != nil {
			return entry, err
		}
		entry.Deps = append(entry.Deps, edge)
	}
	for _, d := range up {
		if clearing[d.IssueID] {
			continue
		}
		edge, err := rigBackupEdge(rigStore, d, disposition)
		if err != nil {
			return entry, err
		}
		entry.Dependents = append(entry.Dependents, edge)
	}
	sortInfraBackupEdges(entry.Deps)
	sortInfraBackupEdges(entry.Dependents)
	return entry, nil
}

func rigBackupEdge(rigStore beads.Store, d beads.Dep, disposition map[string]string) (infraBackupEdge, error) {
	edge, err := infraBackupEdgeFor(rigStore, d)
	if err != nil {
		return edge, err
	}
	edge.Disposition = disposition[d.IssueID+"\x00"+d.DependsOnID]
	if edge.Disposition == "" {
		edge.Disposition = rigEdgeCopied
	}
	return edge, nil
}

// deleteInBatches removes ids through the store's batched delete in chunks,
// falling back to one delete per id.
func deleteInBatches(store beads.Store, ids []string) error {
	return deleteInBatchesStep(store, ids, nil)
}

// deleteInBatchesWithCrash is deleteInBatches with the importer's crash seam
// between chunks.
func deleteInBatchesWithCrash(store beads.Store, ids []string) error {
	return deleteInBatchesStep(store, ids, func() error { return rigImportCrashPoint("deleting") })
}

func deleteInBatchesStep(store beads.Store, ids []string, between func() error) error {
	if len(ids) == 0 {
		return nil
	}
	batcher, batched := store.(beads.BatchDeleter)
	for start := 0; start < len(ids); start += rigImportDeleteChunk {
		end := start + rigImportDeleteChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		if batched {
			err := batcher.DeleteBatch(chunk)
			if err == nil {
				if between != nil && end < len(ids) {
					if err := between(); err != nil {
						return err
					}
				}
				continue
			}
			if !errors.Is(err, beads.ErrBatchDeleteUnsupported) {
				return err
			}
			batched = false
		}
		for _, id := range chunk {
			if err := store.Delete(id); err != nil && !errors.Is(err, beads.ErrNotFound) {
				return fmt.Errorf("deleting %s: %w", id, err)
			}
		}
		if between != nil && end < len(ids) {
			if err := between(); err != nil {
				return err
			}
		}
	}
	return nil
}

// doStorageMigrateFromRigs is `gc storage migrate --from-rigs`.
func doStorageMigrateFromRigs(ctx context.Context, request storageOperatorRequest, rigNames []string, stdout, stderr io.Writer) int {
	const logPrefix = "gc storage migrate --from-rigs"
	if err := config.ReservedPrefixSplitRefusal(request.Cfg); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !ok {
		fmt.Fprintf(stderr, "%s: this city serves every class from its work stores; there is nothing to import\n", logPrefix) //nolint:errcheck // best-effort stderr
		return 1
	}
	if _, err := resolveCityStoragePlan(request.CityPath, request.Cfg); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		fmt.Fprintf(stderr, "%s: controller PID %d is live on this city. Stop it (%s) and run this again\n", logPrefix, pid, storageStopCommand) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !request.FleetStopped {
		fmt.Fprintf(stderr, "%s: pass --%s to attest that every writer that can reach this city's rig stores is stopped. This command proves the controller is stopped and cannot prove anything about the rest\n", logPrefix, storageFleetStoppedFlag) //nolint:errcheck // best-effort stderr
		return 1
	}
	if state, err := readInfraConvergenceState(target); err != nil || state != infraConvergenceMarked {
		fmt.Fprintf(stderr, "%s: the city binding %s has not converged (no %s), and a rig import never creates or re-copies a binding. Run `%s --%s` first\n", logPrefix, target.Binding, target.MarkerPath(), storageMigrationCommand, storageFleetStoppedFlag) //nolint:errcheck // best-effort stderr
		return 1
	}
	rigs, err := selectRigsForImport(request.Cfg, rigNames)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer func() {
		if releaseErr := guard.Release(); releaseErr != nil {
			fmt.Fprintf(stderr, "%s: releasing the migration guard: %v\n", logPrefix, releaseErr) //nolint:errcheck // best-effort stderr
		}
	}()
	env := rigImportEnv{
		CityPath: request.CityPath,
		CityName: loadedCityName(request.Cfg, request.CityPath),
		Cfg:      request.Cfg,
		Target:   target,
		Stdout:   stdout,
		Stderr:   stderr,
		Started:  time.Now(),
	}
	shared := rigsSharingOneDatabase(env, rigs)
	rec := openCityRecorderAt(request.CityPath, stderr)
	exit := 0
	for _, rig := range rigs {
		var result rigImportResult
		if others, dup := shared[rig.Name]; dup {
			result = rigImportResult{Rig: rig.Name, Outcome: rigImportRefused, Err: fmt.Errorf("rig %s lists exactly the rows rig(s) %s list, so they resolve to one database; importing one would stamp the other's rows with the wrong scope. Give each rig its own database, then re-run", rig.Name, strings.Join(others, ", "))}
		} else {
			result = importRig(env, rig)
		}
		reportRigImport(stdout, stderr, result)
		recordRigImportEvent(rec, target, result)
		if !result.ok() {
			exit = 1
		}
	}
	return exit
}

// selectRigsForImport returns the configured rigs named, or every rig.
func selectRigsForImport(cfg *config.City, names []string) ([]config.Rig, error) {
	if len(names) == 0 {
		return append([]config.Rig(nil), cfg.Rigs...), nil
	}
	byName := make(map[string]config.Rig, len(cfg.Rigs))
	for _, r := range cfg.Rigs {
		byName[r.Name] = r
	}
	var out []config.Rig
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		r, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("this city declares no rig %q", name)
		}
		if !seen[name] {
			out = append(out, r)
			seen[name] = true
		}
	}
	return out, nil
}

// rigsSharingOneDatabase reports, for each rig of this run, the other rigs of
// this run whose store lists exactly the same non-empty set of row ids: two
// rigs on one database. A rig that cannot be read is not compared; its own
// import reports why.
func rigsSharingOneDatabase(env rigImportEnv, rigs []config.Rig) map[string][]string {
	if len(rigs) < 2 {
		return nil
	}
	byPrint := map[string][]string{}
	for _, rig := range rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		store, err := rigImportStoreOpener(env.CityPath, rig)
		if err != nil {
			continue
		}
		rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		_ = closeBeadStoreHandle(store)
		if err != nil || len(rows) == 0 {
			continue
		}
		ids := make([]string, 0, len(rows))
		for _, b := range rows {
			ids = append(ids, b.ID)
		}
		sort.Strings(ids)
		key := strings.Join(ids, "\x00")
		byPrint[key] = append(byPrint[key], rig.Name)
	}
	out := map[string][]string{}
	for _, names := range byPrint {
		if len(names) < 2 {
			continue
		}
		for _, n := range names {
			var others []string
			for _, o := range names {
				if o != n {
					others = append(others, o)
				}
			}
			out[n] = others
		}
	}
	return out
}

// reportRigImport prints one rig's outcome.
func reportRigImport(stdout, stderr io.Writer, r rigImportResult) {
	switch r.Outcome {
	case rigImportImported:
		fmt.Fprintf(stdout, "rig %s: imported %d bead(s) and %d edge(s) into the city binding; %d cross-store gate(s) minted, %d scope annotation(s), %d boundary edge(s) recorded, %d rig cop(ies) cleared", r.Rig, r.Copied, r.Edges, r.Gates, r.Annotated, r.Recorded, r.Cleared) //nolint:errcheck // best-effort stdout
		if r.HashFloor > 0 {
			fmt.Fprintf(stdout, "; rig min_hash_length %d", r.HashFloor) //nolint:errcheck // best-effort stdout
		}
		if r.OpenRouted > 0 {
			fmt.Fprintf(stdout, "; %d open routed bead(s) are now visible to demand", r.OpenRouted) //nolint:errcheck // best-effort stdout
		}
		fmt.Fprintf(stdout, " (%s)\n", r.Elapsed.Round(time.Millisecond)) //nolint:errcheck // best-effort stdout
	case rigImportClean:
		fmt.Fprintf(stdout, "rig %s: holds no infrastructure bead; recorded clean\n", r.Rig) //nolint:errcheck // best-effort stdout
	case rigImportNothing:
		msg := "nothing to import"
		if r.Why != "" {
			msg += " (" + r.Why + ")"
		}
		if r.Cleared > 0 {
			msg += fmt.Sprintf("; %d rig cop(ies) cleared", r.Cleared)
		}
		fmt.Fprintf(stdout, "rig %s: %s\n", r.Rig, msg) //nolint:errcheck // best-effort stdout
	case rigImportSkipped:
		fmt.Fprintf(stdout, "rig %s: skipped — %s\n", r.Rig, r.Why) //nolint:errcheck // best-effort stdout
	case rigImportRefused:
		if r.Err != nil {
			fmt.Fprintf(stderr, "rig %s: REFUSED — %v\n", r.Rig, r.Err) //nolint:errcheck // best-effort stderr
		}
		if len(r.Refusals) > 0 {
			fmt.Fprintf(stderr, "rig %s: REFUSED — %d shape(s) the import cannot move without a cross-store edge; nothing was written:\n", r.Rig, len(r.Refusals)) //nolint:errcheck // best-effort stderr
			for _, refusal := range r.Refusals {
				fmt.Fprintf(stderr, "  %s\n", refusal) //nolint:errcheck // best-effort stderr
			}
		}
	default:
		fmt.Fprintf(stderr, "rig %s: FAILED — %v. Re-run `%s` to resume\n", r.Rig, r.Err, rigImportInstruction(r.Rig)) //nolint:errcheck // best-effort stderr
	}
}

// recordRigImportEvent publishes one rig's import outcome.
func recordRigImportEvent(rec events.Recorder, target infraBindingTarget, r rigImportResult) {
	if rec == nil {
		return
	}
	detail := r.Why
	if r.Err != nil {
		detail = r.Err.Error()
	}
	if len(r.Refusals) > 0 {
		parts := make([]string, 0, len(r.Refusals))
		for _, refusal := range r.Refusals {
			parts = append(parts, refusal.String())
		}
		detail = strings.Join(parts, "; ")
	}
	raw, err := json.Marshal(storebinding.StorageRigPayload{
		Rig:     r.Rig,
		Binding: target.Binding,
		Outcome: string(r.Outcome),
		Beads:   r.Copied,
		Gates:   r.Gates,
		Cleared: r.Cleared,
		Detail:  detail,
	})
	if err != nil {
		return
	}
	rec.Record(events.Event{Type: events.StorageRigImport, Actor: "gc", Subject: r.Rig, Payload: raw})
}

// rigsWithoutImportRecord names the bound rigs that carry neither an import
// marker nor a clean record: file reads only, nothing is opened.
func rigsWithoutImportRecord(cityPath string, cfg *config.City, target infraBindingTarget) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" || !rigNameIsFileSafe(rig.Name) {
			continue
		}
		if samePath(resolveStoreScopeRoot(cityPath, rig.Path), resolveStoreScopeRoot(cityPath, cityPath)) {
			continue
		}
		loc := newRigImportLocation(target, cityPath, rig.Name)
		if fileExists(loc.MarkerPath()) || fileExists(loc.CleanPath()) {
			continue
		}
		out = append(out, rig.Name)
	}
	sort.Strings(out)
	return out
}
