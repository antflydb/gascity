package main

// The stranded-write purge: `gc storage recover-stranded --from-work
// --purge-type message`. It backs up, then deletes, the stranded work-store
// rows of one type, where recover-stranded's default copies them into the
// binding.
//
// # Why a purge exists
//
// A stranded row is one the boot refuses over: the work store holds it, the
// converged binding does not, and the proven-copy manifest does not record it
// (confirmInfraConvergence). recover-stranded's answer is to carry it in. For
// mail that is the wrong answer when the mail is dead: maintainer-city's 791
// stranded rows are auto-handoff mails ("context cycle") from August, open
// only because nothing ever read them, and carrying them in would deliver a
// month of stale handoffs to live agents. The owner's decision is to back
// them up and purge them.
//
// # What it selects, and what it refuses
//
// Exactly the rows the containment check reports as stranded
// (classifyInfraContainmentGap, the function the boot gate and `gc storage
// status` call), whose type is the purge type. It never selects by manifest
// id: bd reuses short wisp ids, so an id names a bead only together with its
// created_at, and the check already judges every row that way.
//
// Every selected row must also classify (coordclass, under the manifest's
// generation) as the purge type's class. A type=message row the classifier
// puts elsewhere — a wisp step typed message is graph — is not mail, and the
// whole run refuses rather than purging it. Stranded rows of other types are
// left exactly where they are and named: the purge does not decide for them,
// and `gc storage status` keeps refusing until they are recovered.
//
// A work-store row the binding holds as the same bead (the same id, created
// in the same second) is never deleted. Such a row is not stranded, so it is
// never selected; and refuseInfraPurgeOfBindingRows re-reads the binding for
// every selected row immediately before the delete, so the property does not
// rest on the selection alone.
//
// # Back up, prove, then delete
//
// Every selected row is written, with its metadata, labels and every edge
// touching it, to a timestamped JSONL file at the binding root (under
// .gc/store), in recover-stranded's choice-backup shape
// (infraRecoverChoiceRecord). The file is synced, renamed into place, its
// directory synced, and re-read and compared record for record before
// anything is deleted. The run is recorded in a ledger beside it
// (infra.purged-stranded.jsonl): "backed-up" before the delete, "purged"
// after it.
//
// # Resume
//
// A run that dies after the backup leaves the ledger's last record at
// "backed-up". The next run recomputes the selection from live state (a
// deleted row is simply no longer stranded), and when the pending backup
// already holds every selected row exactly as the work store holds it, it
// reuses that backup instead of writing another. Either way every deleted row
// is in some proven backup first. Re-running after a completed purge selects
// nothing and is a no-op.
//
// # Status
//
// The purged rows leave the work store, so the containment check stops
// naming them and `gc storage status` reports converged once nothing else is
// stranded. The manifest is NOT extended: its meaning is "the copy was proven
// to deliver this bead", and a purged row was never delivered. Status reads
// the ledger instead, reports how many rows were purged and where the backups
// are, and refuses while the ledger shows an interrupted purge.
//
// # Exclusion
//
// The same as recover-stranded's: the controller socket must be silent, the
// operator attests --fleet-stopped, and the run holds the city's migration
// guard. A dry run is read-only and takes none of them, so it can rehearse
// against a live city.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// The purge's flags on recover-stranded.
const (
	strandedPurgeTypeFlag    = "purge-type"
	strandedConfirmPurgeFlag = "confirm-purge"
)

// infraPurgeTypes maps each purgeable bead type to the class its rows must
// classify as. Mail is the one class the owner decided to purge rather than
// recover; a type is added here only by the same decision.
var infraPurgeTypes = map[string]coordclass.Class{
	"message": coordclass.ClassMessaging,
}

// infraPurgeChoice is the Choice a purge backup record carries.
const infraPurgeChoice = "purge-type"

// The ledger and the backups live at the binding root, beside the manifest.
const (
	infraPurgeLedgerName            = "infra.purged-stranded.jsonl"
	infraPurgedStrandedBackupPrefix = "infra.purged-stranded-"
	infraPurgeLedgerFormat          = "gc.infra.purged-stranded/v1"
)

// The ledger's two events for one run.
const (
	infraPurgeEventBackedUp = "backed-up"
	infraPurgeEventPurged   = "purged"
)

// PurgeLedgerPath is where recover-stranded --purge-type records its runs.
func (t infraBindingTarget) PurgeLedgerPath() string {
	return filepath.Join(t.Root, infraPurgeLedgerName)
}

// strandedPurgeOptions are the purge's knobs.
type strandedPurgeOptions struct {
	// Type is the bead type to purge (infraPurgeTypes).
	Type string
	// DryRun lists the selection and writes nothing.
	DryRun bool
	// Confirm is the operator's explicit go-ahead to delete.
	Confirm bool
}

// infraPurgeRowKey is one purged row's identity: an id names a bead only with
// its created_at, because bd reuses short wisp ids.
type infraPurgeRowKey struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// infraPurgeLedgerRecord is one ledger line.
type infraPurgeLedgerRecord struct {
	Format     string             `json:"format"`
	Event      string             `json:"event"`
	Run        string             `json:"run"`
	Type       string             `json:"type"`
	Backup     string             `json:"backup"`
	Rows       []infraPurgeRowKey `json:"rows"`
	RecordedAt time.Time          `json:"recorded_at"`
}

// infraPurgeAfterBackup runs between the recorded backup and the delete. It
// is a seam: a test makes it fail to stand in for a crash at that point.
var infraPurgeAfterBackup = func() error { return nil }

// validateStrandedPurgeFlags refuses --purge-type beside the per-id answers:
// the purge selects by class, and mixing the two would make one run both
// copy and delete.
func validateStrandedPurgeFlags(purgeType string, confirm bool, perID map[string]bool) error {
	if purgeType == "" {
		if confirm {
			return fmt.Errorf("--%s confirms a --%s run; pass --%s message", strandedConfirmPurgeFlag, strandedPurgeTypeFlag, strandedPurgeTypeFlag)
		}
		return nil
	}
	var named []string
	for flag, set := range perID {
		if set {
			named = append(named, "--"+flag)
		}
	}
	if len(named) > 0 {
		sort.Strings(named)
		return fmt.Errorf("--%s selects rows by class and does not combine with %s; run them separately", strandedPurgeTypeFlag, strings.Join(named, ", "))
	}
	return nil
}

// doStoragePurgeStranded is `gc storage recover-stranded --from-work
// --purge-type <type>`.
func doStoragePurgeStranded(ctx context.Context, request storageOperatorRequest, opts strandedPurgeOptions, stdout, stderr io.Writer) int {
	logPrefix := storageRecoveryLogPrefix()
	class, supported := infraPurgeTypes[opts.Type]
	if !supported {
		fmt.Fprintf(stderr, "%s: --%s %q is not supported; the only purgeable type is message (mail). Recover other stranded rows with `%s`\n", //nolint:errcheck // best-effort stderr
			logPrefix, strandedPurgeTypeFlag, opts.Type, storageRecoveryCommand)
		return 1
	}

	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !ok {
		reportUnresolvedInfraBinding(request, logPrefix, stdout, stderr)
		return 1
	}
	if _, err := resolveCityStoragePlan(request.CityPath, request.Cfg); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	state, err := readInfraConvergenceState(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if state != infraConvergenceMarked {
		fmt.Fprintf(stderr, "%s: this city has not converged onto binding %q, so nothing here is a stranded write. Run:  %s\n", //nolint:errcheck // best-effort stderr
			logPrefix, target.Binding, storageMigrationCommand)
		return 1
	}

	if blocked, held := servedBindingNoteHold(request.CityPath, target.Binding, config.StorageProviderSQLiteBeads, target.Database); held {
		blocked.Target = target
		fmt.Fprintln(stderr, infraMigrationOperatorAdvice(blocked, logPrefix)) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !opts.DryRun {
		if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
			fmt.Fprintf(stderr, "%s: controller PID %d is live on this city and is still writing the work store. Stop it (gc stop) and run this again\n", logPrefix, pid) //nolint:errcheck // best-effort stderr
			return 1
		}
		if !request.FleetStopped {
			fmt.Fprintf(stderr, "%s: pass --%s to attest that %s. A purge deletes work-store rows, and a writer still running could write the rows it is judging\n", //nolint:errcheck // best-effort stderr
				logPrefix, storageFleetStoppedFlag, storageFleetStoppedAttestation)
			return 1
		}
		guard, err := storebinding.AcquireMigrationGuard(ctx, cityMigrationGuardDirectory(request.CityPath), storageMigrationGeneration)
		if err != nil {
			if errors.Is(err, storebinding.ErrMigrationGuardBusy) {
				fmt.Fprintf(stderr, "%s: another storage migration or repair holds this city. Wait for it to finish, or resolve it, and run this again\n", logPrefix) //nolint:errcheck // best-effort stderr
				return 1
			}
			fmt.Fprintf(stderr, "%s: taking the migration guard: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		defer func() {
			if releaseErr := guard.Release(); releaseErr != nil {
				fmt.Fprintf(stderr, "%s: releasing the migration guard: %v\n", logPrefix, releaseErr) //nolint:errcheck // best-effort stderr
			}
		}()
	}

	manifest, recorded, err := readInfraCopyManifestRecord(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !recorded {
		fmt.Fprintf(stderr, "%s: %s converged before %s was recorded, so nothing tells a stranded row from one the binding's own GC collected, and nothing here can say which rows are stranded. Re-converge the binding to restore the check\n", //nolint:errcheck // best-effort stderr
			logPrefix, target.Database, target.ManifestPath())
		return 1
	}

	// The selection, by the containment check itself.
	gap, err := classifyInfraContainmentGap(request.CityPath, target, manifest)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	source, err := openInfraMigrationSource(request.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening the work store: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer closeBeadStoreHandle(source) //nolint:errcheck // best-effort close
	all, err := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		fmt.Fprintf(stderr, "%s: listing work store: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	byID := make(map[string]beads.Bead, len(all))
	for _, b := range all {
		byID[b.ID] = b
	}
	heldDifferent := setOf(gap.HeldDifferent)
	var selected, others []beads.Bead
	var wrongClass, vanished []string
	for _, id := range gap.Stranded {
		b, present := byID[id]
		if !present {
			vanished = append(vanished, id)
			continue
		}
		if b.Type != opts.Type {
			others = append(others, b)
			continue
		}
		if got := coordclass.ClassifyAt(b, manifest.Generation); got != class {
			wrongClass = append(wrongClass, fmt.Sprintf("%s (classifies as %s, not %s)", b.ID, got, class))
			continue
		}
		selected = append(selected, b)
	}
	if len(vanished) > 0 {
		fmt.Fprintf(stderr, "%s: the work store changed while it was read (%s were stranded a moment ago and are gone); a purge judged against a moving store proves nothing. Stop every writer and run this again. Nothing was written\n", //nolint:errcheck // best-effort stderr
			logPrefix, strings.Join(vanished, ", "))
		return 1
	}
	if len(wrongClass) > 0 {
		fmt.Fprintf(stderr, "%s: %d stranded row(s) of type %s are not %s-class, and this purge only deletes %s: %s. Nothing was written. Recover them with `%s`\n", //nolint:errcheck // best-effort stderr
			logPrefix, len(wrongClass), opts.Type, class, class, strings.Join(wrongClass, "; "), storageRecoveryCommand)
		return 1
	}
	nonStranded := 0
	stranded := setOf(gap.Stranded)
	for _, b := range all {
		if b.Type == opts.Type && !stranded[b.ID] {
			nonStranded++
		}
	}

	fmt.Fprintf(stdout, "city:     %s\nbinding:  %s\nwork-store rows: %d\nstranded: %d\n", request.CityPath, target.Database, len(all), len(gap.Stranded)) //nolint:errcheck // best-effort stdout
	if nonStranded > 0 {
		fmt.Fprintf(stdout, "left in place: %d %s row(s) the containment check does not call stranded (the binding holds them, or the manifest records them); they are never purged\n", nonStranded, opts.Type) //nolint:errcheck // best-effort stdout
	}
	if len(others) > 0 {
		fmt.Fprintf(stdout, "not purged: %d stranded row(s) of other types stay where they are, and the boot keeps refusing over them until they are recovered with `%s`: %s\n", //nolint:errcheck // best-effort stdout
			len(others), storageRecoveryCommand, beadIDList(others))
	}
	if len(selected) == 0 {
		fmt.Fprintf(stdout, "nothing to purge: no stranded row of type %s\n", opts.Type) //nolint:errcheck // best-effort stdout
		return 0
	}
	reportInfraPurgeSelection(stdout, opts.Type, class, selected, heldDifferent)

	if opts.DryRun {
		fmt.Fprintf(stdout, "dry-run: nothing was written. Run again with --%s --%s to back up and delete these %d row(s)\n", storageFleetStoppedFlag, strandedConfirmPurgeFlag, len(selected)) //nolint:errcheck // best-effort stdout
		return 0
	}
	if !opts.Confirm {
		fmt.Fprintf(stderr, "%s: nothing was written. Deleting %d row(s) needs an explicit --%s; review the list above (or run with --dry-run) and run again with it\n", logPrefix, len(selected), strandedConfirmPurgeFlag) //nolint:errcheck // best-effort stderr
		return 1
	}

	entries, err := infraPurgeBackupEntries(source, selected, all)
	if err != nil {
		fmt.Fprintf(stderr, "%s: backing up the selected rows: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	ledger, err := readInfraPurgeLedger(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	pending, resumed, err := infraPurgeResumableBackup(ledger, entries)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	keys := infraPurgeKeys(selected)
	backupPath, run := pending.Backup, pending.Run
	if resumed {
		fmt.Fprintf(stdout, "resuming the purge run %s: its backup %s already holds every selected row as the work store holds it\n", run, backupPath) //nolint:errcheck // best-effort stdout
	} else {
		now := time.Now().UTC()
		run = now.Format("20060102T150405.000000000Z")
		backupPath = filepath.Join(target.Root, infraPurgedStrandedBackupPrefix+run+".jsonl")
		records := make([]infraRecoverChoiceRecord, 0, len(entries))
		for _, entry := range entries {
			records = append(records, infraRecoverChoiceRecord{Format: infraRecoverChoicesFormat, Choice: infraPurgeChoice, Store: infraRecoverChoiceStoreWork, RecordedAt: now, Entry: entry})
		}
		if err := writeInfraPurgeBackup(target.Root, backupPath, records); err != nil {
			fmt.Fprintf(stderr, "%s: %v. Nothing was deleted\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := appendInfraPurgeLedger(target, ledger, infraPurgeLedgerRecord{
			Format: infraPurgeLedgerFormat, Event: infraPurgeEventBackedUp, Run: run, Type: opts.Type,
			Backup: backupPath, Rows: keys, RecordedAt: now,
		}); err != nil {
			fmt.Fprintf(stderr, "%s: %v. Nothing was deleted; the backup is %s\n", logPrefix, err, backupPath) //nolint:errcheck // best-effort stderr
			return 1
		}
		ledger, err = readInfraPurgeLedger(target)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v. Nothing was deleted; the backup is %s\n", logPrefix, err, backupPath) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	fmt.Fprintf(stdout, "backup: %s (%d row(s) with their edges, proven from disk)\n", backupPath, len(entries)) //nolint:errcheck // best-effort stdout

	if err := infraPurgeAfterBackup(); err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was deleted; run this again to resume from %s\n", logPrefix, err, backupPath) //nolint:errcheck // best-effort stderr
		return 1
	}

	binding, err := openInfraBindingReadOnly(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening binding %s: %v. Nothing was deleted\n", logPrefix, target.Database, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	guardErr := refuseInfraPurgeOfBindingRows(binding, selected)
	_ = closeBeadStoreHandle(binding)
	if guardErr != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was deleted\n", logPrefix, guardErr) //nolint:errcheck // best-effort stderr
		return 1
	}

	if err := deleteInfraPurgeRows(source, entries); err != nil {
		fmt.Fprintf(stderr, "%s: deleting the backed-up rows: %v. Every row is in %s; run this again to finish\n", logPrefix, err, backupPath) //nolint:errcheck // best-effort stderr
		return 1
	}
	if left, err := infraPurgeRowsStillPresent(source, keys); err != nil {
		fmt.Fprintf(stderr, "%s: re-reading the work store after the delete: %v. Every row is in %s; run this again to finish\n", logPrefix, err, backupPath) //nolint:errcheck // best-effort stderr
		return 1
	} else if len(left) > 0 {
		fmt.Fprintf(stderr, "%s: %d row(s) are still in the work store after the delete: %s. Every row is in %s; run this again to finish\n", logPrefix, len(left), infraStrandedIDList(left), backupPath) //nolint:errcheck // best-effort stderr
		return 1
	}
	// A resumed run records every row its run backed up and the work store no
	// longer holds, so rows an interrupted delete already took are counted.
	purgedKeys := keys
	if resumed {
		purgedKeys, err = infraPurgeKeysGone(source, pending.Rows)
		if err != nil {
			fmt.Fprintf(stderr, "%s: re-reading the work store after the delete: %v. Every row is in %s; run this again to record the purge\n", logPrefix, err, backupPath) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if err := appendInfraPurgeLedger(target, ledger, infraPurgeLedgerRecord{
		Format: infraPurgeLedgerFormat, Event: infraPurgeEventPurged, Run: run, Type: opts.Type,
		Backup: backupPath, Rows: purgedKeys, RecordedAt: time.Now().UTC(),
	}); err != nil {
		fmt.Fprintf(stderr, "%s: the %d row(s) were deleted and backed up in %s, but recording the purge failed: %v. Run this again to record it\n", logPrefix, len(selected), backupPath, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "purged: %d stranded %s row(s) deleted from the work store; backup %s\nnext: gc storage status\n", len(selected), opts.Type, backupPath) //nolint:errcheck // best-effort stdout
	return 0
}

// reportInfraPurgeSelection prints what the purge selected: the count, the id
// range, the statuses, the creation dates, the title histogram, and one line
// per row.
func reportInfraPurgeSelection(w io.Writer, purgeType string, class coordclass.Class, rows []beads.Bead, heldDifferent map[string]bool) {
	ids := make([]string, 0, len(rows))
	statuses := map[string]int{}
	titles := map[string]int{}
	ephemeral := 0
	first, last := rows[0].CreatedAt, rows[0].CreatedAt
	for _, b := range rows {
		ids = append(ids, b.ID)
		statuses[b.Status]++
		titles[b.Title]++
		if b.Ephemeral {
			ephemeral++
		}
		if b.CreatedAt.Before(first) {
			first = b.CreatedAt
		}
		if b.CreatedAt.After(last) {
			last = b.CreatedAt
		}
	}
	sort.Strings(ids)
	fmt.Fprintf(w, "selected: %d stranded work-store row(s) of type %s (class %s)\n", len(rows), purgeType, class) //nolint:errcheck // best-effort output
	fmt.Fprintf(w, "  ids:     %s .. %s\n", ids[0], ids[len(ids)-1])                                               //nolint:errcheck // best-effort output
	fmt.Fprintf(w, "  created: %s .. %s\n", first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339))     //nolint:errcheck // best-effort output
	var parts []string
	for _, status := range sortedMapKeys(statuses) {
		parts = append(parts, fmt.Sprintf("%s %d", status, statuses[status]))
	}
	fmt.Fprintf(w, "  status:  %s\n", strings.Join(parts, ", "))                      //nolint:errcheck // best-effort output
	fmt.Fprintf(w, "  tier:    wisp %d, issues %d\n", ephemeral, len(rows)-ephemeral) //nolint:errcheck // best-effort output
	fmt.Fprintln(w, "  titles:")                                                      //nolint:errcheck // best-effort output
	byCount := sortedMapKeys(titles)
	sort.SliceStable(byCount, func(i, j int) bool { return titles[byCount[i]] > titles[byCount[j]] })
	for _, title := range byCount {
		fmt.Fprintf(w, "    %d  %q\n", titles[title], title) //nolint:errcheck // best-effort output
	}
	fmt.Fprintln(w, "  rows:") //nolint:errcheck // best-effort output
	sorted := append([]beads.Bead(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, b := range sorted {
		note := ""
		if heldDifferent[b.ID] {
			note = "  (reused id: the binding holds a different bead under the id, and keeps it)"
		}
		fmt.Fprintf(w, "    %s  created=%s  status=%s  title=%q%s\n", b.ID, b.CreatedAt.UTC().Format(time.RFC3339), b.Status, b.Title, note) //nolint:errcheck // best-effort output
	}
}

// infraPurgeBackupEntries reads every selected row's edges: its own outbound
// edges and every inbound edge from any work-store row, with payloads where
// the store can report them. all is the work store's full listing, which
// answers the inbound edges when the store cannot list relations and carries
// them inline instead.
func infraPurgeBackupEntries(source beads.Store, rows, all []beads.Bead) ([]infraBackupEntry, error) {
	anchors := sourceAnchorIDs(rows)
	reader, err := newSourceDepReader(source, rows[0].ID, anchors)
	if err != nil {
		return nil, err
	}
	var inline map[string][]beads.Dep
	if !reader.relationsOK {
		if !reader.inlineWitnessed {
			return nil, fmt.Errorf("the work store refuses relation listing (%w) and carries no inline dependency projection, so nothing can state the edges a backup must keep", reader.relationsErr)
		}
		inline = map[string][]beads.Dep{}
		for _, b := range all {
			for _, d := range b.Dependencies {
				inline[d.DependsOnID] = append(inline[d.DependsOnID], d)
			}
		}
	}
	entries := make([]infraBackupEntry, 0, len(rows))
	for _, b := range rows {
		entry := infraBackupEntry{Bead: b, Class: coordclass.Classify(b).String()}
		down, ok, err := reader.deps(b)
		if !ok {
			return nil, fmt.Errorf("the edges of %s cannot be read: %w", b.ID, err)
		}
		var up []beads.Dep
		if reader.relationsOK {
			up, err = source.DepList(b.ID, "up")
			if err != nil {
				return nil, fmt.Errorf("listing dependents of %s: %w", b.ID, err)
			}
		} else {
			up = inline[b.ID]
		}
		for _, d := range down {
			edge, err := infraPurgeBackupEdge(source, d)
			if err != nil {
				return nil, err
			}
			entry.Deps = append(entry.Deps, edge)
		}
		for _, d := range up {
			edge, err := infraPurgeBackupEdge(source, d)
			if err != nil {
				return nil, err
			}
			entry.Dependents = append(entry.Dependents, edge)
		}
		sortInfraBackupEdges(entry.Deps)
		sortInfraBackupEdges(entry.Dependents)
		entries = append(entries, entry)
	}
	return entries, nil
}

// infraPurgeBackupEdge records one edge, with its payload when the store can
// report one. A store that has no payload read at all (bd, exec) never stored
// a payload through gc, so the edge is recorded without one.
func infraPurgeBackupEdge(source beads.Store, d beads.Dep) (infraBackupEdge, error) {
	if _, ok := source.(beads.DepMetadataReader); !ok {
		return infraBackupEdge{IssueID: d.IssueID, DependsOnID: d.DependsOnID, Type: d.Type}, nil
	}
	return infraBackupEdgeFor(source, d)
}

// infraPurgeKeys lists the (id, created_at) of rows, sorted by id.
func infraPurgeKeys(rows []beads.Bead) []infraPurgeRowKey {
	keys := make([]infraPurgeRowKey, 0, len(rows))
	for _, b := range rows {
		keys = append(keys, infraPurgeRowKey{ID: b.ID, CreatedAt: b.CreatedAt.UTC()})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys
}

// infraPurgeResumableBackup returns the ledger record of an interrupted run
// when its backup already holds every entry exactly (the row and its edges as
// the work store holds them now), so a resume deletes from the backup it
// proved rather than writing a second one.
func infraPurgeResumableBackup(ledger []infraPurgeLedgerRecord, entries []infraBackupEntry) (infraPurgeLedgerRecord, bool, error) {
	if len(ledger) == 0 || ledger[len(ledger)-1].Event != infraPurgeEventBackedUp {
		return infraPurgeLedgerRecord{}, false, nil
	}
	pending := ledger[len(ledger)-1]
	records, err := readInfraPurgeBackup(pending.Backup)
	if errors.Is(err, os.ErrNotExist) {
		return infraPurgeLedgerRecord{}, false, nil
	}
	if err != nil {
		return infraPurgeLedgerRecord{}, false, fmt.Errorf("the interrupted purge run %s: %w", pending.Run, err)
	}
	held := make(map[string][]byte, len(records))
	for _, record := range records {
		encoded, err := json.Marshal(record.Entry)
		if err != nil {
			return infraPurgeLedgerRecord{}, false, err
		}
		held[record.Entry.Bead.ID] = encoded
	}
	for _, entry := range entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			return infraPurgeLedgerRecord{}, false, err
		}
		if !bytes.Equal(held[entry.Bead.ID], encoded) {
			return infraPurgeLedgerRecord{}, false, nil
		}
	}
	return pending, true, nil
}

// infraPurgeKeysGone returns the keys the work store no longer holds as the
// same bead.
func infraPurgeKeysGone(source beads.Store, keys []infraPurgeRowKey) ([]infraPurgeRowKey, error) {
	left, err := infraPurgeRowsStillPresent(source, keys)
	if err != nil {
		return nil, err
	}
	present := setOf(left)
	gone := make([]infraPurgeRowKey, 0, len(keys))
	for _, key := range keys {
		if !present[key.ID] {
			gone = append(gone, key)
		}
	}
	return gone, nil
}

// refuseInfraPurgeOfBindingRows is the last check before the delete: no row
// the binding holds as the same bead (the same id, created in the same
// second) may be deleted, whatever selected it. A row the binding holds for a
// different bead (a reused id) passes: deleting the work-store row leaves the
// binding's bead alone.
func refuseInfraPurgeOfBindingRows(binding beads.Store, rows []beads.Bead) error {
	var same []string
	for _, b := range rows {
		held, present, err := readInfraHeldRow(binding, b.ID)
		if err != nil {
			return err
		}
		if present && infraHeldRowIsThisBead(b, held) {
			same = append(same, b.ID)
		}
	}
	if len(same) > 0 {
		sort.Strings(same)
		return fmt.Errorf("the binding holds %d selected row(s) as the same bead (the same id and created_at), so they are not stranded and are never purged: %s", len(same), strings.Join(same, ", "))
	}
	return nil
}

// deleteInfraPurgeRows deletes the backed-up rows from the work store: in one
// batch where the store can (bd's --force and SQLite's batch both remove every
// edge touching a deleted row), and otherwise row by row with the edges the
// backup recorded.
func deleteInfraPurgeRows(source beads.Store, entries []infraBackupEntry) error {
	if deleter, ok := source.(beads.BatchDeleter); ok {
		ids := make([]string, 0, len(entries))
		for _, entry := range entries {
			ids = append(ids, entry.Bead.ID)
		}
		err := deleter.DeleteBatch(ids)
		if err == nil || !errors.Is(err, beads.ErrBatchDeleteUnsupported) {
			return err
		}
	}
	for _, entry := range entries {
		if err := removeInfraRowWithEdges(source, entry); err != nil {
			return fmt.Errorf("removing %s: %w", entry.Bead.ID, err)
		}
	}
	return nil
}

// infraPurgeRowsStillPresent re-reads the work store and returns the ids of
// keys it still holds as the same bead.
func infraPurgeRowsStillPresent(source beads.Store, keys []infraPurgeRowKey) ([]string, error) {
	rows, err := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, err
	}
	want := make(map[string]time.Time, len(keys))
	for _, key := range keys {
		want[key.ID] = key.CreatedAt
	}
	var left []string
	for _, b := range rows {
		if at, ok := want[b.ID]; ok && sameCreatedSecond(at, b.CreatedAt) {
			left = append(left, b.ID)
		}
	}
	sort.Strings(left)
	return left, nil
}

// readInfraPurgeBackup reads one purge backup.
func readInfraPurgeBackup(path string) ([]infraRecoverChoiceRecord, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the purge backup %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var records []infraRecoverChoiceRecord
	for decoder.More() {
		var record infraRecoverChoiceRecord
		if err := decoder.Decode(&record); err != nil {
			return nil, fmt.Errorf("reading the purge backup %s: record %d: %w", path, len(records)+1, err)
		}
		if record.Format != infraRecoverChoicesFormat || record.Choice != infraPurgeChoice {
			return nil, fmt.Errorf("reading the purge backup %s: record %d is %q/%q, want %q/%q", path, len(records)+1, record.Format, record.Choice, infraRecoverChoicesFormat, infraPurgeChoice)
		}
		records = append(records, record)
	}
	return records, nil
}

// writeInfraPurgeBackup writes records to path durably (temp file, sync,
// rename, directory sync) and proves them by reading them back. Nothing is
// deleted until it returns nil.
func writeInfraPurgeBackup(dir, path string, records []infraRecoverChoiceRecord) error {
	if err := writeInfraPurgeFile(dir, path, func(encoder *json.Encoder) error {
		for _, record := range records {
			if err := encoder.Encode(record); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("writing the purge backup %s: %w", path, err)
	}
	got, err := readInfraPurgeBackup(path)
	if err != nil {
		return fmt.Errorf("proving the purge backup: %w", err)
	}
	if len(got) != len(records) {
		return fmt.Errorf("proving the purge backup: %s holds %d record(s), want %d", path, len(got), len(records))
	}
	for i := range records {
		want, err := json.Marshal(records[i])
		if err != nil {
			return err
		}
		have, err := json.Marshal(got[i])
		if err != nil {
			return err
		}
		if !bytes.Equal(want, have) {
			return fmt.Errorf("proving the purge backup: %s holds a different %s than the work store", path, records[i].Entry.Bead.ID)
		}
	}
	return nil
}

// writeInfraPurgeFile replaces path with what write encodes, durably: a temp
// file in dir, synced, renamed over path, and the directory synced, so a crash
// leaves either the old file or the new one and never a torn line.
func writeInfraPurgeFile(dir, path string, write func(*json.Encoder) error) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(cause error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return cause
	}
	writer := bufio.NewWriter(tmp)
	if err := write(json.NewEncoder(writer)); err != nil {
		return fail(err)
	}
	if err := writer.Flush(); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := infraMigrationRename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close() //nolint:errcheck // read-only directory handle
	return handle.Sync()
}

// readInfraPurgeLedger returns every ledger record, oldest first. An absent
// ledger holds none.
func readInfraPurgeLedger(target infraBindingTarget) ([]infraPurgeLedgerRecord, error) {
	contents, err := os.ReadFile(target.PurgeLedgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the purge ledger %s: %w", target.PurgeLedgerPath(), err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var records []infraPurgeLedgerRecord
	for decoder.More() {
		var record infraPurgeLedgerRecord
		if err := decoder.Decode(&record); err != nil {
			return nil, fmt.Errorf("reading the purge ledger %s: record %d: %w", target.PurgeLedgerPath(), len(records)+1, err)
		}
		if record.Format != infraPurgeLedgerFormat {
			return nil, fmt.Errorf("reading the purge ledger %s: record %d has format %q, want %q", target.PurgeLedgerPath(), len(records)+1, record.Format, infraPurgeLedgerFormat)
		}
		records = append(records, record)
	}
	return records, nil
}

// appendInfraPurgeLedger adds record to the ledger durably. previous is the
// ledger as this run read it under the migration guard.
func appendInfraPurgeLedger(target infraBindingTarget, previous []infraPurgeLedgerRecord, record infraPurgeLedgerRecord) error {
	err := writeInfraPurgeFile(target.Root, target.PurgeLedgerPath(), func(encoder *json.Encoder) error {
		for _, r := range append(append([]infraPurgeLedgerRecord(nil), previous...), record) {
			if err := encoder.Encode(r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("recording the purge in %s: %w", target.PurgeLedgerPath(), err)
	}
	return nil
}

// reportInfraPurgeLedger is `gc storage status`'s purge readout. It returns
// false when the ledger shows an interrupted purge, which blocks.
func reportInfraPurgeLedger(target infraBindingTarget, stdout io.Writer) bool {
	ledger, err := readInfraPurgeLedger(target)
	if err != nil {
		fmt.Fprintf(stdout, "  purge ledger: unreadable — %v\n", err) //nolint:errcheck // best-effort stdout
		return false
	}
	if len(ledger) == 0 {
		return true
	}
	purged := 0
	var backups []string
	for _, record := range ledger {
		if record.Event == infraPurgeEventPurged {
			purged += len(record.Rows)
			backups = append(backups, record.Backup)
		}
	}
	if purged > 0 {
		fmt.Fprintf(stdout, "  purged stranded: %d bead(s) by --%s (backups: %s)\n", purged, strandedPurgeTypeFlag, strings.Join(backups, ", ")) //nolint:errcheck // best-effort stdout
	}
	if last := ledger[len(ledger)-1]; last.Event == infraPurgeEventBackedUp {
		fmt.Fprintf(stdout, "  purge: INTERRUPTED (run %s backed up %d row(s) to %s and did not finish the delete)\nblocking invariant: finish it with `%s --%s --%s %s --%s`\n", //nolint:errcheck // best-effort stdout
			last.Run, len(last.Rows), last.Backup, storageRecoveryCommand, storageFleetStoppedFlag, strandedPurgeTypeFlag, last.Type, strandedConfirmPurgeFlag)
		return false
	}
	return true
}
