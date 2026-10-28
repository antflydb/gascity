package beadstest

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestClassHandleCapabilityConformanceMemStore(t *testing.T) {
	RunClassHandleCapabilityConformance(t, func(*testing.T) beads.Store { return beads.NewMemStore() })
}

func TestClassHandleCapabilityConformanceCachingStore(t *testing.T) {
	RunClassHandleCapabilityConformance(t, func(*testing.T) beads.Store {
		return beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	})
}

func TestClassHandleCapabilityConformanceFileStore(t *testing.T) {
	RunClassHandleCapabilityConformance(t, func(t *testing.T) beads.Store {
		s, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestSingleStoreMintsEveryClassOverOneStore(t *testing.T) {
	mem := beads.NewMemStore()
	c := SingleStore(mem)
	all := c.All()
	if len(all) != 6 {
		t.Fatalf("SingleStore has %d classes, want 6", len(all))
	}
	for class, h := range all {
		if !beads.SameStore(h, mem) {
			t.Errorf("%s: not over the given store", class)
		}
	}
	if c.Graph.Raw() != beads.Store(mem) || c.Nudges.Raw() != beads.Store(mem) {
		t.Fatal("a handle wraps a different store")
	}
}

// TestConformanceCatchesAHiddenCapability: the conformance fails on a handle
// that hides a capability its store has.
func TestConformanceCatchesAHiddenCapability(t *testing.T) {
	mem := beads.NewMemStore()
	want, ok := beads.As[beads.ConditionalWriter](mem)
	if !ok {
		t.Fatal("fixture: MemStore is no ConditionalWriter")
	}
	// A wrapper that is not a class layer stops As: the capability is hidden.
	hidden := struct{ beads.Store }{mem}
	got, gotOK := capabilityProbes[0].look(hidden)
	if capabilityProbes[0].name != "ConditionalWriter" || gotOK || got == want {
		t.Fatalf("probe through an opaque wrapper = %v, %v; the conformance would not catch a hidden capability", got, gotOK)
	}
}
