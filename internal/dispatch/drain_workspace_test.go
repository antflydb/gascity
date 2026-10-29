package dispatch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formula"
)

// recordingWorkspaceHook is a SharedDrainWorkspace fake that records every
// drain control it was asked to provision for. With perControl set, each
// control gets its own workspace under path.
type recordingWorkspaceHook struct {
	path       string
	perControl bool
	err        error
	calls      []string
}

func (h *recordingWorkspaceHook) provision(control beads.Bead) (string, error) {
	h.calls = append(h.calls, control.ID)
	if h.perControl && h.err == nil {
		return filepath.Join(h.path, control.ID), nil
	}
	return h.path, h.err
}

// seedSharedExclusiveDrain returns a shared, exclusive-access drain over two
// task members, in convoy order.
func seedSharedExclusiveDrain(t *testing.T) (*beads.MemStore, beads.Bead, []beads.Bead) {
	t.Helper()
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadataBatch(drain.ID, map[string]string{
		"gc.drain_context":          "shared",
		"gc.drain_member_access":    "exclusive",
		"gc.drain_item_single_lane": "true",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(shared exclusive drain): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	members, err := convoycore.Members(store, root.Metadata["gc.input_convoy_id"], false)
	if err != nil {
		t.Fatalf("convoycore.Members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	return store, drain, members
}

// assertItemWorkDir requires the item root and every step under it to carry
// want in both work_dir keys, and wantLender as the drain that lent it.
func assertItemWorkDir(t *testing.T, store beads.Store, rootID, want, wantLender string) {
	t.Helper()
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List beads: %v", err)
	}
	seen := 0
	for _, bead := range all {
		if bead.ID != rootID && bead.ParentID != rootID && bead.Metadata[beadmeta.RootBeadIDMetadataKey] != rootID {
			continue
		}
		seen++
		for key, wantValue := range map[string]string{
			beadmeta.WorkDirMetadataKey:               want,
			beadmeta.LegacyWorkDirMetadataKey:         want,
			beadmeta.DrainWorkspaceControlMetadataKey: wantLender,
		} {
			if got := bead.Metadata[key]; got != wantValue {
				t.Fatalf("bead %s (%s) %s = %q, want %q", bead.ID, bead.Title, key, got, wantValue)
			}
		}
	}
	if seen < 2 {
		t.Fatalf("item root %s has %d beads (root + steps), want at least 2", rootID, seen)
	}
}

// assertMemberWorkspace requires a member's work_dir, and the drain recorded
// as having lent it, to be wantWorkDir and wantLender.
func assertMemberWorkspace(t *testing.T, store beads.Store, memberID, wantWorkDir, wantLender string) {
	t.Helper()
	member := mustGetBead(t, store, memberID)
	if got := member.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != wantWorkDir {
		t.Fatalf("member %s work_dir = %q, want %q", memberID, got, wantWorkDir)
	}
	if got := member.Metadata[beadmeta.DrainWorkspaceControlMetadataKey]; got != wantLender {
		t.Fatalf("member %s %s = %q, want %q", memberID, beadmeta.DrainWorkspaceControlMetadataKey, got, wantLender)
	}
}

// passSharedDrainRow closes a materialized row's item root pass and advances
// the drain past it.
func passSharedDrainRow(t *testing.T, store beads.Store, drainID string, row int, opts ProcessOptions) ControlResult {
	t.Helper()
	manifest := mustDrainManifest(t, mustGetBead(t, store, drainID))
	if err := updateMetadataAndClose(store, manifest.Rows[row].ItemRootID, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("close row %d item root pass: %v", row, err)
	}
	result, err := ProcessControl(store, mustGetBead(t, store, drainID), opts)
	if err != nil {
		t.Fatalf("ProcessControl(after row %d): %v", row, err)
	}
	return result
}

func TestProcessDrainSharedProvisionsWorkspaceOnReservedMember(t *testing.T) {
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain, members := seedSharedExclusiveDrain(t)
	hook := &recordingWorkspaceHook{path: t.TempDir()}

	result, err := ProcessControl(store, drain, ProcessOptions{
		FormulaSearchPaths:   []string{dir},
		SharedDrainWorkspace: hook.provision,
	})
	if err != nil {
		t.Fatalf("ProcessControl(shared first): %v", err)
	}
	if result.Action != "drain-shared-advanced" {
		t.Fatalf("Action = %q, want drain-shared-advanced", result.Action)
	}
	manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	if manifest.Rows[0].MemberID != members[0].ID || manifest.Rows[0].ItemRootID == "" {
		t.Fatalf("rows = %+v, want first member %s materialized", manifest.Rows, members[0].ID)
	}
	if manifest.Rows[0].Workspace != hook.path {
		t.Fatalf("first row workspace = %q, want %q", manifest.Rows[0].Workspace, hook.path)
	}
	assertMemberWorkspace(t, store, members[0].ID, hook.path, drain.ID)
	assertItemWorkDir(t, store, manifest.Rows[0].ItemRootID, hook.path, drain.ID)
}

func TestProcessDrainSharedReusesWorkspaceAndKeepsExistingWorkDir(t *testing.T) {
	t.Run("second member gets the same workspace", func(t *testing.T) {
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		hook := &recordingWorkspaceHook{path: t.TempDir()}
		opts := ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}

		if _, err := ProcessControl(store, drain, opts); err != nil {
			t.Fatalf("ProcessControl(shared first): %v", err)
		}
		manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
		if err := updateMetadataAndClose(store, manifest.Rows[0].ItemRootID, map[string]string{"gc.outcome": "pass"}); err != nil {
			t.Fatalf("close first root pass: %v", err)
		}
		result, err := ProcessControl(store, mustGetBead(t, store, drain.ID), opts)
		if err != nil {
			t.Fatalf("ProcessControl(shared second): %v", err)
		}
		if result.Action != "drain-shared-advanced" {
			t.Fatalf("Action = %q, want drain-shared-advanced", result.Action)
		}
		manifest = mustDrainManifest(t, mustGetBead(t, store, drain.ID))
		if manifest.Rows[1].ItemRootID == "" {
			t.Fatalf("second row = %+v, want materialized", manifest.Rows[1])
		}
		second := mustGetBead(t, store, members[1].ID)
		if got := second.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != hook.path {
			t.Fatalf("second member work_dir = %q, want %q", got, hook.path)
		}
		assertItemWorkDir(t, store, manifest.Rows[1].ItemRootID, hook.path, drain.ID)
		if want := []string{drain.ID, drain.ID}; !slices.Equal(hook.calls, want) {
			t.Fatalf("hook calls = %v, want one per reserved member %v", hook.calls, want)
		}
	})

	t.Run("existing work_dir wins and skips the hook", func(t *testing.T) {
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		existing := t.TempDir()
		if err := store.SetMetadata(members[0].ID, beadmeta.LegacyWorkDirMetadataKey, existing); err != nil {
			t.Fatalf("SetMetadata(existing work_dir): %v", err)
		}
		hook := &recordingWorkspaceHook{path: t.TempDir()}

		if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}); err != nil {
			t.Fatalf("ProcessControl(shared first): %v", err)
		}
		assertMemberWorkspace(t, store, members[0].ID, existing, "")
		manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
		if manifest.Rows[0].Workspace != "" {
			t.Fatalf("first row workspace = %q, want none for a work_dir the drain did not lend", manifest.Rows[0].Workspace)
		}
		assertItemWorkDir(t, store, manifest.Rows[0].ItemRootID, existing, "")
		if len(hook.calls) != 0 {
			t.Fatalf("hook calls = %v, want none for a member that already has work_dir", hook.calls)
		}
	})

	t.Run("crash resume after the write reuses the recorded workspace", func(t *testing.T) {
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		recorded := t.TempDir()
		if err := store.SetMetadataBatch(members[0].ID, map[string]string{
			beadmeta.LegacyWorkDirMetadataKey:             recorded,
			beadmeta.DrainWorkspaceControlMetadataKey:     drain.ID,
			beadmeta.ExclusiveDrainReservationMetadataKey: drain.ID,
		}); err != nil {
			t.Fatalf("SetMetadataBatch(resume state): %v", err)
		}
		hook := &recordingWorkspaceHook{path: t.TempDir()}

		if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}); err != nil {
			t.Fatalf("ProcessControl(resume): %v", err)
		}
		manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
		if manifest.Rows[0].Workspace != recorded {
			t.Fatalf("first row workspace = %q, want the recorded %q", manifest.Rows[0].Workspace, recorded)
		}
		assertItemWorkDir(t, store, manifest.Rows[0].ItemRootID, recorded, drain.ID)
		if len(hook.calls) != 0 {
			t.Fatalf("hook calls = %v, want none on resume", hook.calls)
		}
	})

	t.Run("work_dir another drain lent is provisioned again", func(t *testing.T) {
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		if err := store.SetMetadataBatch(members[0].ID, map[string]string{
			beadmeta.LegacyWorkDirMetadataKey:         t.TempDir(),
			beadmeta.DrainWorkspaceControlMetadataKey: "gc-earlier-drain",
		}); err != nil {
			t.Fatalf("SetMetadataBatch(earlier drain's workspace): %v", err)
		}
		hook := &recordingWorkspaceHook{path: t.TempDir()}

		if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}); err != nil {
			t.Fatalf("ProcessControl(shared first): %v", err)
		}
		if want := []string{drain.ID}; !slices.Equal(hook.calls, want) {
			t.Fatalf("hook calls = %v, want %v", hook.calls, want)
		}
		assertMemberWorkspace(t, store, members[0].ID, hook.path, drain.ID)
		manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
		assertItemWorkDir(t, store, manifest.Rows[0].ItemRootID, hook.path, drain.ID)
	})

	t.Run("observability mirror alone does not count as a workspace", func(t *testing.T) {
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		if err := store.SetMetadata(members[0].ID, beadmeta.WorkDirMetadataKey, t.TempDir()); err != nil {
			t.Fatalf("SetMetadata(gc.work_dir mirror): %v", err)
		}
		hook := &recordingWorkspaceHook{path: t.TempDir()}

		if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}); err != nil {
			t.Fatalf("ProcessControl(shared first): %v", err)
		}
		if len(hook.calls) != 1 {
			t.Fatalf("hook calls = %v, want one for a member with only gc.work_dir", hook.calls)
		}
		assertMemberWorkspace(t, store, members[0].ID, hook.path, drain.ID)
	})
}

func TestProcessDrainWorkspaceHookSkippedOutsideSharedExclusive(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		nilHook  bool
	}{
		{name: "shared read access", metadata: map[string]string{"gc.drain_member_access": "read"}},
		{name: "separate context", metadata: map[string]string{"gc.drain_context": "separate"}},
		{name: "nil hook", nilHook: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeDrainItemFormula(t, dir)
			store, drain, members := seedSharedExclusiveDrain(t)
			if len(tc.metadata) > 0 {
				if err := store.SetMetadataBatch(drain.ID, tc.metadata); err != nil {
					t.Fatalf("SetMetadataBatch(%s): %v", tc.name, err)
				}
				drain = mustGetBead(t, store, drain.ID)
			}
			hook := &recordingWorkspaceHook{path: t.TempDir()}
			opts := ProcessOptions{FormulaSearchPaths: []string{dir}}
			if !tc.nilHook {
				opts.SharedDrainWorkspace = hook.provision
			}

			if _, err := ProcessControl(store, drain, opts); err != nil {
				t.Fatalf("ProcessControl(%s): %v", tc.name, err)
			}
			if len(hook.calls) != 0 {
				t.Fatalf("hook calls = %v, want none", hook.calls)
			}
			for _, member := range members {
				assertMemberWorkspace(t, store, member.ID, "", "")
			}
		})
	}
}

// TestProcessDrainSharedWorkspaceFailureClosesHardAndReleases pins that any
// provisioning error closes the drain hard and visibly, including git output
// that names a network failure and a git call that ran out of time: the
// store-outage classifier would read those as an outage to wait out, but they
// come from git, and retrying them would hold the drain open with nothing
// recorded.
func TestProcessDrainSharedWorkspaceFailureClosesHardAndReleases(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		// outage: the store-outage classifier reads err as an outage.
		outage bool
	}{
		{name: "refusal", err: errors.New("worktree root is inside the rig checkout")},
		{name: "git output naming a refused connection", err: errors.New("git worktree add: exit status 128: Error downloading object: big.bin: Smudge error: dial tcp 127.0.0.1:443: connect: connection refused"), outage: true},
		{name: "git call out of time", err: fmt.Errorf("git worktree add: %w", context.DeadlineExceeded), outage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientControllerError(tc.err); got != tc.outage {
				t.Fatalf("IsTransientControllerError(%v) = %v, want %v", tc.err, got, tc.outage)
			}
			dir := t.TempDir()
			writeDrainItemFormula(t, dir)
			store, drain, members := seedSharedExclusiveDrain(t)
			hook := &recordingWorkspaceHook{err: tc.err}

			result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision})
			if err != nil {
				t.Fatalf("ProcessControl(workspace failure): %v", err)
			}
			if result.Action != "drain-failed" {
				t.Fatalf("Action = %q, want drain-failed", result.Action)
			}
			drain = mustGetBead(t, store, drain.ID)
			if drain.Status != "closed" {
				t.Fatalf("drain status = %q, want closed", drain.Status)
			}
			for key, want := range map[string]string{
				"gc.outcome":         "fail",
				"gc.failure_class":   "hard",
				"gc.failure_reason":  "shared_workspace_unavailable",
				"gc.failure_subject": members[0].ID,
			} {
				if got := drain.Metadata[key]; got != want {
					t.Fatalf("drain %s = %q, want %q", key, got, want)
				}
			}
			if got := drain.Metadata[beadmeta.ControllerErrorMetadataKey]; !strings.Contains(got, tc.err.Error()) {
				t.Fatalf("gc.controller_error = %q, want the hook error %q", got, tc.err)
			}
			if got := drain.Metadata[beadmeta.ControllerErrorClassMetadataKey]; got != beadmeta.FailureClassHard {
				t.Fatalf("gc.controller_error_class = %q, want hard", got)
			}
			manifest := mustDrainManifest(t, drain)
			for _, row := range manifest.Rows {
				if row.ItemRootID != "" {
					t.Fatalf("row = %+v, want no item root", row)
				}
				if row.Status != "failed" || row.Failure != "shared_workspace_unavailable" {
					t.Fatalf("row = %+v, want failed shared_workspace_unavailable", row)
				}
			}
			for _, member := range members {
				after := mustGetBead(t, store, member.ID)
				if got := strings.TrimSpace(after.Metadata[beadmeta.ExclusiveDrainReservationMetadataKey]); got != "" {
					t.Fatalf("member %s reservation = %q, want released", member.ID, got)
				}
				assertMemberWorkspace(t, store, member.ID, "", "")
			}
		})
	}
}

func TestProcessDrainSharedWorkspaceMemberWriteErrorKeepsDrainOpen(t *testing.T) {
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	mem, drain, members := seedSharedExclusiveDrain(t)
	// An availability-tier error: the control dispatcher retries it rather
	// than quarantining the drain.
	writeErr := errors.New("dial tcp 127.0.0.1:3306: connect: connection refused")
	store := &failOnceMetadataKeyStore{Store: mem, key: beadmeta.LegacyWorkDirMetadataKey, err: writeErr}
	hook := &recordingWorkspaceHook{path: t.TempDir()}

	_, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision})
	if !errors.Is(err, writeErr) {
		t.Fatalf("ProcessControl err = %v, want wrapped %v", err, writeErr)
	}
	if !IsTransientControllerError(err) {
		t.Fatalf("ProcessControl err = %v, want the dispatcher to classify it transient", err)
	}
	if !strings.Contains(err.Error(), members[0].ID) {
		t.Fatalf("ProcessControl err = %v, want it to name member %s", err, members[0].ID)
	}
	drain = mustGetBead(t, mem, drain.ID)
	if drain.Status == "closed" {
		t.Fatalf("drain closed with outcome %q, want it open for retry", drain.Metadata["gc.outcome"])
	}
	if raw := drain.Metadata[drainManifestMetadataKey]; raw != "" {
		for _, row := range mustDrainManifest(t, drain).Rows {
			if row.ItemRootID != "" {
				t.Fatalf("row = %+v, want no item root", row)
			}
		}
	}
	if got := mustGetBead(t, mem, members[0].ID).Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != "" {
		t.Fatalf("first member work_dir = %q, want unwritten", got)
	}

	result, err := ProcessControl(store, mustGetBead(t, mem, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision})
	if err != nil {
		t.Fatalf("ProcessControl(retry): %v", err)
	}
	if result.Action != "drain-shared-advanced" {
		t.Fatalf("retry Action = %q, want drain-shared-advanced", result.Action)
	}
	manifest := mustDrainManifest(t, mustGetBead(t, mem, drain.ID))
	assertItemWorkDir(t, mem, manifest.Rows[0].ItemRootID, hook.path, drain.ID)
}

// TestProcessDrainSharedReleasesLentWorkspaceOnTerminalClose pins that every
// terminal close of a shared drain takes back the workspace it lent to the
// members still open: each one's work_dir and the key naming the drain are
// cleared before the reservation is released. A work_dir the drain did not
// lend, one replaced since, and the one a member its item closed holds are
// left as they are.
func TestProcessDrainSharedReleasesLentWorkspaceOnTerminalClose(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		hook := &recordingWorkspaceHook{path: t.TempDir()}
		opts := ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}

		if _, err := ProcessControl(store, drain, opts); err != nil {
			t.Fatalf("ProcessControl(shared first): %v", err)
		}
		passSharedDrainRow(t, store, drain.ID, 0, opts)
		if result := passSharedDrainRow(t, store, drain.ID, 1, opts); result.Action != "drain-succeeded" {
			t.Fatalf("Action = %q, want drain-succeeded", result.Action)
		}
		for _, member := range members {
			assertMemberWorkspace(t, store, member.ID, "", "")
		}
		for _, row := range mustDrainManifest(t, mustGetBead(t, store, drain.ID)).Rows {
			if row.Workspace != hook.path {
				t.Fatalf("row %d workspace = %q, want %q kept in the manifest", row.Index, row.Workspace, hook.path)
			}
		}
	})

	t.Run("a work_dir the drain did not lend survives", func(t *testing.T) {
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		operator := t.TempDir()
		if err := store.SetMetadata(members[0].ID, beadmeta.LegacyWorkDirMetadataKey, operator); err != nil {
			t.Fatalf("SetMetadata(operator work_dir): %v", err)
		}
		hook := &recordingWorkspaceHook{path: t.TempDir()}
		opts := ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}

		if _, err := ProcessControl(store, drain, opts); err != nil {
			t.Fatalf("ProcessControl(shared first): %v", err)
		}
		passSharedDrainRow(t, store, drain.ID, 0, opts)
		assertMemberWorkspace(t, store, members[1].ID, hook.path, drain.ID)
		// A pack step that prepares its own worktree replaces the lent
		// work_dir while the item runs, leaving the lending drain recorded.
		replaced := t.TempDir()
		if err := store.SetMetadata(members[1].ID, beadmeta.LegacyWorkDirMetadataKey, replaced); err != nil {
			t.Fatalf("SetMetadata(replaced work_dir): %v", err)
		}
		if result := passSharedDrainRow(t, store, drain.ID, 1, opts); result.Action != "drain-succeeded" {
			t.Fatalf("Action = %q, want drain-succeeded", result.Action)
		}
		assertMemberWorkspace(t, store, members[0].ID, operator, "")
		assertMemberWorkspace(t, store, members[1].ID, replaced, "")
		rows := mustDrainManifest(t, mustGetBead(t, store, drain.ID)).Rows
		if rows[0].Workspace != "" || rows[1].Workspace != hook.path {
			t.Fatalf("row workspaces = %q, %q, want none for the operator's and %q lent", rows[0].Workspace, rows[1].Workspace, hook.path)
		}
	})

	t.Run("a member its item closed keeps the worktree", func(t *testing.T) {
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		hook := &recordingWorkspaceHook{path: t.TempDir()}
		opts := ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}

		if _, err := ProcessControl(store, drain, opts); err != nil {
			t.Fatalf("ProcessControl(shared first): %v", err)
		}
		// Each item closes its member on success, as the item agent closes its
		// source anchor, before its root closes.
		for row, member := range members {
			if err := store.Close(member.ID); err != nil {
				t.Fatalf("close member %s: %v", member.ID, err)
			}
			result := passSharedDrainRow(t, store, drain.ID, row, opts)
			if row == len(members)-1 && result.Action != "drain-succeeded" {
				t.Fatalf("Action = %q, want drain-succeeded", result.Action)
			}
		}
		for _, member := range members {
			assertMemberWorkspace(t, store, member.ID, hook.path, drain.ID)
			if got := mustGetBead(t, store, member.ID).Metadata[beadmeta.ExclusiveDrainReservationMetadataKey]; got != "" {
				t.Fatalf("member %s reservation = %q, want released", member.ID, got)
			}
		}
	})

	t.Run("hard failure", func(t *testing.T) {
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		hook := &recordingWorkspaceHook{path: t.TempDir()}
		opts := ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}

		if _, err := ProcessControl(store, drain, opts); err != nil {
			t.Fatalf("ProcessControl(shared first): %v", err)
		}
		hook.err = errors.New("worktree root is inside the rig checkout")
		if result := passSharedDrainRow(t, store, drain.ID, 0, opts); result.Action != "drain-failed" {
			t.Fatalf("Action = %q, want drain-failed", result.Action)
		}
		assertMemberWorkspace(t, store, members[0].ID, "", "")
		assertMemberWorkspace(t, store, members[1].ID, "", "")
	})

	t.Run("reservation conflict", func(t *testing.T) {
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		hook := &recordingWorkspaceHook{path: t.TempDir()}
		opts := ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}

		if _, err := ProcessControl(store, drain, opts); err != nil {
			t.Fatalf("ProcessControl(shared first): %v", err)
		}
		if err := store.SetMetadata(members[1].ID, beadmeta.ExclusiveDrainReservationMetadataKey, "gc-other-drain"); err != nil {
			t.Fatalf("SetMetadata(competing reservation): %v", err)
		}
		if result := passSharedDrainRow(t, store, drain.ID, 0, opts); result.Action != "drain-reservation-failed" {
			t.Fatalf("Action = %q, want drain-reservation-failed", result.Action)
		}
		assertMemberWorkspace(t, store, members[0].ID, "", "")
		if got := mustGetBead(t, store, members[0].ID).Metadata[beadmeta.ExclusiveDrainReservationMetadataKey]; got != "" {
			t.Fatalf("first member reservation = %q, want released", got)
		}
	})
}

// TestProcessDrainSharedRerunWorksInItsOwnWorkspace re-runs a shared drain
// over a convoy whose first item failed. The re-run must work every item in
// its own workspace, not leave the failed member in the earlier drain's.
func TestProcessDrainSharedRerunWorksInItsOwnWorkspace(t *testing.T) {
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, first, members := seedSharedExclusiveDrain(t)
	hook := &recordingWorkspaceHook{path: t.TempDir(), perControl: true}
	opts := ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}

	if _, err := ProcessControl(store, first, opts); err != nil {
		t.Fatalf("ProcessControl(first drain): %v", err)
	}
	manifest := mustDrainManifest(t, mustGetBead(t, store, first.ID))
	if err := updateMetadataAndClose(store, manifest.Rows[0].ItemRootID, map[string]string{
		"gc.outcome":        "fail",
		"gc.failure_reason": "unit_failed",
	}); err != nil {
		t.Fatalf("close first item root fail: %v", err)
	}
	result, err := ProcessControl(store, mustGetBead(t, store, first.ID), opts)
	if err != nil {
		t.Fatalf("ProcessControl(first drain failure): %v", err)
	}
	if result.Action != "drain-failed" {
		t.Fatalf("Action = %q, want drain-failed", result.Action)
	}
	assertMemberWorkspace(t, store, members[0].ID, "", "")

	rerun, err := store.Create(beads.Bead{
		Title: "drain rerun",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       "drain",
			beadmeta.RootBeadIDMetadataKey: first.Metadata[beadmeta.RootBeadIDMetadataKey],
			"gc.drain_context":             "shared",
			"gc.drain_formula":             "drain-item",
			"gc.drain_member_access":       "exclusive",
			"gc.drain_item_single_lane":    "true",
		},
	})
	if err != nil {
		t.Fatalf("create rerun drain: %v", err)
	}
	want := filepath.Join(hook.path, rerun.ID)
	if _, err := ProcessControl(store, rerun, opts); err != nil {
		t.Fatalf("ProcessControl(rerun): %v", err)
	}
	for i, member := range members {
		if i > 0 {
			passSharedDrainRow(t, store, rerun.ID, i-1, opts)
		}
		row := mustDrainManifest(t, mustGetBead(t, store, rerun.ID)).Rows[i]
		if row.MemberID != member.ID || row.ItemRootID == "" {
			t.Fatalf("rerun row %d = %+v, want member %s materialized", i, row, member.ID)
		}
		assertMemberWorkspace(t, store, member.ID, want, rerun.ID)
		assertItemWorkDir(t, store, row.ItemRootID, want, rerun.ID)
	}
	if want := []string{first.ID, rerun.ID, rerun.ID}; !slices.Equal(hook.calls, want) {
		t.Fatalf("hook calls = %v, want %v", hook.calls, want)
	}
}

// TestProcessDrainSharedItemCheckRunsRigScriptInWorkspace drives a shared
// exclusive drain item whose step has a relative exec check. The item's ralph
// control inherits the drain workspace as its work_dir and runs there, but its
// script resolves from the store root: a copy the item's worker plants at the
// same path in the workspace never runs, whether the workspace lies outside
// the city and the store or under the city, where a work_dir's own copy would
// otherwise win (gastownhall/gascity#3008).
func TestProcessDrainSharedItemCheckRunsRigScriptInWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace func(parent string) string
	}{
		{name: "workspace outside the city and the store", workspace: func(parent string) string {
			return filepath.Join(parent, "worktrees", "rig", "drain-workspace")
		}},
		{name: "workspace under the city", workspace: func(parent string) string {
			return filepath.Join(parent, "city", ".gc", "worktrees", "rig", "drain-workspace")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			cityPath := filepath.Join(parent, "city")
			storePath := filepath.Join(parent, "rig")
			workspace := tc.workspace(parent)
			checkRel := filepath.Join(".gc", "scripts", "checks", "verify.sh")
			formulaDir := filepath.Join(parent, "formulas")
			for _, d := range []string{cityPath, filepath.Dir(filepath.Join(storePath, checkRel)), filepath.Dir(filepath.Join(workspace, checkRel)), formulaDir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", d, err)
				}
			}
			marker := filepath.Join(parent, "check-cwd")
			if err := os.WriteFile(filepath.Join(storePath, checkRel), []byte("#!/bin/sh\npwd -P > '"+marker+"'\n"), 0o755); err != nil {
				t.Fatalf("write rig check script: %v", err)
			}
			planted := filepath.Join(parent, "planted-ran")
			if err := os.WriteFile(filepath.Join(workspace, checkRel), []byte("#!/bin/sh\ntouch '"+planted+"'\nexit 0\n"), 0o755); err != nil {
				t.Fatalf("write planted check script: %v", err)
			}
			itemFormula := `formula = "drain-item"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work {{convoy_id}}"

[steps.check]
max_attempts = 1

[steps.check.check]
mode = "exec"
path = ".gc/scripts/checks/verify.sh"
timeout = "30s"
`
			if err := os.WriteFile(filepath.Join(formulaDir, "drain-item.formula.toml"), []byte(itemFormula), 0o644); err != nil {
				t.Fatalf("write drain item formula: %v", err)
			}
			store, drain, _ := seedSharedExclusiveDrain(t)
			hook := &recordingWorkspaceHook{path: workspace}
			opts := ProcessOptions{
				CityPath:             cityPath,
				StorePath:            storePath,
				FormulaSearchPaths:   []string{formulaDir},
				SharedDrainWorkspace: hook.provision,
			}

			if _, err := ProcessControl(store, drain, opts); err != nil {
				t.Fatalf("ProcessControl(shared first): %v", err)
			}
			itemRootID := mustDrainManifest(t, mustGetBead(t, store, drain.ID)).Rows[0].ItemRootID
			all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			if err != nil {
				t.Fatalf("List beads: %v", err)
			}
			var attempt, check beads.Bead
			for _, bead := range all {
				if bead.Metadata[beadmeta.RootBeadIDMetadataKey] != itemRootID {
					continue
				}
				switch {
				case bead.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindRalph:
					check = bead
				case bead.Metadata[beadmeta.StepRefMetadataKey] == "work.iteration.1":
					attempt = bead
				}
			}
			if attempt.ID == "" || check.ID == "" {
				t.Fatalf("item %s attempt=%q check=%q, want both materialized", itemRootID, attempt.ID, check.ID)
			}
			if got := check.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != workspace {
				t.Fatalf("check work_dir = %q, want the drain workspace %q", got, workspace)
			}
			if err := updateMetadataAndClose(store, attempt.ID, map[string]string{"gc.outcome": "pass"}); err != nil {
				t.Fatalf("close attempt pass: %v", err)
			}

			if _, err := ProcessControl(store, mustGetBead(t, store, check.ID), opts); err != nil {
				t.Fatalf("ProcessControl(item check): %v", err)
			}
			check = mustGetBead(t, store, check.ID)
			if check.Status != "closed" || check.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
				t.Fatalf("check status=%q outcome=%q, want closed pass", check.Status, check.Metadata[beadmeta.OutcomeMetadataKey])
			}
			if _, err := os.Stat(planted); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stat planted marker: %v, want the copy planted in the workspace never to run", err)
			}
			ran, err := os.ReadFile(marker)
			if err != nil {
				t.Fatalf("read rig check marker: %v", err)
			}
			wantCwd, err := filepath.EvalSymlinks(workspace)
			if err != nil {
				t.Fatalf("EvalSymlinks(%s): %v", workspace, err)
			}
			if got := strings.TrimSpace(string(ran)); got != wantCwd {
				t.Fatalf("check ran in %q, want the drain workspace %q", got, wantCwd)
			}
		})
	}
}

// TestRunRalphCheckResolvesDrainWorkspaceScriptFromStore pins that a work_dir
// a shared drain lent (gc.drain_workspace_control, on the check or inherited
// from its item root) is only the check's cwd: the relative script resolves
// from the store, never from the workspace, and the workspace may lie outside
// the city and the store.
func TestRunRalphCheckResolvesDrainWorkspaceScriptFromStore(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace func(parent string) string
		onRoot    bool
	}{
		{name: "outside the city and the store", workspace: func(parent string) string { return filepath.Join(parent, "worktrees", "rig", "ws") }},
		{name: "under the city", workspace: func(parent string) string { return filepath.Join(parent, "city", ".gc", "worktrees", "rig", "ws") }},
		{name: "lender inherited from the item root", workspace: func(parent string) string { return filepath.Join(parent, "city", ".gc", "worktrees", "rig", "ws") }, onRoot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			cityPath := filepath.Join(parent, "city")
			storePath := filepath.Join(parent, "rig")
			workspace := tc.workspace(parent)
			for _, dir := range []string{cityPath, storePath, workspace} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}
			if err := os.WriteFile(filepath.Join(storePath, "check.sh"), []byte("#!/bin/sh\necho \"store $GC_WORK_DIR\"\n"), 0o755); err != nil {
				t.Fatalf("write store check script: %v", err)
			}
			if err := os.WriteFile(filepath.Join(workspace, "check.sh"), []byte("#!/bin/sh\necho planted\n"), 0o755); err != nil {
				t.Fatalf("write planted check script: %v", err)
			}
			store := beads.NewMemStore()
			lent := map[string]string{beadmeta.DrainWorkspaceControlMetadataKey: "gc-drain"}
			rootMetadata := map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}
			checkMetadata := map[string]string{
				"gc.check_path":    "check.sh",
				"gc.work_dir":      workspace,
				"gc.check_timeout": "30s",
			}
			if tc.onRoot {
				maps.Copy(rootMetadata, lent)
			} else {
				maps.Copy(checkMetadata, lent)
			}
			root := mustCreate(t, store, beads.Bead{Title: "item root", Metadata: rootMetadata})
			checkMetadata[beadmeta.RootBeadIDMetadataKey] = root.ID
			check := mustCreate(t, store, beads.Bead{Title: "check", Type: "task", Metadata: checkMetadata})

			result, err := runRalphCheck(store, check, beads.Bead{ID: "gc-attempt", Type: "task"}, 1, ProcessOptions{
				CityPath:  cityPath,
				StorePath: storePath,
			})
			if err != nil {
				t.Fatalf("runRalphCheck: %v", err)
			}
			if result.Outcome != convergence.GatePass {
				t.Fatalf("Outcome = %q (stderr=%q), want pass", result.Outcome, result.Stderr)
			}
			if got, want := strings.TrimSpace(result.Stdout), "store "+workspace; got != want {
				t.Fatalf("stdout = %q, want %q: the store's script, run in the workspace", got, want)
			}
		})
	}
}

func TestStampDrainItemRecipePrefersLegacyWorkDir(t *testing.T) {
	recipe := &formula.Recipe{Steps: []formula.RecipeStep{
		{ID: "drain-item", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
		{ID: "drain-item.work"},
	}}
	control := beads.Bead{ID: "gc-drain", Metadata: map[string]string{"gc.drain_member_access": "exclusive"}}
	member := beads.Bead{ID: "gc-member", Metadata: map[string]string{
		beadmeta.LegacyWorkDirMetadataKey: "/work/creator",
		beadmeta.WorkDirMetadataKey:       "/work/mirror",
	}}
	row := &drainManifestRow{Index: 0, MemberID: member.ID}

	stampDrainItemRecipe(recipe, control, beads.Bead{ID: "gc-unit"}, member, 1, row, "drain-item", nil)

	for _, step := range recipe.Steps {
		for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
			if got := step.Metadata[key]; got != "/work/creator" {
				t.Fatalf("step %s %s = %q, want the creator's work_dir %q", step.ID, key, got, "/work/creator")
			}
		}
	}
}
