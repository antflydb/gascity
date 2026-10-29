package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worktree"
)

// sharedDrainWorkspaceCity returns a city directory and a config with one rig
// named alpha at rigPath.
func sharedDrainWorkspaceCity(t *testing.T, rigPath string) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "alpha", Path: rigPath}},
	}
	return cityPath, cfg
}

func sharedDrainControl(rootID, drainID string) beads.Bead {
	return beads.Bead{ID: drainID, Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID}}
}

func TestSharedDrainWorkspaceProvisionerEnsuresManagedRigWorktree(t *testing.T) {
	repo, _ := testutil.InitGitRepo(t)
	worktreesRoot := t.TempDir()
	t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
	cityPath, cfg := sharedDrainWorkspaceCity(t, repo)
	control := sharedDrainControl("gc-root1", "gc-drain1")
	provision := sharedDrainWorkspaceProvisioner(cityPath, repo, cfg)

	path, err := provision(control)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	root := filepath.Join(worktreesRoot, "alpha")
	wantPath := filepath.Join(canonicalTestPath(root), "gc-root1-gc-drain1")
	if path != wantPath {
		t.Fatalf("path = %q, want %q", path, wantPath)
	}
	if path != canonicalTestPath(path) {
		t.Fatalf("path = %q, want canonical", path)
	}
	if got := testutil.RunGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "gc/drain/gc-drain1" {
		t.Fatalf("worktree branch = %q, want gc/drain/gc-drain1", got)
	}
	if got, want := testutil.RunGit(t, path, "rev-parse", "HEAD"), testutil.RunGit(t, repo, "rev-parse", "HEAD"); got != want {
		t.Fatalf("worktree HEAD = %s, want rig HEAD %s", got, want)
	}
	spec := worktree.Spec{
		RepoDir:    repo,
		Path:       filepath.Join(root, "gc-root1-gc-drain1"),
		Root:       root,
		Branch:     "gc/drain/gc-drain1",
		Base:       "HEAD",
		BeadID:     "gc-drain1",
		StoreRef:   "rig:alpha",
		Creator:    sharedDrainWorkspaceOwner,
		Owner:      sharedDrainWorkspaceOwner,
		Generation: "gc-drain1",
		Lifecycle:  worktree.LifecycleActive,
	}
	verified, err := worktree.Verify(spec)
	if err != nil {
		t.Fatalf("worktree.Verify: %v", err)
	}
	if verified.Provenance == nil || verified.Provenance.BeadID != "gc-drain1" || verified.Provenance.StoreRef != "rig:alpha" {
		t.Fatalf("provenance = %+v, want bead gc-drain1 store ref rig:alpha", verified.Provenance)
	}
	attempt := verified.Provenance.AttemptID

	// A commit on the rig's HEAD between calls does not break re-verification.
	testutil.RunGit(t, repo, "commit", "--allow-empty", "-m", "rig moved")
	again, err := provision(control)
	if err != nil {
		t.Fatalf("provision again: %v", err)
	}
	if again != path {
		t.Fatalf("second path = %q, want %q", again, path)
	}
	reverified, err := worktree.Verify(spec)
	if err != nil {
		t.Fatalf("worktree.Verify after second call: %v", err)
	}
	if reverified.Provenance.AttemptID != attempt {
		t.Fatalf("attempt id = %q after re-call, want unchanged %q", reverified.Provenance.AttemptID, attempt)
	}

	testutil.RunGit(t, repo, "worktree", "remove", path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat removed worktree: %v, want not exist", err)
	}
	recreated, err := provision(control)
	if err != nil {
		t.Fatalf("provision after worktree remove: %v", err)
	}
	if recreated != path {
		t.Fatalf("recreated path = %q, want %q", recreated, path)
	}
	if got := testutil.RunGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "gc/drain/gc-drain1" {
		t.Fatalf("recreated worktree branch = %q, want gc/drain/gc-drain1", got)
	}

	// An agent switching branches inside the shared worktree makes the next
	// call fail closed rather than adopt the foreign checkout.
	testutil.RunGit(t, path, "switch", "-c", "elsewhere")
	if _, err := provision(control); err == nil {
		t.Fatal("provision after branch switch succeeded, want a refusal")
	}
}

func TestSharedDrainWorkspaceProvisionerSkipsAndRefuses(t *testing.T) {
	t.Run("city scope", func(t *testing.T) {
		repo, _ := testutil.InitGitRepo(t)
		worktreesRoot := t.TempDir()
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
		cityPath, cfg := sharedDrainWorkspaceCity(t, repo)

		path, err := sharedDrainWorkspaceProvisioner(cityPath, cityPath, cfg)(sharedDrainControl("gc-root2", "gc-drain2"))
		if err != nil || path != "" {
			t.Fatalf("provision = (%q, %v), want (\"\", nil)", path, err)
		}
		assertDirEmpty(t, worktreesRoot)
	})

	t.Run("rig that is not a git repository", func(t *testing.T) {
		rig := t.TempDir()
		worktreesRoot := t.TempDir()
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
		cityPath, cfg := sharedDrainWorkspaceCity(t, rig)

		path, err := sharedDrainWorkspaceProvisioner(cityPath, rig, cfg)(sharedDrainControl("gc-root2", "gc-drain2"))
		if err != nil || path != "" {
			t.Fatalf("provision = (%q, %v), want (\"\", nil)", path, err)
		}
		assertDirEmpty(t, worktreesRoot)
	})

	t.Run("worktrees root inside the rig checkout", func(t *testing.T) {
		repo, _ := testutil.InitGitRepo(t)
		worktreesRoot := filepath.Join(repo, ".worktrees")
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
		cityPath, cfg := sharedDrainWorkspaceCity(t, repo)

		_, err := sharedDrainWorkspaceProvisioner(cityPath, repo, cfg)(sharedDrainControl("gc-root2", "gc-drain2"))
		if err == nil {
			t.Fatal("provision succeeded, want a refusal for a root inside the rig checkout")
		}
		for _, want := range []string{worktreesRoot, "GC_WORKTREES_DIR", "T3CODE_WORKTREES_DIR", "T3CODE_HOME"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to name %q", err, want)
			}
		}
	})

	t.Run("control without a workflow root", func(t *testing.T) {
		repo, _ := testutil.InitGitRepo(t)
		t.Setenv("GC_WORKTREES_DIR", t.TempDir())
		cityPath, cfg := sharedDrainWorkspaceCity(t, repo)

		_, err := sharedDrainWorkspaceProvisioner(cityPath, repo, cfg)(beads.Bead{ID: "gc-drain1"})
		if err == nil || !strings.Contains(err.Error(), "gc-drain1") {
			t.Fatalf("provision err = %v, want an error naming the control", err)
		}
	})
}

func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s has %d entries, want none", dir, len(entries))
	}
}

// drainItemWorkFormula is a drain item formula with one worker step.
const drainItemWorkFormula = `formula = "drain-item"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work {{convoy_id}}"
metadata = { "gc.run_target" = "worker" }
`

// sharedDrainDispatchFixture is a rig-scoped shared exclusive drain over two
// task members, ready for the real control dispatcher. The rig alpha is a git
// repository.
type sharedDrainDispatchFixture struct {
	cityPath      string
	repo          string
	worktreesRoot string
	cfg           *config.City
	store         *beads.MemStore
	rootID        string
	drainID       string
	memberIDs     []string
}

// newSharedDrainDispatchFixture seeds the fixture with itemFormula as the
// drain's item formula. With externalRoot the worktrees root
// (GC_WORKTREES_DIR) lies outside both the city and the rig; without it the
// root is the default <city>/.gc/worktrees.
func newSharedDrainDispatchFixture(t *testing.T, itemFormula string, externalRoot bool) sharedDrainDispatchFixture {
	t.Helper()
	repo, _ := testutil.InitGitRepo(t)
	cityPath, cfg := sharedDrainWorkspaceCity(t, repo)
	for _, key := range []string{"GC_WORKTREES_DIR", "T3CODE_WORKTREES_DIR", "T3CODE_HOME"} {
		t.Setenv(key, "")
	}
	worktreesRoot := filepath.Join(cityPath, ".gc", "worktrees")
	if externalRoot {
		worktreesRoot = t.TempDir()
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
	}
	cfg.Agents = []config.Agent{
		{Name: config.ControlDispatcherAgentName, Dir: "alpha"},
		{Name: "worker", Dir: "alpha"},
	}
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "drain-item.formula.toml"), []byte(itemFormula), 0o644); err != nil {
		t.Fatalf("write drain item formula: %v", err)
	}
	cfg.FormulaLayers.City = []string{formulaDir}
	store := beads.NewMemStore()

	convoy, err := store.Create(beads.Bead{
		Title:    "input convoy",
		Type:     "convoy",
		Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
	})
	if err != nil {
		t.Fatalf("create input convoy: %v", err)
	}
	var memberIDs []string
	for _, title := range []string{"first", "second"} {
		member, err := store.Create(beads.Bead{Title: title, Type: "task"})
		if err != nil {
			t.Fatalf("create member: %v", err)
		}
		if err := convoycore.TrackItem(store, convoy.ID, member.ID); err != nil {
			t.Fatalf("track member: %v", err)
		}
		memberIDs = append(memberIDs, member.ID)
	}
	root, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
			"gc.formula_contract":    "graph.v2",
			"gc.input_convoy_id":     convoy.ID,
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	drain, err := store.Create(beads.Bead{
		Title: "drain",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       "drain",
			beadmeta.RootBeadIDMetadataKey: root.ID,
			"gc.drain_context":             "shared",
			"gc.drain_formula":             "drain-item",
			"gc.drain_member_access":       "exclusive",
			"gc.drain_item_single_lane":    "true",
		},
	})
	if err != nil {
		t.Fatalf("create drain control: %v", err)
	}
	return sharedDrainDispatchFixture{
		cityPath:      cityPath,
		repo:          repo,
		worktreesRoot: worktreesRoot,
		cfg:           cfg,
		store:         store,
		rootID:        root.ID,
		drainID:       drain.ID,
		memberIDs:     memberIDs,
	}
}

// dispatch runs the real control dispatcher over one control bead.
func (f sharedDrainDispatchFixture) dispatch(t *testing.T, beadID string) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(f.cityPath, f.repo, f.store, beadID, f.cfg, &stdout, &stderr); err != nil {
		t.Fatalf("control dispatch %s: %v; stderr=%q", beadID, err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// workspace is the drain's managed rig worktree.
func (f sharedDrainDispatchFixture) workspace() string {
	return filepath.Join(canonicalTestPath(filepath.Join(f.worktreesRoot, "alpha")), f.rootID+"-"+f.drainID)
}

// firstItemRoot returns the item root the drain materialized for its first
// member.
func (f sharedDrainDispatchFixture) firstItemRoot(t *testing.T, stdout, stderr string) beads.Bead {
	t.Helper()
	raw := beadByID(t, f.store, f.drainID).Metadata[beadmeta.DrainManifestMetadataKey]
	var manifest struct {
		Rows []struct {
			MemberID   string `json:"member_id"`
			ItemRootID string `json:"item_root_id"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatalf("parse drain manifest %q: %v", raw, err)
	}
	if len(manifest.Rows) == 0 || manifest.Rows[0].ItemRootID == "" {
		t.Fatalf("manifest = %s, want the first member's item materialized; drain metadata=%v stdout=%q stderr=%q", raw, beadByID(t, f.store, f.drainID).Metadata, stdout, stderr)
	}
	return beadByID(t, f.store, manifest.Rows[0].ItemRootID)
}

// TestControlDispatchSharedDrainProvisionsRigWorktree drives the real
// dispatcher entry point over a rig-scoped shared exclusive drain and pins that
// the reserved member and its expanded item work in the drain's managed rig
// worktree.
func TestControlDispatchSharedDrainProvisionsRigWorktree(t *testing.T) {
	f := newSharedDrainDispatchFixture(t, drainItemWorkFormula, true)
	stdout, stderr := f.dispatch(t, f.drainID)

	want := f.workspace()
	member := beadByID(t, f.store, f.memberIDs[0])
	if got := member.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != want {
		t.Fatalf("member %s work_dir = %q, want the drain worktree %q; stderr=%q", member.ID, got, want, stderr)
	}
	if got := member.Metadata[beadmeta.DrainWorkspaceControlMetadataKey]; got != f.drainID {
		t.Fatalf("member %s %s = %q, want the drain %s", member.ID, beadmeta.DrainWorkspaceControlMetadataKey, got, f.drainID)
	}
	if got := testutil.RunGit(t, want, "rev-parse", "--abbrev-ref", "HEAD"); got != "gc/drain/"+f.drainID {
		t.Fatalf("drain worktree branch = %q, want gc/drain/%s", got, f.drainID)
	}
	itemRoot := f.firstItemRoot(t, stdout, stderr)
	for _, key := range []string{beadmeta.LegacyWorkDirMetadataKey, beadmeta.WorkDirMetadataKey} {
		if got := itemRoot.Metadata[key]; got != want {
			t.Fatalf("item root %s %s = %q, want %q", itemRoot.ID, key, got, want)
		}
	}
	all, err := f.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list beads: %v", err)
	}
	steps := 0
	for _, step := range all {
		if step.ID == itemRoot.ID || step.Metadata[beadmeta.RootBeadIDMetadataKey] != itemRoot.ID {
			continue
		}
		steps++
		if got := resolveDrainSourceWorkDir(f.cityPath, f.store, step); got != want {
			t.Fatalf("resolveDrainSourceWorkDir(step %s) = %q, want %q", step.ID, got, want)
		}
	}
	if steps == 0 {
		t.Fatalf("item root %s has no materialized steps", itemRoot.ID)
	}
}

// TestControlDispatchSharedDrainItemCheckRunsInRigWorktree drives a shared
// drain item whose step has a relative exec check through the real
// dispatcher. The check script lives only in the rig checkout (like the
// gitignored .gc/scripts), and the item's worker plants a different one at
// the same path in the drain worktree. The check must run in the worktree with
// the rig's script, never the planted one, both when the worktrees root lies
// outside the city and the rig and when it is the default under the city.
func TestControlDispatchSharedDrainItemCheckRunsInRigWorktree(t *testing.T) {
	for _, tc := range []struct {
		name         string
		externalRoot bool
	}{
		{name: "worktrees root outside the city and the rig", externalRoot: true},
		{name: "default worktrees root under the city"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSharedDrainDispatchFixture(t, drainItemWorkFormula+`
[steps.check]
max_attempts = 1

[steps.check.check]
mode = "exec"
path = ".gc/scripts/checks/verify.sh"
timeout = "30s"
`, tc.externalRoot)
			checkRel := filepath.Join(".gc", "scripts", "checks", "verify.sh")
			marker := filepath.Join(t.TempDir(), "check-cwd")
			script := filepath.Join(f.repo, checkRel)
			if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
				t.Fatalf("mkdir check scripts: %v", err)
			}
			if err := os.WriteFile(script, []byte("#!/bin/sh\npwd -P > '"+marker+"'\n"), 0o755); err != nil {
				t.Fatalf("write check script: %v", err)
			}
			stdout, stderr := f.dispatch(t, f.drainID)
			itemRoot := f.firstItemRoot(t, stdout, stderr)

			planted := filepath.Join(t.TempDir(), "planted-ran")
			plantedScript := filepath.Join(f.workspace(), checkRel)
			if err := os.MkdirAll(filepath.Dir(plantedScript), 0o755); err != nil {
				t.Fatalf("mkdir planted check scripts: %v", err)
			}
			if err := os.WriteFile(plantedScript, []byte("#!/bin/sh\ntouch '"+planted+"'\nexit 0\n"), 0o755); err != nil {
				t.Fatalf("write planted check script: %v", err)
			}
			all, err := f.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			if err != nil {
				t.Fatalf("list beads: %v", err)
			}
			var attempt, check beads.Bead
			for _, bead := range all {
				if bead.Metadata[beadmeta.RootBeadIDMetadataKey] != itemRoot.ID {
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
				t.Fatalf("item %s attempt=%q check=%q, want both materialized", itemRoot.ID, attempt.ID, check.ID)
			}
			if err := f.store.SetMetadata(attempt.ID, beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass); err != nil {
				t.Fatalf("record attempt outcome: %v", err)
			}
			if err := f.store.Close(attempt.ID); err != nil {
				t.Fatalf("close attempt: %v", err)
			}

			_, stderr = f.dispatch(t, check.ID)
			check = beadByID(t, f.store, check.ID)
			if check.Status != "closed" || check.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
				t.Fatalf("check status=%q outcome=%q, want closed pass; metadata=%v stderr=%q", check.Status, check.Metadata[beadmeta.OutcomeMetadataKey], check.Metadata, stderr)
			}
			if _, err := os.Stat(planted); !os.IsNotExist(err) {
				t.Fatalf("stat planted marker: %v, want the script planted in the drain worktree never to run", err)
			}
			ran, err := os.ReadFile(marker)
			if err != nil {
				t.Fatalf("read check marker: %v; stderr=%q", err, stderr)
			}
			if got, want := strings.TrimSpace(string(ran)), f.workspace(); got != want {
				t.Fatalf("check ran in %q, want the drain worktree %q", got, want)
			}
		})
	}
}
