package beads

import (
	"context"
	"testing"
)

// countingStore is a store with an optional capability (Counter) that no
// typed handle promotes.
type countingStore struct {
	*MemStore
}

func (countingStore) Count(context.Context, ListQuery, ...string) (int, error) { return 7, nil }

// unhashableStore has a non-comparable dynamic type: == on two of them
// panics.
type unhashableStore struct {
	*MemStore
	tags []string
}

func TestAsSeesThroughEveryClassHandle(t *testing.T) {
	raw := countingStore{NewMemStore()}
	for name, h := range map[string]Store{
		"work":     WorkStore{raw},
		"graph":    GraphStore{raw},
		"sessions": SessionStore{raw},
		"mail":     MailStore{raw},
		"orders":   OrdersStore{raw},
		"nudges":   NudgesStore{raw},
		"nested":   GraphStore{WorkStore{raw}},
	} {
		if _, ok := h.(Counter); ok {
			t.Fatalf("%s: the handle itself is a Counter; the test no longer shows a hidden capability", name)
		}
		c, ok := As[Counter](h)
		if !ok {
			t.Fatalf("%s: As[Counter] through the handle = false", name)
		}
		if n, _ := c.Count(context.Background(), ListQuery{}); n != 7 {
			t.Fatalf("%s: As[Counter] reached a different store (count %d)", name, n)
		}
	}
}

func TestAsMissingCapability(t *testing.T) {
	if _, ok := As[Counter](GraphStore{NewMemStore()}); ok {
		t.Fatal("As[Counter] found a capability the store lacks")
	}
}

func TestAsIsNilSafe(t *testing.T) {
	if _, ok := As[Counter](nil); ok {
		t.Fatal("As on nil = true")
	}
	if _, ok := As[Counter](GraphStore{}); ok {
		t.Fatal("As on a zero handle = true")
	}
	if _, ok := As[ConditionalWriter](WorkStore{}); ok {
		t.Fatal("As on a zero handle for a provider-backed capability = true")
	}
	if s, ok := As[Store](WorkStore{}); ok {
		t.Fatalf("As[Store] on a zero handle = %v, true; an absent handle is no store", s)
	}
	if _, ok := As[Store](GraphStore{WorkStore{}}); ok {
		t.Fatal("As[Store] on a handle over a zero handle = true")
	}
}

// TestAsMatchesTheOutermostLayer: a capability the handle itself has is
// answered by the handle, so As[Store] keeps the class handle.
func TestAsMatchesTheOutermostLayer(t *testing.T) {
	h := GraphStore{NewMemStore()}
	s, ok := As[Store](h)
	if !ok {
		t.Fatal("As[Store] = false")
	}
	if _, isGraph := s.(GraphStore); !isGraph {
		t.Fatalf("As[Store] = %T, want the GraphStore handle", s)
	}
}

// TestAsNeverPeelsTheCache: As walks class layers only. The cache's own
// capabilities are what a caller wants; its backing is not reachable.
func TestAsNeverPeelsTheCache(t *testing.T) {
	mem := NewMemStore()
	cache := NewCachingStoreForTest(mem, nil)
	h := GraphStore{cache}
	got, ok := As[*CachingStore](h)
	if !ok || got != cache {
		t.Fatalf("As[*CachingStore] = %v, %v; want the cache", got, ok)
	}
	if _, ok := As[*MemStore](h); ok {
		t.Fatal("As[*MemStore] peeled the cache")
	}
}

// TestAsHonorsProviders: for a capability with an XFor helper, As through a
// handle answers exactly what the helper answers on the handle's store.
func TestAsHonorsProviders(t *testing.T) {
	mem := NewMemStore()
	cache := NewCachingStoreForTest(mem, nil)
	for name, raw := range map[string]Store{"mem": mem, "cache": cache} {
		h := SessionStore{raw}
		check := func(capability string, viaAs, viaFor bool) {
			t.Helper()
			if viaAs != viaFor {
				t.Errorf("%s %s: As = %v, XFor on the raw store = %v", name, capability, viaAs, viaFor)
			}
		}
		_, a := As[ConditionalWriter](h)
		_, f := ConditionalWriterFor(raw)
		check("ConditionalWriter", a, f)
		_, a = As[GuardedBatcher](h)
		_, f = GuardedBatcherFor(raw)
		check("GuardedBatcher", a, f)
		_, a = As[GraphApplyStore](h)
		_, f = GraphApplyFor(raw)
		check("GraphApplyStore", a, f)
		_, a = As[MetadataCASWriter](h)
		_, f = MetadataCASWriterFor(raw)
		check("MetadataCASWriter", a, f)
		_, a = As[AtomicConditionalCloser](h)
		_, f = AtomicConditionalCloserFor(raw)
		check("AtomicConditionalCloser", a, f)
		_, a = As[NamespaceCensus](h)
		_, f = NamespaceCensusFor(raw)
		check("NamespaceCensus", a, f)
		_, a = As[DependencyBatchLister](h)
		_, f = DepListBatchFor(raw)
		check("DependencyBatchLister", a, f)
		_, a = As[GraphOnlyReadyStore](h)
		_, f = GraphOnlyReadyFor(raw)
		check("GraphOnlyReadyStore", a, f)
		_, a = As[AssignmentGuardedUpdater](h)
		_, f = AssignmentGuardedUpdaterFor(raw)
		check("AssignmentGuardedUpdater", a, f)
	}
	if _, ok := As[ConditionalWriter](GraphStore{mem}); !ok {
		t.Fatal("the fixture's MemStore is no ConditionalWriter: the test proves nothing")
	}
}

func TestRawReturnsTheWrappedStore(t *testing.T) {
	mem := NewMemStore()
	for name, got := range map[string]Store{
		"work": WorkStore{mem}.Raw(), "graph": GraphStore{mem}.Raw(), "sessions": SessionStore{mem}.Raw(),
		"mail": MailStore{mem}.Raw(), "orders": OrdersStore{mem}.Raw(), "nudges": NudgesStore{mem}.Raw(),
	} {
		if got != Store(mem) {
			t.Errorf("%s: Raw() = %v, want the wrapped store", name, got)
		}
	}
	if (GraphStore{}).Raw() != nil {
		t.Error("Raw() of a zero handle is not nil")
	}
}

func TestSameStore(t *testing.T) {
	a, b := NewMemStore(), NewMemStore()
	u := unhashableStore{MemStore: a, tags: []string{"x"}}
	for name, tc := range map[string]struct {
		x, y Store
		want bool
	}{
		"one store, two classes":       {GraphStore{a}, WorkStore{a}, true},
		"a handle and its store":       {a, SessionStore{a}, true},
		"nested handles":               {GraphStore{WorkStore{a}}, MailStore{a}, true},
		"two stores":                   {GraphStore{a}, GraphStore{b}, false},
		"zero handles":                 {GraphStore{}, WorkStore{}, false},
		"nil":                          {nil, a, false},
		"a non-comparable store":       {u, u, false},
		"a non-comparable under class": {GraphStore{u}, WorkStore{u}, false},
	} {
		if got := SameStore(tc.x, tc.y); got != tc.want {
			t.Errorf("%s: SameStore = %v, want %v", name, got, tc.want)
		}
	}
}

// testGate is a class-guard layer as T8 adds one: a class layer that vets
// class-write capabilities. It adapts ForeignIDCreator and hides the rest.
type testGate struct {
	Store
	adapted ForeignIDCreator
}

func (g testGate) unwrapClassLayer() Store { return g.Store }

func (g testGate) gateCapability(target any) bool {
	if p, ok := target.(*ForeignIDCreator); ok && g.adapted != nil {
		*p = g.adapted
		return true
	}
	return false
}

type fakeForeignIDCreator struct{ name string }

func (fakeForeignIDCreator) CreateWithForeignID(Bead) (Bead, error) { return Bead{}, nil }

// rawForeignStore has every class-write capability the gate must vet, so a
// capability that reaches the caller unvetted is visible.
type rawForeignStore struct {
	*MemStore
}

func (rawForeignStore) CreateWithForeignID(Bead) (Bead, error) { return Bead{}, nil }

func TestAsThroughAClassGate(t *testing.T) {
	inner := rawForeignStore{NewMemStore()}
	if _, ok := Store(inner).(ForeignIDCreator); !ok {
		t.Fatal("fixture: the inner store is no ForeignIDCreator")
	}
	adapter := fakeForeignIDCreator{name: "guarded"}
	h := GraphStore{testGate{Store: inner, adapted: adapter}}

	got, ok := As[ForeignIDCreator](h)
	if !ok || got != ForeignIDCreator(adapter) {
		t.Fatalf("As[ForeignIDCreator] through a gate = %v, %v; want the gate's adapter", got, ok)
	}
	// A class-write capability the gate does not adapt is hidden, never the
	// unguarded inner one.
	if _, ok := As[ConditionalWriter](h); ok {
		t.Fatal("As[ConditionalWriter] leaked the unguarded writer through a gate")
	}
	declining := GraphStore{testGate{Store: inner}}
	if _, ok := As[ForeignIDCreator](declining); ok {
		t.Fatal("As[ForeignIDCreator] leaked the unguarded creator through a declining gate")
	}
	// A capability that cannot change a class passes through.
	counted := GraphStore{testGate{Store: countingStore{NewMemStore()}}}
	if _, ok := As[Counter](counted); !ok {
		t.Fatal("As[Counter] did not pass through the gate")
	}
	// The gate is a class layer: identity sees through it.
	if !SameStore(h, WorkStore{inner}) {
		t.Fatal("SameStore did not see through the gate")
	}
}

func TestClassWriteCapabilities(t *testing.T) {
	for name, got := range map[string]bool{
		"StorageCreateStore":       isClassWriteCapability[StorageCreateStore](),
		"ForeignIDCreator":         isClassWriteCapability[ForeignIDCreator](),
		"StorageGraphApplyStore":   isClassWriteCapability[StorageGraphApplyStore](),
		"EphemeralGraphApplyStore": isClassWriteCapability[EphemeralGraphApplyStore](),
		"AssignmentGuardedUpdater": isClassWriteCapability[AssignmentGuardedUpdater](),
		"ConditionalWriter":        isClassWriteCapability[ConditionalWriter](),
		"GuardedBatcher":           isClassWriteCapability[GuardedBatcher](),
		"GraphApplyStore":          isClassWriteCapability[GraphApplyStore](),
		"MetadataCASWriter":        isClassWriteCapability[MetadataCASWriter](),
		"AtomicConditionalCloser":  isClassWriteCapability[AtomicConditionalCloser](),
	} {
		if !got {
			t.Errorf("%s is not a class-write capability", name)
		}
	}
	for name, got := range map[string]bool{
		"Counter":                       isClassWriteCapability[Counter](),
		"ConditionalAssignmentReleaser": isClassWriteCapability[ConditionalAssignmentReleaser](),
		"ConditionalAssigneeTransferer": isClassWriteCapability[ConditionalAssigneeTransferer](),
		"DependencyBatchLister":         isClassWriteCapability[DependencyBatchLister](),
		"Store":                         isClassWriteCapability[Store](),
	} {
		if got {
			t.Errorf("%s is a class-write capability; it cannot change a class", name)
		}
	}
}

// TestXForThroughAHandleToday pins today's behavior, which T1 does not
// change: the XFor helpers that do not follow a declared resolution target
// answer "no" on a typed handle even when its store has the capability, and
// HandlesFor falls back to the logical wrappers. Provider forwarding on the
// handles (design §3.3.1-2) changes these answers, so it lands with its own
// behavior tests; until then As is the way through a handle.
func TestXForThroughAHandleToday(t *testing.T) {
	mem := NewMemStore()
	h := GraphStore{mem}
	if _, ok := ConditionalWriterFor(mem); !ok {
		t.Fatal("fixture: MemStore is no ConditionalWriter")
	}
	if _, ok := ConditionalWriterFor(h); ok {
		t.Error("ConditionalWriterFor now sees through a handle: provider forwarding landed; update this pin and its callers' tests")
	}
	if _, ok := As[ConditionalWriter](h); !ok {
		t.Error("As[ConditionalWriter] does not see through a handle")
	}
	if _, ok := GuardedBatcherFor(h); !ok {
		t.Error("GuardedBatcherFor no longer forwards through a handle")
	}
	if _, ok := MetadataCASWriterFor(h); !ok {
		t.Error("MetadataCASWriterFor no longer follows the handle's resolution target")
	}
	cache := NewCachingStoreForTest(mem, nil)
	if _, logical := HandlesFor(GraphStore{cache}).Cached.(logicalCachedStoreReader); !logical {
		t.Error("HandlesFor now sees through a handle to the cache: provider forwarding landed; update this pin")
	}
}
