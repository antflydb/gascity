package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"

	"github.com/steveyegge/beads/backend/antfly"
)

func TestAntflyWorkStoreNativeRoleAdapter(t *testing.T) {
	library := os.Getenv("ANTFLY_TEST_LIBRARY")
	if library == "" {
		t.Skip("ANTFLY_TEST_LIBRARY required")
	}
	ctx := context.Background()
	root := t.TempDir()
	if err := antfly.Initialize(ctx, filepath.Join(root, ".beads"), antfly.Target{Path: filepath.Join(root, "work.aflite"), Library: library, Table: "work", Workspace: "work"}, "aw"); err != nil {
		t.Fatal(err)
	}
	store, err := OpenAntflyWorkStoreAt(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.CloseStore() }()
	first, err := store.Create(Bead{Title: "first", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(Bead{Title: "second", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.DepAdd(second.ID, first.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].ID != first.ID {
		t.Fatalf("readiness: %+v", ready)
	}
	if err = store.Close(second.ID); err == nil {
		t.Fatal("blocked close accepted")
	}
	if err = store.Close(first.ID); err != nil {
		t.Fatal(err)
	}
	ready, err = store.Ready()
	if err != nil || len(ready) != 1 || ready[0].ID != second.ID {
		t.Fatalf("released readiness: %+v %v", ready, err)
	}
	other, err := OpenAntflyWorkStoreAt(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.CloseStore() }()
	got, err := other.Get(second.ID)
	if err != nil || got.ID != second.ID {
		t.Fatalf("independent handle: %+v %v", got, err)
	}
}

func TestAntflyWorkStoreGraphBatchNativeAndRemote(t *testing.T) {
	for _, mode := range []string{"embedded", "remote"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			target := antfly.Target{Table: "work_graph_" + time.Now().Format("150405000000000"), Workspace: "work"}
			if mode == "embedded" {
				target.Library = os.Getenv("ANTFLY_TEST_LIBRARY")
				if target.Library == "" {
					t.Skip("ANTFLY_TEST_LIBRARY required")
				}
				target.Path = filepath.Join(root, "work.aflite")
			} else {
				target.URL = os.Getenv("ANTFLY_TEST_URL")
				if target.URL == "" {
					t.Skip("ANTFLY_TEST_URL required")
				}
			}
			if err := antfly.Initialize(ctx, filepath.Join(root, ".beads"), target, "aw"); err != nil {
				t.Fatal(err)
			}
			store, err := OpenAntflyWorkStoreAt(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.CloseStore() }()
			result, err := store.ApplyGraphPlanWithStorage(ctx, &GraphApplyPlan{Nodes: []GraphApplyNode{
				{Key: "root", Title: "Legacy root", Type: "molecule", MetadataRefs: map[string]string{"last": "second"}},
				{Key: "first", Title: "First", Type: "step", ParentKey: "root"},
				{Key: "second", Title: "Second", Type: "step", ParentKey: "root", Assignee: "worker", AssignAfterCreate: true},
			}, Edges: []GraphApplyEdge{{FromKey: "second", ToKey: "first", Type: "blocks"}}}, StorageDefault)
			if err != nil {
				t.Fatal(err)
			}
			rootBead, err := store.Get(result.IDs["root"])
			if err != nil || rootBead.Metadata["last"] != result.IDs["second"] {
				t.Fatalf("root: %+v %v", rootBead, err)
			}
			second, err := store.Get(result.IDs["second"])
			if err != nil || second.ParentID != rootBead.ID || second.Assignee != "worker" {
				t.Fatalf("second: %+v %v", second, err)
			}
			if err = store.Close(second.ID); err == nil {
				t.Fatal("batch bypassed close blocker")
			}
			if err = store.Close(result.IDs["first"]); err != nil {
				t.Fatal(err)
			}
			empty := ""
			status := "open"
			if err = store.Update(second.ID, UpdateOpts{Assignee: &empty, Status: &status}); err != nil {
				t.Fatal(err)
			}
			if err = store.Close(second.ID); err != nil {
				t.Fatal(err)
			}
			if err = store.Close(rootBead.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAntflyWorkUpdateRetriesOnlyProvedAbort(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		attempts int
	}{
		{"typed rejection", fmt.Errorf("batch publication: %w", antfly.ErrSnapshotConflict), 2},
		{"lookalike text", errors.New(antfly.ErrSnapshotConflict.Error()), 1},
		{"unknown outcome", errors.New("write commit result indeterminate"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{runInTransaction: func(context.Context, string, func(beadslib.Transaction) error) error {
				attempts++
				if attempts == 1 {
					return tc.err
				}
				return nil
			}})
			err := store.Update("aw-test", UpdateOpts{Metadata: map[string]string{"receipt": "complete"}})
			if attempts != tc.attempts {
				t.Fatalf("attempts=%d want %d", attempts, tc.attempts)
			}
			if tc.attempts == 1 && !errors.Is(err, tc.err) {
				t.Fatalf("lost refusal: %v", err)
			}
			if tc.attempts == 2 && err != nil {
				t.Fatal(err)
			}
		})
	}
}
