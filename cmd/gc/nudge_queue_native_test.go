package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/storebinding"
)

type nativeQueueSnapshot struct {
	mu      sync.Mutex
	data    []byte
	version uint64
}

func (s *nativeQueueSnapshot) Load(context.Context) ([]byte, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data...), s.version, nil
}

func (s *nativeQueueSnapshot) CompareAndSwap(_ context.Context, v uint64, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v != s.version {
		return errors.New("unexpected snapshot conflict")
	}
	s.data = append([]byte(nil), data...)
	s.version++
	return nil
}
func (*nativeQueueSnapshot) Close() error { return nil }
func nativeQueueStore(t *testing.T) *beads.AntflyStore {
	t.Helper()
	s, e := beads.NewAntflyStore(&nativeQueueSnapshot{}, "gcg")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.CloseStore() })
	return s
}

func TestAntflyNativeNudgeQueueRoutesDeliveryAndShadows(t *testing.T) {
	city := t.TempDir()
	s := nativeQueueStore(t)
	cached := beads.NewCachingStoreForTest(s, nil)
	t.Cleanup(cached.StopReconciler)
	store := beads.NudgesStore{Store: cached}
	h := openNudgeQueue(city, store)
	now := time.Now().UTC()
	item := nudgequeue.Item{ID: "native-one", Agent: "worker", Message: "native delivery", Source: "test", CreatedAt: now, DeliverAfter: now, ExpiresAt: now.Add(time.Hour)}
	if e := h.Queue.Enqueue(item); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(city, ".gc", "nudges", "state.json")); !os.IsNotExist(e) {
		t.Fatalf("delivery state written to file: %v", e)
	}
	target := storebinding.ClaimTarget{QueueKeys: []string{"worker"}}
	if due, e := h.Probe.WouldClaimBatch(target, now); e != nil || !due {
		t.Fatalf("probe: %v %v", due, e)
	}
	claimed, e := h.Queue.ClaimBatch(target, now)
	if e != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, e)
	}
	if e = h.Repin.RepinClaims([]string{item.ID}, "replacement"); e != nil {
		t.Fatal(e)
	}
	if e = h.Queue.Ack([]string{item.ID}, "", "delivered", "test"); e != nil {
		t.Fatal(e)
	}
	if _, found, e := nudgeShadows(store).Find(item.ID); e != nil || found {
		t.Fatalf("terminal shadow remained open: %v %v", found, e)
	}
	if shadow, found, e := nudgeShadows(store).FindIncludingTerminal(item.ID); e != nil || !found || shadow.Open {
		t.Fatalf("terminal state lost: %+v %v %v", shadow, found, e)
	}
}

func TestAntflyNativeNudgeQueueMovesLegacyPendingBatch(t *testing.T) {
	city := t.TempDir()
	s := nativeQueueStore(t)
	store := beads.NudgesStore{Store: s}
	now := time.Now().UTC()
	legacy := &fileNudgeQueue{cityPath: city, store: store, clk: clock.Real{}}
	item := nudgequeue.Item{ID: "legacy-one", Agent: "worker", Message: "retain", Source: "test", CreatedAt: now, DeliverAfter: now, ExpiresAt: now.Add(time.Hour)}
	if e := legacy.Enqueue(item); e != nil {
		t.Fatal(e)
	}
	h := openNudgeQueue(city, store)
	pending, _, _, e := h.Queue.ListForAgent("worker", now)
	if e != nil || len(pending) != 1 || pending[0].ID != item.ID {
		t.Fatalf("migration lost item: %+v %v", pending, e)
	}
	if _, found, e := nudgeShadows(store).Find(item.ID); e != nil || !found {
		t.Fatalf("native view lost migrated item: %v %v", found, e)
	}
	if e = h.Queue.Ack([]string{item.ID}, "", "delivered", "migrated"); e != nil {
		t.Fatal(e)
	}
	if _, found, e := nudgeShadows(store).Find(item.ID); e != nil || found {
		t.Fatalf("legacy shadow resurrected terminal nudge: %v %v", found, e)
	}
}

func TestAntflyNativeNudgeProbeKeepsPinnedBatchesWhole(t *testing.T) {
	s := nativeQueueStore(t)
	h := openNudgeQueue(t.TempDir(), beads.NudgesStore{Store: s})
	now := time.Now().UTC()
	target := storebinding.ClaimTarget{QueueKeys: []string{"worker"}}
	for _, id := range []string{"a", "b"} {
		if e := h.Queue.EnqueueDeferred(nudgequeue.Item{ID: id, Agent: "worker", Source: "test", Message: id, CreatedAt: now, DeliverAfter: now, ExpiresAt: now.Add(2 * time.Hour)}); e != nil {
			t.Fatal(e)
		}
	}
	first, e := h.Queue.ClaimBatch(target, now)
	if e != nil || len(first) != 2 {
		t.Fatalf("initial batch: %+v %v", first, e)
	}
	key := first[0].DeliveryBatch
	if e = h.Queue.ReleaseClaims([]string{"b"}, now.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if e = h.Queue.ReleaseClaims([]string{"a"}, time.Time{}); e != nil {
		t.Fatal(e)
	}
	if due, e := h.Probe.WouldClaimBatch(target, now); e != nil || due {
		t.Fatalf("partial pinned batch looked claimable: %v %v", due, e)
	}
	at := now.Add(time.Hour + time.Second)
	if due, e := h.Probe.WouldClaimBatch(target, at); e != nil || !due {
		t.Fatalf("whole batch not claimable: %v %v", due, e)
	}
	next, e := h.Queue.ClaimBatch(target, at)
	if e != nil || len(next) != 2 || next[0].DeliveryBatch != key {
		t.Fatalf("batch changed: %+v %v", next, e)
	}
	if e = h.Repin.RepinClaims([]string{"a", "absent"}, "must-not-publish"); e == nil {
		t.Fatal("repinned missing member")
	}
	_, inflight, _, e := h.Queue.ListForAgent("worker", at)
	if e != nil || len(inflight) != 2 {
		t.Fatalf("lost claims: %+v %v", inflight, e)
	}
	for _, item := range inflight {
		if item.DeliveryBatch != key {
			t.Fatal("partial repin publication")
		}
	}
}
