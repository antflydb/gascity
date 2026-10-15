package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// listCountingStore counts the List calls that reach it. Behind a cold
// CachingStore it stands in for a suspended rig's bd-backed store, where each
// List is a bd list plus a bd query subprocess.
type listCountingStore struct {
	*beads.MemStore
	lists atomic.Int32
}

func (s *listCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.lists.Add(1)
	return s.MemStore.List(query)
}

// addSuspendedColdRig adds a rig suspended at city start, holding the store the
// controller gives such a rig: a CachingStore that is never primed
// (wrapWithCachingStore with backgroundRefresh=false), so every read of it
// reaches the backing store.
func addSuspendedColdRig(t *testing.T, fs *fakeState, name string) *listCountingStore {
	t.Helper()
	fs.cfg.Rigs = append(fs.cfg.Rigs, config.Rig{Name: name, Path: "/tmp/" + name, SuspendedOnStart: true})
	backing := &listCountingStore{MemStore: beads.NewMemStore()}
	fs.stores[name] = beads.NewCachingStoreForTest(backing, nil)
	return backing
}

func TestHandleSessionListDoesNotReadSuspendedRigStores(t *testing.T) {
	fs := newSessionFakeState(t)
	cold := addSuspendedColdRig(t, fs, "coldrig")
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	const active = 3
	for i := 0; i < active; i++ {
		createTestSession(t, fs.cityBeadStore, fs.sp, fmt.Sprintf("session %d", i))
	}

	req := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp struct {
		Items []sessionResponse `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != active {
		t.Fatalf("items = %d, want %d", len(resp.Items), active)
	}
	for _, item := range resp.Items {
		if !item.Running {
			t.Fatalf("session %s running = false: the list never reached its active-bead lookup", item.ID)
		}
	}
	if got := cold.lists.Load(); got != 0 {
		t.Fatalf("suspended rig store List calls = %d for %d active sessions, want 0: each one is a bd list + bd query pair in production", got, active)
	}
}

func TestHandleSessionGetDoesNotReadSuspendedRigStores(t *testing.T) {
	fs := newSessionFakeState(t)
	cold := addSuspendedColdRig(t, fs, "coldrig")
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")

	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Running {
		t.Fatal("running = false: the get never reached its active-bead lookup")
	}
	if got := cold.lists.Load(); got != 0 {
		t.Fatalf("suspended rig store List calls = %d, want 0", got)
	}
}

func TestHandleSessionListFindsActiveBeadBesideASuspendedRig(t *testing.T) {
	fs := newSessionFakeState(t)
	cold := addSuspendedColdRig(t, fs, "coldrig")
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")
	work, err := fs.stores["myrig"].Create(beads.Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	assignee := info.ID
	if err := fs.stores["myrig"].Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}

	req := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp struct {
		Items []sessionResponse `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.Items))
	}
	if got := resp.Items[0].ActiveBead; got != work.ID {
		t.Fatalf("active_bead = %q, want %q from the running rig", got, work.ID)
	}
	if got := cold.lists.Load(); got != 0 {
		t.Fatalf("suspended rig store List calls = %d, want 0", got)
	}
}
