package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// budgetTestStore decorates a store the way a slow, flaky backing store looks
// to the repair writers: every durable write waits for delay (on the test's
// fake clock) and the ids in failIDs always fail after that wait. It counts
// the writes and the Gets it sees so a test can assert both the budget's caps
// and the root-read memo.
type budgetTestStore struct {
	beads.Store
	delay   time.Duration
	failIDs map[string]bool
	failAll bool

	writes  int
	written []string // ids of write attempts, in order
	gets    map[string]int
}

var errBudgetTestWrite = errors.New("budget test: write failed")

func (s *budgetTestStore) write(id string) error {
	s.writes++
	s.written = append(s.written, id)
	if s.delay > 0 {
		<-time.After(s.delay)
	}
	if s.failAll || s.failIDs[id] {
		return errBudgetTestWrite
	}
	return nil
}

func (s *budgetTestStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.write(id); err != nil {
		return err
	}
	return s.Store.Update(id, opts)
}

func (s *budgetTestStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.write(id); err != nil {
		return err
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *budgetTestStore) Get(id string) (beads.Bead, error) {
	if s.gets == nil {
		s.gets = map[string]int{}
	}
	s.gets[id]++
	return s.Store.Get(id)
}

// seedSlotRoutedBacklog creates n open, unassigned rows routed at a live slot
// suffix, each of which collapseSlotSuffixedRoutedWork repairs with exactly one
// Update. The returned slice is index-aligned with the store slice.
func seedSlotRoutedBacklog(t *testing.T, store *budgetTestStore, n int) ([]beads.Bead, []beads.Store) {
	t.Helper()
	rows := make([]beads.Bead, 0, n)
	stores := make([]beads.Store, 0, n)
	for i := range n {
		created, err := store.Create(beads.Bead{
			Title: fmt.Sprintf("slot routed %03d", i), Type: "task",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate + "-2"},
		})
		if err != nil {
			t.Fatalf("seeding row %d: %v", i, err)
		}
		rows = append(rows, created)
		stores = append(stores, store)
	}
	return rows, stores
}

// reloadRows re-reads every row from the store, as the next build's collection
// would.
func reloadRows(t *testing.T, store beads.Store, rows []beads.Bead) []beads.Bead {
	t.Helper()
	out := make([]beads.Bead, 0, len(rows))
	for _, row := range rows {
		fresh, err := store.Get(row.ID)
		if err != nil {
			t.Fatalf("re-reading %s: %v", row.ID, err)
		}
		out = append(out, fresh)
	}
	return out
}

func repairedRouteCount(t *testing.T, store beads.Store, rows []beads.Bead) int {
	t.Helper()
	n := 0
	for _, row := range reloadRows(t, store, rows) {
		if strings.TrimSpace(row.Metadata[beadmeta.RoutedToMetadataKey]) == agreementTemplate {
			n++
		}
	}
	return n
}

func TestRepairWriteBudgetCapsAttemptsPerBuild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &budgetTestStore{Store: beads.NewMemStore()}
		rows, stores := seedSlotRoutedBacklog(t, store, 4*repairWritesPerBuild)
		budget := newRepairWriteBudget().forDomain(t.Name())

		collapseSlotSuffixedRoutedWork(agreementConfig(), rows, stores, budget, io.Discard)

		if store.writes == 0 || store.writes > repairWritesPerBuild {
			t.Fatalf("writes = %d, want 0 < writes <= %d", store.writes, repairWritesPerBuild)
		}
		if !budget.exhausted() {
			t.Error("exhausted() = false after the attempt cap was spent")
		}
		if got := repairedRouteCount(t, store, rows); got != store.writes {
			t.Errorf("repaired rows = %d, want %d (one per write)", got, store.writes)
		}
	})
}

func TestRepairWriteBudgetTimeCapStopsBeforeAttemptCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const perWrite = 4 * time.Second
		store := &budgetTestStore{Store: beads.NewMemStore(), delay: perWrite}
		rows, stores := seedSlotRoutedBacklog(t, store, 4*repairWritesPerBuild)
		budget := newRepairWriteBudget().forDomain(t.Name())

		start := time.Now()
		collapseSlotSuffixedRoutedWork(agreementConfig(), rows, stores, budget, io.Discard)
		elapsed := time.Since(start)

		if store.writes == 0 || store.writes >= repairWritesPerBuild {
			t.Fatalf("writes = %d, want the time cap to stop it short of the attempt cap %d", store.writes, repairWritesPerBuild)
		}
		// The write in flight when the cap is crossed finishes; no further one starts.
		if elapsed >= repairWriteTimeBudget+perWrite {
			t.Errorf("spent %s in writes, want < %s (cap %s plus the one write in flight)", elapsed, repairWriteTimeBudget+perWrite, repairWriteTimeBudget)
		}
		if !budget.exhausted() {
			t.Error("exhausted() = false after the time cap was spent")
		}
	})
}

func TestRepairWriteBudgetChargesWriteTimeOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &budgetTestStore{Store: beads.NewMemStore()}
		rows, stores := seedSlotRoutedBacklog(t, store, 4*repairWritesPerBuild)
		budget := newRepairWriteBudget().forDomain(t.Name())

		// Time spent outside a write (reads, classification) is not the repairs'
		// to pay for: a build that idles before it repairs still gets its writes.
		time.Sleep(10 * repairWriteTimeBudget)
		collapseSlotSuffixedRoutedWork(agreementConfig(), rows, stores, budget, io.Discard)

		if store.writes != repairWritesPerBuild {
			t.Fatalf("writes = %d, want the full attempt cap %d (idle time must not be charged)", store.writes, repairWritesPerBuild)
		}
	})
}

func TestRepairWriteBudgetCountsFailedWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &budgetTestStore{Store: beads.NewMemStore(), failAll: true}
		rows, stores := seedSlotRoutedBacklog(t, store, 4*repairWritesPerBuild)
		budget := newRepairWriteBudget().forDomain(t.Name())

		var stderr bytes.Buffer
		collapseSlotSuffixedRoutedWork(agreementConfig(), rows, stores, budget, &stderr)

		if store.writes != repairWritesPerBuild {
			t.Fatalf("attempted writes = %d, want %d: a failed write spends its attempt", store.writes, repairWritesPerBuild)
		}
		if got := strings.Count(stderr.String(), "collapseSlotSuffixedRoutedWork:"); got != repairWritesPerBuild {
			t.Errorf("logged %d write failures, want %d (a refused write is not an error)", got, repairWritesPerBuild)
		}
	})
}

func TestRepairWriteBudgetWriteContract(t *testing.T) {
	storeA := &budgetTestStore{Store: beads.NewMemStore()}
	storeB := &budgetTestStore{Store: beads.NewMemStore()}
	budget := newRepairWriteBudget().forDomain(t.Name())

	if budget.attempted(storeA) {
		t.Fatal("attempted(storeA) = true before any write")
	}
	var seenInside bool
	ran, err := budget.write(storeA, func() error {
		seenInside = budget.attempted(storeA)
		return errBudgetTestWrite
	})
	if !ran || !errors.Is(err, errBudgetTestWrite) {
		t.Fatalf("write() = (%v, %v), want the failed write to run and return its error", ran, err)
	}
	if !seenInside {
		t.Error("the attempt was not recorded before the write ran")
	}
	if !budget.attempted(storeA) {
		t.Error("attempted(storeA) = false after a failed write")
	}
	if budget.attempted(storeB) {
		t.Error("attempted(storeB) = true: an attempt on store A must leave store B unattempted")
	}

	// Spend what is left; the refused write must not run, must not be an error,
	// and must not mark its store attempted.
	for range repairWritesPerBuild - 1 {
		if ran, _ := budget.write(storeA, func() error { return nil }); !ran {
			t.Fatal("write refused before the attempt cap was spent")
		}
	}
	refusedRan := false
	ran, err = budget.write(storeB, func() error { refusedRan = true; return nil })
	if ran || err != nil || refusedRan {
		t.Fatalf("write() past the cap = (ran=%v, err=%v, fn ran=%v), want a silent refusal", ran, err, refusedRan)
	}
	if budget.attempted(storeB) {
		t.Error("a refused write marked its store attempted")
	}
	if !budget.exhausted() {
		t.Error("exhausted() = false with the attempt cap spent")
	}
}

func TestRepairWriteBudgetAttemptedLooksThroughBeadPolicyWrapper(t *testing.T) {
	raw := &budgetTestStore{Store: beads.NewMemStore()}
	wrapped := wrapStoreWithBeadPolicies(raw, nil)
	if inner, _, ok := unwrapBeadPolicyStore(wrapped); !ok || inner != beads.Store(raw) {
		t.Fatalf("test premise: %T is not a policy wrapper over the raw store", wrapped)
	}

	viaWrapper := newRepairWriteBudget().forDomain(t.Name())
	if _, err := viaWrapper.write(wrapped, func() error { return nil }); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !viaWrapper.attempted(raw) {
		t.Error("a write through the wrapper left the unwrapped store unattempted")
	}

	viaRaw := newRepairWriteBudget().forDomain(t.Name())
	if _, err := viaRaw.write(raw, func() error { return nil }); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !viaRaw.attempted(wrapped) {
		t.Error("a write to the unwrapped store left the wrapper unattempted")
	}
}

func TestNilRepairWriteBudgetIsUnbounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var budget *repairWriteBudget
		other := &budgetTestStore{Store: beads.NewMemStore()}
		if !budget.attempted(other) {
			t.Error("nil budget: attempted() = false, want true for every store")
		}
		if budget.exhausted() {
			t.Error("nil budget: exhausted() = true")
		}
		ran := 0
		for range 3 * repairWritesPerBuild {
			attempted, err := budget.write(other, func() error { ran++; return nil })
			if !attempted || err != nil {
				t.Fatalf("nil budget write() = (%v, %v), want it to just run", attempted, err)
			}
		}
		if ran != 3*repairWritesPerBuild {
			t.Fatalf("ran %d writes, want %d", ran, 3*repairWritesPerBuild)
		}

		store := &budgetTestStore{Store: beads.NewMemStore(), delay: time.Minute}
		rows, stores := seedSlotRoutedBacklog(t, store, 3*repairWritesPerBuild)
		collapseSlotSuffixedRoutedWork(agreementConfig(), rows, stores, nil, io.Discard)
		if got := repairedRouteCount(t, store, rows); got != len(rows) {
			t.Errorf("repaired %d of %d rows with a nil budget, want all", got, len(rows))
		}
	})
}

func TestRepairWriteBudgetRotationReachesTheTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const total = 4 * repairWritesPerBuild
		poison := map[string]bool{}
		store := &budgetTestStore{Store: beads.NewMemStore(), failIDs: poison}
		rows, stores := seedSlotRoutedBacklog(t, store, total)
		// The head of the list never repairs. Without a rotating start every
		// build would spend its whole budget on it and never reach the tail.
		for _, row := range rows[:repairWritesPerBuild] {
			poison[row.ID] = true
		}
		cfg := agreementConfig()
		domain := t.Name()

		for build := 1; build <= 3; build++ {
			budget := newRepairWriteBudget().forDomain(domain)
			store.writes = 0
			collapseSlotSuffixedRoutedWork(cfg, reloadRows(t, store, rows), stores, budget, io.Discard)
			if store.writes == 0 || store.writes > repairWritesPerBuild {
				t.Fatalf("build %d: writes = %d, want 0 < writes <= %d", build, store.writes, repairWritesPerBuild)
			}
			want := (build - 1) * repairWritesPerBuild
			if got := repairedRouteCount(t, store, rows); got != want {
				t.Fatalf("after build %d: %d rows repaired, want %d (the next build must resume where the last stopped)", build, got, want)
			}
		}
		// The fourth build reaches the last of the tail.
		budget := newRepairWriteBudget().forDomain(domain)
		collapseSlotSuffixedRoutedWork(cfg, reloadRows(t, store, rows), stores, budget, io.Discard)
		if got := repairedRouteCount(t, store, rows); got != total-repairWritesPerBuild {
			t.Fatalf("after the tail build: %d rows repaired, want %d (everything but the poisoned head)", got, total-repairWritesPerBuild)
		}
	})
}

func TestRepairWriteBudgetRotationIsPerDomain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &budgetTestStore{Store: beads.NewMemStore()}
		rows, stores := seedSlotRoutedBacklog(t, store, 2*repairWritesPerBuild)
		cfg := agreementConfig()

		// City one stops mid-list and stores a hint. City two, a different
		// domain, must still start at the head.
		collapseSlotSuffixedRoutedWork(cfg, rows, stores, newRepairWriteBudget().forDomain(t.Name()+"/one"), io.Discard)
		store.written = nil
		collapseSlotSuffixedRoutedWork(cfg, rows, stores, newRepairWriteBudget().forDomain(t.Name()+"/two"), io.Discard)

		if len(store.written) == 0 || store.written[0] != rows[0].ID {
			t.Fatalf("second city's first write = %v, want %s: it must not inherit the first city's resume hint", store.written, rows[0].ID)
		}
	})
}

func TestRepairWriteBudgetSteadyStateWritesNothingAndSaysNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &budgetTestStore{Store: beads.NewMemStore()}
		rows, stores := seedSlotRoutedBacklog(t, store, 3)
		cfg := agreementConfig()
		collapseSlotSuffixedRoutedWork(cfg, rows, stores, newRepairWriteBudget().forDomain(t.Name()), io.Discard)

		store.writes = 0
		budget := newRepairWriteBudget().forDomain(t.Name())
		collapseSlotSuffixedRoutedWork(cfg, reloadRows(t, store, rows), stores, budget, io.Discard)
		var stderr bytes.Buffer
		budget.report(&stderr)

		if store.writes != 0 {
			t.Errorf("steady state wrote %d times, want 0", store.writes)
		}
		if budget.attemptCount() != 0 || budget.exhausted() {
			t.Errorf("steady state budget = (attempts %d, exhausted %v), want (0, false)", budget.attemptCount(), budget.exhausted())
		}
		if stderr.Len() != 0 {
			t.Errorf("steady state printed %q, want no budget line", stderr.String())
		}
	})
}

func TestRepairWriteBudgetReportsOneLineWhenRepairsWereDeferred(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &budgetTestStore{Store: beads.NewMemStore()}
		rows, stores := seedSlotRoutedBacklog(t, store, 2*repairWritesPerBuild)
		budget := newRepairWriteBudget().forDomain(t.Name())
		collapseSlotSuffixedRoutedWork(agreementConfig(), rows, stores, budget, io.Discard)

		var stderr bytes.Buffer
		budget.report(&stderr)
		line := strings.TrimRight(stderr.String(), "\n")
		if line == "" || strings.Contains(line, "\n") {
			t.Fatalf("report = %q, want exactly one line", stderr.String())
		}
		for _, want := range []string{fmt.Sprint(repairWritesPerBuild), fmt.Sprint(len(rows) - repairWritesPerBuild)} {
			if !strings.Contains(line, want) {
				t.Errorf("report %q lacks %q (attempts, time spent and repairs deferred)", line, want)
			}
		}

		var nilBudget *repairWriteBudget
		stderr.Reset()
		nilBudget.report(&stderr)
		if stderr.Len() != 0 {
			t.Errorf("nil budget printed %q", stderr.String())
		}
	})
}

func TestRepairControlDispatcherRoutesDrawsFromTheSharedBudget(t *testing.T) {
	maxActive := 1
	cfg := &config.City{Agents: []config.Agent{{
		Name:              config.ControlDispatcherAgentName,
		BindingName:       "core",
		Dir:               "fixture",
		StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
		MaxActiveSessions: &maxActive,
	}}}
	const count = 2 * controlDispatcherRouteRepairLimitPerTick
	run := func(domain string, budget *repairWriteBudget) int {
		seed := make([]beads.Bead, 0, count)
		for i := range count {
			seed = append(seed, beads.Bead{
				ID: fmt.Sprintf("control-%02d", i), Type: "task", Status: "open",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
					beadmeta.RoutedToMetadataKey:     "core.control-dispatcher",
					beadmeta.RootStoreRefMetadataKey: "rig:fixture",
				},
			})
		}
		store := &routeRepairCountingStore{Store: beads.NewMemStoreFrom(1, seed, nil)}
		work := append([]beads.Bead(nil), seed...)
		stores := make([]beads.Store, count)
		refs := make([]string, count)
		for i := range count {
			stores[i] = store
			refs[i] = "fixture"
		}
		repairControlDispatcherRoutesForStoreScope(domain, cfg, work, stores, refs, budget, io.Discard)
		return store.updates
	}

	// Its own cap still holds with budget to spare...
	if got := run(t.Name()+"/own-cap", newRepairWriteBudget().forDomain(t.Name()+"/own-cap")); got != controlDispatcherRouteRepairLimitPerTick {
		t.Errorf("updates with a fresh shared budget = %d, want the dispatcher's own cap %d", got, controlDispatcherRouteRepairLimitPerTick)
	}

	// ...and the shared budget caps it below that when the other repairs spent it.
	spent := newRepairWriteBudget().forDomain(t.Name() + "/shared")
	other := &budgetTestStore{Store: beads.NewMemStore()}
	for range repairWritesPerBuild - 2 {
		if _, err := spent.write(other, func() error { return nil }); err != nil {
			t.Fatalf("pre-spending the budget: %v", err)
		}
	}
	if got := run(t.Name()+"/shared", spent); got != 2 {
		t.Errorf("updates with 2 shared writes left = %d, want 2", got)
	}
}

func rootMemoFixture(t *testing.T, steps int, rootExists bool) (*budgetTestStore, []beads.Bead, *sessionBeadSnapshot) {
	t.Helper()
	const sessionName = "rootmemo-worker-1"
	seed := []beads.Bead{}
	if rootExists {
		seed = append(seed, beads.Bead{ID: "rm-root", Type: "molecule", Status: "in_progress", Metadata: map[string]string{"gc.kind": "workflow"}})
	}
	work := make([]beads.Bead, 0, steps)
	for i := range steps {
		step := beads.Bead{
			ID: fmt.Sprintf("rm-step-%02d", i), Type: "step", Status: "in_progress", Assignee: sessionName,
			Metadata: map[string]string{"gc.root_bead_id": "rm-root"},
		}
		seed = append(seed, step)
		work = append(work, step)
	}
	store := &budgetTestStore{Store: beads.NewMemStoreFrom(0, seed, nil)}
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession(sessionName, "/home/ds/rootmemo-worker-1")})
	return store, work, sessions
}

func TestStampRunSessionIdentityReadsAMissingRootOncePerBuild(t *testing.T) {
	const steps = 10
	store, work, sessions := rootMemoFixture(t, steps, false)
	stores := make([]beads.Store, steps)
	for i := range stores {
		stores[i] = store
	}

	stampRunSessionIdentity(gaConfig(), work, stores, sessions, nil, newRootGetMemo(), io.Discard)
	if got := store.gets["rm-root"]; got != 1 {
		t.Fatalf("Get(rm-root) ran %d times in one build over %d steps, want 1 (the failure is remembered for the rest of the build)", got, steps)
	}

	// The next build retries: the failure is not remembered across builds, and
	// a root that has appeared by then is stamped.
	if _, err := store.Create(beads.Bead{ID: "rm-root", Type: "molecule", Status: "in_progress", Metadata: map[string]string{"gc.kind": "workflow"}}); err != nil {
		t.Fatalf("creating the root: %v", err)
	}
	stampRunSessionIdentity(gaConfig(), reloadRows(t, store, work), stores, sessions, nil, newRootGetMemo(), io.Discard)
	if got := store.gets["rm-root"]; got != 2 {
		t.Fatalf("Get(rm-root) ran %d times over two builds, want 2 (one per build)", got)
	}
	root, err := store.Store.Get("rm-root")
	if err != nil {
		t.Fatalf("Get(rm-root): %v", err)
	}
	if root.Metadata[beadmeta.SessionNameMetadataKey] == "" {
		t.Error("the second build did not stamp the root that now exists")
	}
}

func TestStampRunSessionIdentityReadsASharedRootOncePerStore(t *testing.T) {
	const steps = 6
	storeA, workA, sessions := rootMemoFixture(t, steps, true)
	storeB, _, _ := rootMemoFixture(t, steps, true)
	work := append([]beads.Bead(nil), workA...)
	stores := make([]beads.Store, 0, 2*steps)
	for range steps {
		stores = append(stores, storeA)
	}
	// The same step ids routed through a second store: the memo is keyed on the
	// store as well as the root id, so each store's root is read exactly once.
	for _, step := range workA {
		work = append(work, step)
		stores = append(stores, storeB)
	}

	stampRunSessionIdentity(gaConfig(), work, stores, sessions, nil, newRootGetMemo(), io.Discard)

	if storeA.gets["rm-root"] != 1 || storeB.gets["rm-root"] != 1 {
		t.Fatalf("Get(rm-root) per store = (%d, %d), want (1, 1)", storeA.gets["rm-root"], storeB.gets["rm-root"])
	}
	for name, store := range map[string]*budgetTestStore{"A": storeA, "B": storeB} {
		root, err := store.Store.Get("rm-root")
		if err != nil {
			t.Fatalf("store %s Get(rm-root): %v", name, err)
		}
		if root.Metadata[beadmeta.SessionNameMetadataKey] == "" {
			t.Errorf("store %s root was not stamped", name)
		}
	}
}
