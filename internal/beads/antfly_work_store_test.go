package beads

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
