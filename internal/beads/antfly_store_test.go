package beads_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	antfly "github.com/steveyegge/beads/backend/antfly"
)

type antflySnapshotService struct {
	mu      sync.Mutex
	data    []byte
	version uint64
	reject  bool
	lost    bool
}

func (s *antflySnapshotService) Load(context.Context) ([]byte, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data...), s.version, nil
}

func (s *antflySnapshotService) CompareAndSwap(_ context.Context, v uint64, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reject || v != s.version {
		s.reject = false
		return antfly.ErrSnapshotConflict
	}
	s.data = append([]byte(nil), data...)
	s.version++
	if s.lost {
		s.lost = false
		return errors.New("lost commit acknowledgement")
	}
	return nil
}
func (*antflySnapshotService) Close() error { return nil }
func freshAntflyStore(t *testing.T) *beads.AntflyStore {
	t.Helper()
	s, e := beads.NewAntflyStore(&antflySnapshotService{}, "af")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.CloseStore() })
	return s
}

func TestAntflyStoreConformance(t *testing.T) {
	beadstest.RunStoreTests(t, func() beads.Store { return freshAntflyStore(t) })
	beadstest.RunFenceConformance(t, func() beads.Store { return freshAntflyStore(t) })
	beadstest.RunConditionalWriterConformanceWithOptions(t, "antfly", func(t *testing.T) beads.Store { return freshAntflyStore(t) }, beadstest.ConditionalWriterOptions{RowBackedMutationFlavors: true, SuppliesCurrent: true})
	beadstest.RunGuardedBatchConformance(t, "antfly", func(t *testing.T) beads.Store { return freshAntflyStore(t) })
	beadstest.RunMetadataCASConformance(t, "antfly", func(t *testing.T) beads.Store { return freshAntflyStore(t) })
	beadstest.RunAtomicConditionalCloserConformance(t, "antfly", func(t *testing.T) beads.Store { return freshAntflyStore(t) })
	beadstest.RunPinnedIDDuplicateConformance(t, func(t *testing.T) beads.Store { return freshAntflyStore(t) })
	beadstest.RunPinnedIDFenceConformance(t, func(t *testing.T, prefix string, namespaces ...string) beads.Store {
		s, e := beads.NewAntflyStore(&antflySnapshotService{}, prefix, namespaces...)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = s.CloseStore() })
		return s
	})
}

func TestAntflyTransactionCallbackRunsOnceAndRollsBack(t *testing.T) {
	service := &antflySnapshotService{}
	s, e := beads.NewAntflyStore(service, "gcg")
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	service.reject = true
	e = s.Tx("callback", func(tx beads.Tx) error { calls++; _, err := tx.Create(beads.Bead{Title: "once"}); return err })
	if !errors.Is(e, antfly.ErrSnapshotConflict) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, e)
	}
	rows, e := s.List(beads.ListQuery{AllowScan: true})
	if e != nil || len(rows) != 0 {
		t.Fatalf("partial callback publication: %+v %v", rows, e)
	}
	e = s.Tx("rollback", func(tx beads.Tx) error {
		_, err := tx.Create(beads.Bead{Title: "discard"})
		if err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if e == nil {
		t.Fatal("accepted failed callback")
	}
	rows, e = s.List(beads.ListQuery{AllowScan: true})
	if e != nil || len(rows) != 0 {
		t.Fatal("rollback leaked")
	}
}

func TestAntflyRevisionAndFenceSurviveReopenAndRecreate(t *testing.T) {
	service := &antflySnapshotService{}
	s, e := beads.NewAntflyStore(service, "gcg")
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.Create(beads.Bead{ID: "gcg-pinned", Title: "old"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SetMetadata(b.ID, "generation", "old"); e != nil {
		t.Fatal(e)
	}
	b, _ = s.Get(b.ID)
	if e = s.Delete(b.ID); e != nil {
		t.Fatal(e)
	}
	other, e := beads.NewAntflyStore(service, "gcg")
	if e != nil {
		t.Fatal(e)
	}
	next, e := other.Create(beads.Bead{ID: b.ID, Title: "new"})
	if e != nil {
		t.Fatal(e)
	}
	if next.Revision == b.Revision {
		t.Fatal("reused revision after reopen/recreate")
	}
	if e = other.UpdateIfMatch(next.ID, b.Revision, beads.UpdateOpts{Metadata: map[string]string{"stale": "yes"}}); !beads.IsPreconditionFailed(e) {
		t.Fatalf("stale token accepted: %v", e)
	}
	raw, _, _ := service.Load(context.Background())
	if !json.Valid(raw) {
		t.Fatal("invalid durable JSON")
	}
}

func TestNativeAntflyInfrastructurePersistence(t *testing.T) {
	lib := os.Getenv("ANTFLY_TEST_LIBRARY")
	if lib == "" {
		t.Skip("ANTFLY_TEST_LIBRARY is required for real native persistence")
	}
	target := antfly.Target{Path: filepath.Join(t.TempDir(), "infra.aflite"), Library: lib, Table: "gascity_infra", Workspace: "infra"}
	s, e := beads.OpenAntflyStore(target, "gcg")
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.Create(beads.Bead{Type: "session", Title: "Native -> session & metadata"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SetMetadata(b.ID, "session_state", "active"); e != nil {
		t.Fatal(e)
	}
	before, _ := s.Get(b.ID)
	if e = s.CloseStore(); e != nil {
		t.Fatal(e)
	}
	s, e = beads.OpenAntflyStore(target, "gcg")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = s.CloseStore() }()
	after, e := s.Get(b.ID)
	if e != nil || after.Metadata["session_state"] != "active" || after.Revision != before.Revision {
		t.Fatalf("lost durable state: %+v %v", after, e)
	}
}

func TestAntflyUnknownCommitOutcomeIsNotReplayed(t *testing.T) {
	service := &antflySnapshotService{}
	s, e := beads.NewAntflyStore(service, "gcg")
	if e != nil {
		t.Fatal(e)
	}
	service.lost = true
	if _, e = s.Create(beads.Bead{Title: "published once"}); e == nil {
		t.Fatal("lost acknowledgement was hidden")
	}
	rows, e := s.List(beads.ListQuery{AllowScan: true})
	if e != nil || len(rows) != 1 {
		t.Fatalf("commit replayed or lost: %+v %v", rows, e)
	}
}

func TestAntflyIndependentHandlesDoNotLoseWrites(t *testing.T) {
	service := &antflySnapshotService{}
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, e := beads.NewAntflyStore(service, "gcg")
			if e != nil {
				t.Error(e)
				return
			}
			for i := 0; i < 10; i++ {
				if _, e = s.Create(beads.Bead{Title: "concurrent"}); e != nil {
					t.Error(e)
					return
				}
			}
		}()
	}
	wg.Wait()
	s, e := beads.NewAntflyStore(service, "gcg")
	if e != nil {
		t.Fatal(e)
	}
	rows, e := s.List(beads.ListQuery{AllowScan: true})
	if e != nil || len(rows) != 40 {
		t.Fatalf("lost writes: count=%d %v", len(rows), e)
	}
}

func TestAntflyGraphApplyIsAtomic(t *testing.T) {
	s := freshAntflyStore(t)
	plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{{Key: "root", Title: "Formula", Type: "molecule"}, {Key: "step", Title: "Step", ParentKey: "root", MetadataRefs: map[string]string{"root": "root"}}}, Edges: []beads.GraphApplyEdge{{FromKey: "step", ToKey: "root", Type: "parent-child"}}}
	result, e := s.ApplyGraphPlanWithStorage(context.Background(), plan, beads.StorageEphemeral)
	if e != nil {
		t.Fatal(e)
	}
	step, e := s.Get(result.IDs["step"])
	if e != nil || step.ParentID != result.IDs["root"] || step.Metadata["root"] != result.IDs["root"] || !step.Ephemeral {
		t.Fatalf("bad graph: %+v %v", step, e)
	}
	before, e := s.List(beads.ListQuery{AllowScan: true})
	if e != nil {
		t.Fatal(e)
	}
	cycle := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{{Key: "a", Title: "A"}, {Key: "b", Title: "B"}}, Edges: []beads.GraphApplyEdge{{FromKey: "a", ToKey: "b"}, {FromKey: "b", ToKey: "a"}}}
	if _, e = s.ApplyGraphPlan(context.Background(), cycle); e == nil {
		t.Fatal("accepted cycle")
	}
	after, e := s.List(beads.ListQuery{AllowScan: true})
	if e != nil || len(after) != len(before) {
		t.Fatalf("partial graph: %+v %v", after, e)
	}
}

func TestAntflyDependencyMetadataIsReplacedAndCleared(t *testing.T) {
	s := freshAntflyStore(t)
	a, e := s.Create(beads.Bead{Title: "A"})
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.Create(beads.Bead{Title: "B"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.DepAddWithMetadata(b.ID, a.ID, "", `{"condition":"ready"}`); e != nil {
		t.Fatal(e)
	}
	payload, carried, e := s.DepMetadata(b.ID, a.ID)
	if e != nil || !carried || payload != `{"condition":"ready"}` {
		t.Fatalf("lost payload: %q %v %v", payload, carried, e)
	}
	if e = s.DepAdd(b.ID, a.ID, "blocks"); e != nil {
		t.Fatal(e)
	}
	_, carried, e = s.DepMetadata(b.ID, a.ID)
	if e != nil || carried {
		t.Fatalf("plain dep add retained stale payload: %v %v", carried, e)
	}
	if e = s.DepRemove(b.ID, a.ID); e != nil {
		t.Fatal(e)
	}
	deps, e := s.DepList(b.ID, "down")
	if e != nil || len(deps) != 0 {
		t.Fatalf("stale dependency: %+v %v", deps, e)
	}
}

func TestNativeAntflyConcurrentHandles(t *testing.T) {
	lib := os.Getenv("ANTFLY_TEST_LIBRARY")
	if lib == "" {
		t.Skip("ANTFLY_TEST_LIBRARY required")
	}
	target := antfly.Target{Path: filepath.Join(t.TempDir(), "concurrent.aflite"), Library: lib, Table: "infra", Workspace: "infra"}
	stores := make([]*beads.AntflyStore, 3)
	for i := range stores {
		var e error
		stores[i], e = beads.OpenAntflyStore(target, "af")
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = stores[i].CloseStore() })
	}
	var wg sync.WaitGroup
	for _, s := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 6; n++ {
				b, e := s.Create(beads.Bead{Title: "concurrent native"})
				if e != nil {
					t.Error(e)
					return
				}
				if e = s.SetMetadata(b.ID, "persisted", "yes"); e != nil {
					t.Error(e)
					return
				}
				if _, e = s.Get(b.ID); e != nil {
					t.Error(e)
					return
				}
			}
		}()
	}
	wg.Wait()
	if e := stores[0].CloseStore(); e != nil {
		t.Fatal(e)
	}
	rows, e := stores[1].List(beads.ListQuery{AllowScan: true})
	if e != nil || len(rows) != 18 {
		t.Fatalf("lost native writes: %d %v", len(rows), e)
	}
	if _, e = stores[2].Create(beads.Bead{Title: "after another handle closes"}); e != nil {
		t.Fatal(e)
	}
}

func TestAntflyClaimHasOneWinnerAndPreservesFence(t *testing.T) {
	service := &antflySnapshotService{}
	s, e := beads.NewAntflyStore(service, "af")
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.Create(beads.Bead{Title: "claim"})
	if e != nil {
		t.Fatal(e)
	}
	results := make(chan beads.Bead, 2)
	var wg sync.WaitGroup
	for _, owner := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, err := beads.NewAntflyStore(service, "af")
			if err != nil {
				t.Error(err)
				return
			}
			claimed, ok, err := other.Claim(b.ID, owner)
			if err != nil {
				t.Error(err)
			}
			if ok {
				results <- claimed
			}
		}()
	}
	wg.Wait()
	close(results)
	var winner beads.Bead
	count := 0
	for row := range results {
		winner = row
		count++
	}
	if count != 1 {
		t.Fatalf("claim winners=%d", count)
	}
	same, ok, e := s.Claim(b.ID, winner.Assignee)
	if e != nil || !ok || same.Revision != winner.Revision || same.ClaimFence != winner.ClaimFence {
		t.Fatalf("non-idempotent self claim: %+v %v %v", same, ok, e)
	}
	if ok, e = s.ReleaseIfCurrent(b.ID, winner.Assignee); e != nil || !ok {
		t.Fatal(e)
	}
	next, ok, e := s.Claim(b.ID, "next")
	if e != nil || !ok || next.ClaimFence == winner.ClaimFence {
		t.Fatalf("fence reused: %+v %v", next, e)
	}
}
