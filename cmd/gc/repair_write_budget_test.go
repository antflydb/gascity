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
	"github.com/gastownhall/gascity/internal/runtime"
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
	hidden  map[string]bool // ids whose Get reports not-found, as a root not yet visible

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
	if s.hidden[id] {
		return beads.Bead{}, fmt.Errorf("getting bead %q: %w", id, beads.ErrNotFound)
	}
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
		<-time.After(10 * repairWriteTimeBudget)
		collapseSlotSuffixedRoutedWork(agreementConfig(), rows, stores, budget, io.Discard)

		if store.writes == 0 || store.writes > repairWritesPerBuild {
			t.Fatalf("writes = %d, want 0 < writes <= %d (idle time must not be charged)", store.writes, repairWritesPerBuild)
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

		if store.writes == 0 || store.writes > repairWritesPerBuild {
			t.Fatalf("attempted writes = %d, want 0 < writes <= %d: a failed write spends its attempt", store.writes, repairWritesPerBuild)
		}
		if got := strings.Count(stderr.String(), "collapseSlotSuffixedRoutedWork:"); got != store.writes {
			t.Errorf("logged %d write failures, want %d (a refused write is not an error)", got, store.writes)
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
	seed = append(seed, beads.Bead{ID: "rm-root", Type: "molecule", Status: "in_progress", Metadata: map[string]string{"gc.kind": "workflow"}})
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
	if !rootExists {
		store.hidden = map[string]bool{"rm-root": true}
	}
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
	store.hidden = nil
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

// A write that ends at exactly repairWriteTimeBudget spends the time cap: the
// check is >=, so the next write does not start.
func TestRepairWriteBudgetTimeCapIsSpentAtExactlyTheBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &budgetTestStore{Store: beads.NewMemStore()}
		budget := newRepairWriteBudget().forDomain(t.Name())

		if ran, err := budget.write(store, func() error { <-time.After(repairWriteTimeBudget); return nil }); !ran || err != nil {
			t.Fatalf("first write = (%v, %v), want it to run", ran, err)
		}
		if !budget.exhausted() {
			t.Error("exhausted() = false after a write that took exactly the time cap")
		}
		ran := false
		if attempted, _ := budget.write(store, func() error { ran = true; return nil }); attempted || ran {
			t.Errorf("a write after the time cap was spent exactly ran (attempted=%v, fn ran=%v)", attempted, ran)
		}
	})
}

// A pass that finishes within budget leaves the resume hint alone: only a pass
// that was cut off stores one.
func TestRepairWriteBudgetFinishedPassLeavesTheHintAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const total = repairWritesPerBuild + 5
		store := &budgetTestStore{Store: beads.NewMemStore()}
		rows, stores := seedSlotRoutedBacklog(t, store, total)
		cfg := agreementConfig()
		domain := t.Name()
		pass := func() {
			store.written = nil
			collapseSlotSuffixedRoutedWork(cfg, reloadRows(t, store, rows), stores, newRepairWriteBudget().forDomain(domain), io.Discard)
		}

		pass() // cut off at row 25, which stores the hint
		pass() // starts at row 25 and finishes within budget
		if len(store.written) != total-repairWritesPerBuild || store.written[0] != rows[repairWritesPerBuild].ID {
			t.Fatalf("second pass wrote %v, want the %d rows past the cut-off starting at %s", store.written, total-repairWritesPerBuild, rows[repairWritesPerBuild].ID)
		}

		for _, row := range rows {
			if err := store.Store.SetMetadataBatch(row.ID, map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate + "-2"}); err != nil {
				t.Fatalf("re-breaking %s: %v", row.ID, err)
			}
		}
		pass()
		if len(store.written) == 0 || store.written[0] != rows[repairWritesPerBuild].ID {
			t.Fatalf("third pass started at %v, want %s: the finished pass must not have moved the hint", store.written, rows[repairWritesPerBuild].ID)
		}
	})
}

const (
	budgetPoolSlot  = ".gc/worktrees/gascity/builder-1"
	budgetEvidence  = "/home/ds/gascity-worktrees/ga-3c5isi"
	budgetLegacy    = "rig-A/gc.planner"
	budgetCanonical = "rig-A/planner"
	budgetSession   = "budget-worker-1"
	budgetSessionWD = "/home/ds/budget-worker-1"
)

// budgetWriter is one repair writer plus a row it repairs with exactly one durable write.
type budgetWriter struct {
	name     string
	cfg      func() *config.City
	seed     func(i int) beads.Bead
	run      func(budget *repairWriteBudget, rows []beads.Bead, stores []beads.Store)
	repaired func(b beads.Bead) bool
	// assigned reports whether the build collects this writer's rows with the assigned work (else the open, unassigned routed work).
	assigned bool
	// extra seeds beyond the repairable rows (a session bead the stamp needs).
	extra []beads.Bead
}

func budgetWriters() []budgetWriter {
	rowID := func(prefix string, i int) string { return fmt.Sprintf("%s-%03d", prefix, i) }
	sessions := func() *sessionBeadSnapshot {
		return newSessionBeadSnapshot([]beads.Bead{stampTestSession(budgetSession, budgetSessionWD)})
	}
	return []budgetWriter{
		{
			name: "pool-slot-assigned", cfg: gaConfig, assigned: true,
			seed: func(i int) beads.Bead {
				return beads.Bead{
					ID: rowID("psa", i), Type: "task", Status: "in_progress", Assignee: "someone",
					Metadata: map[string]string{beadmeta.WorkDirMetadataKey: budgetPoolSlot, beadmeta.LegacyWorkDirMetadataKey: budgetEvidence},
				}
			},
			run: func(b *repairWriteBudget, rows []beads.Bead, stores []beads.Store) {
				repairPoolSlotWorkDirClobber(gaConfig(), rows, stores, b, "assigned", io.Discard)
			},
			repaired: func(b beads.Bead) bool { return b.Metadata[beadmeta.WorkDirMetadataKey] == budgetEvidence },
		},
		{
			name: "pool-slot-unassigned", cfg: gaConfig,
			seed: func(i int) beads.Bead {
				return beads.Bead{
					ID: rowID("psu", i), Type: "task", Status: "open",
					Metadata: map[string]string{
						beadmeta.WorkDirMetadataKey: budgetPoolSlot, beadmeta.LegacyWorkDirMetadataKey: budgetEvidence,
						beadmeta.RoutedToMetadataKey: "rig-A/planner",
					},
				}
			},
			run: func(b *repairWriteBudget, rows []beads.Bead, stores []beads.Store) {
				repairPoolSlotWorkDirClobber(gaConfig(), rows, stores, b, "unassigned", io.Discard)
			},
			repaired: func(b beads.Bead) bool { return b.Metadata[beadmeta.WorkDirMetadataKey] == budgetEvidence },
		},
		{
			name: "stamp-run-session-identity", cfg: gaConfig, assigned: true,
			extra: []beads.Bead{{
				ID: "sess-budget", Type: sessionBeadType, Labels: []string{"gc:session"}, Status: "open",
				Metadata: map[string]string{"session_name": budgetSession, "work_dir": budgetSessionWD, "state": "active"},
			}},
			seed: func(i int) beads.Bead {
				return beads.Bead{ID: rowID("sti", i), Type: "molecule", Status: "in_progress", Assignee: budgetSession}
			},
			run: func(b *repairWriteBudget, rows []beads.Bead, stores []beads.Store) {
				stampRunSessionIdentity(gaConfig(), rows, stores, sessions(), b, newRootGetMemo(), io.Discard)
			},
			repaired: func(b beads.Bead) bool { return b.Metadata[beadmeta.SessionNameMetadataKey] == budgetSession },
		},
		{
			name: "canonicalize-legacy-bound-assigned", cfg: legacyBoundRecoveryConfig, assigned: true,
			seed: func(i int) beads.Bead { return workBead(rowID("cla", i), budgetLegacy, budgetLegacy, "in_progress", 5) },
			run: func(b *repairWriteBudget, rows []beads.Bead, stores []beads.Store) {
				canonicalizeLegacyBoundAssignedWork(legacyBoundRecoveryConfig(), rows, stores, newSessionBeadSnapshot(nil), b, io.Discard)
			},
			repaired: func(b beads.Bead) bool { return b.Assignee == budgetCanonical },
		},
		{
			name: "canonicalize-legacy-bound-unassigned", cfg: legacyBoundRecoveryConfig,
			seed: func(i int) beads.Bead { return workBead(rowID("clu", i), budgetLegacy, "", "open", 5) },
			run: func(b *repairWriteBudget, rows []beads.Bead, stores []beads.Store) {
				canonicalizeLegacyBoundUnassignedRoutedWork(legacyBoundRecoveryConfig(), rows, stores, b, io.Discard)
			},
			repaired: func(b beads.Bead) bool { return b.Metadata[beadmeta.RoutedToMetadataKey] == budgetCanonical },
		},
		{
			name: "collapse-slot-suffixed", cfg: agreementConfig,
			seed: func(i int) beads.Bead {
				return beads.Bead{
					ID: rowID("col", i), Type: "task", Status: "open",
					Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate + "-2"},
				}
			},
			run: func(b *repairWriteBudget, rows []beads.Bead, stores []beads.Store) {
				collapseSlotSuffixedRoutedWork(agreementConfig(), rows, stores, b, io.Discard)
			},
			repaired: func(b beads.Bead) bool { return b.Metadata[beadmeta.RoutedToMetadataKey] == agreementTemplate },
		},
	}
}

// budgetBacklogRows is the length of the repairable backlog the build-level
// pins seed: three budgets' worth, so a bounded pass is visibly cut off.
const budgetBacklogRows = 3 * repairWritesPerBuild

// budgetBacklogStore seeds budgetBacklogRows repairable rows for w; the first
// failFirst always fail their write.
func budgetBacklogStore(w budgetWriter, failFirst int) (*budgetTestStore, []beads.Bead, []beads.Store) {
	n := budgetBacklogRows
	seed := make([]beads.Bead, 0, n+len(w.extra))
	rows := make([]beads.Bead, n)
	for i := range n {
		rows[i] = w.seed(i)
		seed = append(seed, rows[i])
	}
	seed = append(seed, w.extra...)
	store := &budgetTestStore{Store: beads.NewMemStoreFrom(0, seed, nil), failIDs: map[string]bool{}}
	for i := range failFirst {
		store.failIDs[rows[i].ID] = true
	}
	stores := make([]beads.Store, n)
	for i := range stores {
		stores[i] = store
	}
	return store, rows, stores
}

// Every repair writer draws on the build's budget. A nil budget repairs every
// row, which shows the fixture is repairable and the bounded pass is not vacuous.
func TestRepairWriteBudgetEveryRepairWriterDrawsOnTheBuildBudget(t *testing.T) {
	for _, w := range budgetWriters() {
		t.Run(w.name, func(t *testing.T) {
			n := budgetBacklogRows
			free, freeRows, freeStores := budgetBacklogStore(w, 0)
			w.run(nil, freeRows, freeStores)
			if free.writes != n {
				t.Fatalf("nil budget: %d writes over %d repairable rows, want %d (the fixture must be repairable)", free.writes, n, n)
			}
			store, rows, stores := budgetBacklogStore(w, 0)
			w.run(newRepairWriteBudget().forDomain(t.Name()), rows, stores)
			if store.writes == 0 || store.writes > repairWritesPerBuild {
				t.Fatalf("%d writes over %d repairable rows, want 0 < writes <= %d", store.writes, n, repairWritesPerBuild)
			}
		})
	}
}

// Every repair writer rotates: after a cut-off build stores its resume index the
// next build starts there, so a head that never repairs cannot hold the tail back.
func TestRepairWriteBudgetEveryRepairWriterRotatesToTheTail(t *testing.T) {
	for _, w := range budgetWriters() {
		t.Run(w.name, func(t *testing.T) {
			store, rows, stores := budgetBacklogStore(w, repairWritesPerBuild) // the head never repairs
			for range 2 {
				w.run(newRepairWriteBudget().forDomain(t.Name()), reloadRows(t, store, rows), stores)
			}
			tail := 0
			for _, row := range reloadRows(t, store, rows)[repairWritesPerBuild:] {
				if w.repaired(row) {
					tail++
				}
			}
			if tail == 0 {
				t.Fatalf("two builds over a head of %d rows that never repair reached no row past the head: the second build re-tried the head", repairWritesPerBuild)
			}
		})
	}
}

// Each writer site keeps its own rotation hint within one repair domain: one
// writer being cut off must not move another writer's start.
func TestRepairWriteBudgetRotationHintsAreIndependentPerWriterSite(t *testing.T) {
	writers := budgetWriters()
	for ai, a := range writers {
		for bi, b := range writers {
			if ai == bi {
				continue
			}
			t.Run(a.name+"_then_"+b.name, func(t *testing.T) {
				domain := t.Name()
				storeA, rowsA, storesA := budgetBacklogStore(a, repairWritesPerBuild)
				a.run(newRepairWriteBudget().forDomain(domain), rowsA, storesA) // stops at index 25: its hint is 25
				storeB, rowsB, storesB := budgetBacklogStore(b, repairWritesPerBuild)
				b.run(newRepairWriteBudget().forDomain(domain), rowsB, storesB)
				if len(storeB.written) == 0 || storeB.written[0] != rowsB[0].ID {
					t.Fatalf("%s's first pass in the same domain started at %v, want its own head %s: the hint of %s leaked", b.name, storeB.written, rowsB[0].ID, a.name)
				}
				_ = storeA
			})
		}
	}
}

// The stamp writer's root write draws on the same budget as its step write.
func TestRepairWriteBudgetStampDrawsRootWritesOnTheSameBudget(t *testing.T) {
	const steps = 3 * repairWritesPerBuild
	seed := []beads.Bead{}
	work := []beads.Bead{}
	for i := range steps {
		rootID := fmt.Sprintf("pr-root-%03d", i)
		seed = append(seed, beads.Bead{ID: rootID, Type: "molecule", Status: "in_progress", Metadata: map[string]string{"gc.kind": "workflow"}})
		step := beads.Bead{
			ID: fmt.Sprintf("pr-step-%03d", i), Type: "step", Status: "in_progress", Assignee: budgetSession,
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
		}
		seed = append(seed, step)
		work = append(work, step)
	}
	store := &budgetTestStore{Store: beads.NewMemStoreFrom(0, seed, nil)}
	stores := make([]beads.Store, len(work))
	for i := range stores {
		stores[i] = store
	}
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession(budgetSession, budgetSessionWD)})
	stampRunSessionIdentity(gaConfig(), work, stores, sessions, newRepairWriteBudget().forDomain(t.Name()), newRootGetMemo(), io.Discard)
	if store.writes == 0 || store.writes > repairWritesPerBuild {
		t.Fatalf("%d writes (step + root) over %d steps with their own roots, want 0 < writes <= %d", store.writes, steps, repairWritesPerBuild)
	}
}

// A root shared by several steps is written once per build.
func TestRepairWriteBudgetStampWritesASharedRootOncePerBuild(t *testing.T) {
	const steps = 4
	store, work, sessions := rootMemoFixture(t, steps, true)
	stores := make([]beads.Store, steps)
	for i := range stores {
		stores[i] = store
	}
	stampRunSessionIdentity(gaConfig(), work, stores, sessions, newRepairWriteBudget().forDomain(t.Name()), newRootGetMemo(), io.Discard)
	rootWrites := 0
	for _, id := range store.written {
		if id == "rm-root" {
			rootWrites++
		}
	}
	if rootWrites != 1 {
		t.Fatalf("the shared root was written %d times over %d steps in one build, want exactly 1 (writes %v)", rootWrites, steps, store.written)
	}
}

// budgetBuildIn runs one desired-state build over a store holding only w's backlog, in the given city directory
// (a fresh directory is a fresh rotation domain), and returns the store and the build's stderr.
func budgetBuildIn(t *testing.T, cityDir string, w budgetWriter, n int, cycle *SessionReconcilerTraceCycle) (*budgetTestStore, string) {
	t.Helper()
	seed := make([]beads.Bead, 0, n+len(w.extra))
	for i := range n {
		seed = append(seed, w.seed(i))
	}
	seed = append(seed, w.extra...)
	store := &budgetTestStore{Store: beads.NewMemStoreFrom(0, seed, nil)}
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}
	var out bytes.Buffer
	buildDesiredStateWithSessionBeads("test-city", cityDir, time.Now().UTC(), w.cfg(), runtime.NewFake(), store, nil, snapshot, cycle, &out)
	return store, out.String()
}

// budgetRowWrites lists the ids of the repair writes a build made, in order (the session bead is not a repair row).
func budgetRowWrites(store *budgetTestStore) []string {
	var ids []string
	for _, id := range store.written {
		if !strings.HasPrefix(id, "sess-") {
			ids = append(ids, id)
		}
	}
	return ids
}

// The desired-state build starts no more than repairWritesPerBuild repair writes
// across any one writer family, and says so once on stderr.
func TestRepairWriteBudgetDesiredStateBuildBoundsEveryRepairWriter(t *testing.T) {
	for _, w := range budgetWriters() {
		t.Run(w.name, func(t *testing.T) {
			n := budgetBacklogRows
			store, stderr := budgetBuildIn(t, t.TempDir(), w, n, nil)
			writes := len(budgetRowWrites(store))
			if writes == 0 {
				t.Fatalf("the build attempted no repair of %d repairable rows: the fixture is not collected by the build (stderr %q)", n, stderr)
			}
			if writes > repairWritesPerBuild {
				t.Fatalf("one build attempted %d repair writes over %d rows, want <= %d", writes, n, repairWritesPerBuild)
			}
			if got := strings.Count(stderr, "repairWriteBudget: exhausted"); got != 1 {
				t.Fatalf("stderr carries %d budget lines, want exactly 1 (stderr %q, writes %d)", got, stderr, len(store.written))
			}
			if want := fmt.Sprintf("exhausted after %d write attempts", repairWritesPerBuild); !strings.Contains(stderr, want) {
				t.Fatalf("stderr %q does not say %q", stderr, want)
			}
		})
	}
}

// Rotation at build level is per city and per pass: a second build of one city
// does not restart at the head, another city starts at its own head, and the
// unassigned pool-slot pass starts at its own head.
func TestRepairWriteBudgetBuildRotationIsPerCityAndPerPass(t *testing.T) {
	assigned, unassigned := budgetWriters()[0], budgetWriters()[1]
	const n = 3 * repairWritesPerBuild
	cityA, cityB := t.TempDir(), t.TempDir()
	firstWrite := func(dir string, w budgetWriter) string {
		t.Helper()
		store, stderr := budgetBuildIn(t, dir, w, n, nil)
		ids := budgetRowWrites(store)
		if len(ids) == 0 {
			t.Fatalf("%s: the build attempted no repair (stderr %q)", w.name, stderr)
		}
		return ids[0]
	}
	if got, head := firstWrite(cityA, assigned), assigned.seed(0).ID; got != head {
		t.Fatalf("the first build of a city started at %s, want its head %s", got, head)
	}
	if got, head := firstWrite(cityA, assigned), assigned.seed(0).ID; got == head {
		t.Fatalf("the second build of the same city started at the head %s again: the rotation did not carry across builds", head)
	}
	if got, head := firstWrite(cityB, assigned), assigned.seed(0).ID; got != head {
		t.Fatalf("another city first build started at %s, want its head %s: the first city rotation leaked", got, head)
	}
	if got, head := firstWrite(cityA, unassigned), unassigned.seed(0).ID; got != head {
		t.Fatalf("the unassigned pass started at %s, want its own head %s: the assigned pass rotation leaked", got, head)
	}
}

// The assigned_repairs and unassigned_repairs trace records carry the build's
// repair-budget attempts and whether it ran out.
func TestRepairWriteBudgetTraceRecordsTheRepairBudgetOfEachPass(t *testing.T) {
	w := budgetWriters()[0]
	for _, c := range []struct {
		name          string
		rows          int
		wantAttempts  int
		wantExhausted bool
	}{
		{"exhausted", 3 * repairWritesPerBuild, repairWritesPerBuild, true},
		{"within", 3, 3, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cityDir := t.TempDir()
			tracer, cycle := demandTraceCycle(t, cityDir, newLockedClock(0))
			budgetBuildIn(t, cityDir, w, c.rows, cycle)
			byName := demandTraceRecords(t, cityDir, tracer, cycle)
			for _, name := range []string{"assigned_repairs", "unassigned_repairs"} {
				recs := byName["demand_snapshot."+name]
				if len(recs) != 1 {
					t.Fatalf("%s: %d records, want exactly 1", name, len(recs))
				}
				if got := demandTraceInt(t, recs[0], "budget_attempts"); got != c.wantAttempts {
					t.Errorf("%s budget_attempts = %d, want %d", name, got, c.wantAttempts)
				}
				if got, _ := recs[0].Fields["budget_exhausted"].(bool); got != c.wantExhausted {
					t.Errorf("%s budget_exhausted = %v, want %v", name, got, c.wantExhausted)
				}
			}
		})
	}
}

func budgetDispatcherConfig() *config.City {
	cfg := agreementConfig()
	maxActive := 1
	cfg.Agents = append(cfg.Agents, config.Agent{
		Name: config.ControlDispatcherAgentName, BindingName: "core", Dir: "rig",
		StartCommand: config.ControlDispatcherStartCommandFor("{{.Agent}}"), MaxActiveSessions: &maxActive,
	})
	return cfg
}

func budgetControlRows(n int) []beads.Bead {
	rows := make([]beads.Bead, n)
	for i := range n {
		rows[i] = beads.Bead{ID: fmt.Sprintf("ctl-%03d", i), Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:     "core.control-dispatcher",
			beadmeta.RootStoreRefMetadataKey: "rig:rig",
		}}
	}
	return rows
}

// The build's control dispatcher draws on the same budget as the other writers:
// when they spent all of it the dispatcher repairs nothing, and with budget to
// spare it repairs. A rig-rooted control row is read through the class binding on
// the split topology, so the fixture registers one (as the build-level
// dispatcher tests do).
func TestRepairWriteBudgetBuildLetsTheControlDispatcherDrawOnTheSameBudget(t *testing.T) {
	collapse := budgetWriters()[5]
	run := func(collapseRows int) (ctlWrites int, stderr string) {
		t.Helper()
		cityPath := t.TempDir()
		work := beads.NewMemStore()
		seed := append([]beads.Bead(nil), budgetControlRows(5)...)
		for i := range collapseRows {
			seed = append(seed, collapse.seed(i))
		}
		binding := &budgetTestStore{Store: beads.NewMemStoreFrom(0, seed, nil)}
		routes := splitRoutes(binding)
		registerResidencyRoutes(cityPath, routes, func() beads.Store { return work })
		t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })
		var out bytes.Buffer
		buildDesiredStateWithSessionBeads("test-city", cityPath, time.Now().UTC(), budgetDispatcherConfig(), runtime.NewFake(), binding,
			map[string]beads.Store{"rig": beads.NewMemStore()}, newSessionBeadSnapshot(nil), nil, &out)
		for _, id := range binding.written {
			if strings.HasPrefix(id, "ctl-") {
				ctlWrites++
			}
		}
		return ctlWrites, out.String()
	}
	if got, stderr := run(3); got == 0 {
		t.Fatalf("with budget to spare the build repaired no control route (stderr %q): the fixture is not collected", stderr)
	}
	if got, stderr := run(repairWritesPerBuild); got != 0 {
		t.Fatalf("the build repaired %d control routes after the other writers spent all %d writes (stderr %q)", got, repairWritesPerBuild, stderr)
	}
}

// A dispatcher repair the shared budget refused is removed from the in-memory
// demand snapshot.
func TestRepairWriteBudgetDispatcherRepairRefusedByTheBudgetIsSuppressedFromTheSnapshot(t *testing.T) {
	run := func(budget *repairWriteBudget) (*budgetTestStore, []beads.Bead) {
		t.Helper()
		store := &budgetTestStore{Store: beads.NewMemStoreFrom(0, budgetControlRows(3), nil)}
		work := budgetControlRows(3)
		stores := make([]beads.Store, len(work))
		refs := make([]string, len(work))
		for i := range work {
			stores[i], refs[i] = store, "rig"
		}
		repairControlDispatcherRoutesForStoreScope(t.Name(), budgetDispatcherConfig(), work, stores, refs, budget, io.Discard)
		return store, work
	}
	store, work := run(nil)
	if store.writes != 3 {
		t.Fatalf("unbounded run wrote %d routes, want 3 (the fixture must be repairable)", store.writes)
	}
	for _, b := range work {
		if got := b.Metadata[beadmeta.RoutedToMetadataKey]; got != "rig/core.control-dispatcher" {
			t.Fatalf("unbounded run left %s routed to %q, want the repaired route", b.ID, got)
		}
	}
	spent := newRepairWriteBudget().forDomain(t.Name())
	other := &budgetTestStore{Store: beads.NewMemStore()}
	for range repairWritesPerBuild {
		if _, err := spent.write(other, func() error { return nil }); err != nil {
			t.Fatalf("pre-spending the budget: %v", err)
		}
	}
	store, work = run(spent)
	if store.writes != 0 {
		t.Fatalf("a spent budget still let %d route writes through", store.writes)
	}
	for _, b := range work {
		if got, ok := b.Metadata[beadmeta.RoutedToMetadataKey]; ok {
			t.Errorf("%s still carries route %q in the demand snapshot after the budget refused its repair", b.ID, got)
		}
	}
}

// The one report line gives the three figures it names as three separate
// numbers: the write attempts made, the time spent inside the writes, and the
// repairs deferred to the next build. A backlog tail of a different length than
// the attempt cap tells the figures apart.
func TestRepairWriteBudgetReportNamesAttemptsTimeAndDeferredRepairsSeparately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			perWrite = repairWriteTimeBudget / (4 * repairWritesPerBuild)
			tail     = 7
		)
		store := &budgetTestStore{Store: beads.NewMemStore(), delay: perWrite}
		rows, stores := seedSlotRoutedBacklog(t, store, repairWritesPerBuild+tail)
		budget := newRepairWriteBudget().forDomain(t.Name())
		collapseSlotSuffixedRoutedWork(agreementConfig(), rows, stores, budget, io.Discard)

		var stderr bytes.Buffer
		budget.report(&stderr)
		line := strings.TrimRight(stderr.String(), "\n")
		for _, want := range []string{
			fmt.Sprintf("after %d write attempts", repairWritesPerBuild),
			"and " + (repairWritesPerBuild * perWrite).String() + " in writes",
			fmt.Sprintf("; %d repairs deferred", tail),
		} {
			if !strings.Contains(line, want) {
				t.Errorf("report %q lacks %q", line, want)
			}
		}
	})
}

// The time cap is a cap on the time the writes take, whether they succeed or
// fail: writes that each cost a fifth of the budget get about five before the
// build stops, and a store that fails slowly, which is what a write timeout
// looks like, is stopped just the same. The range, not an exact count, is the
// assertion.
func TestRepairWriteBudgetTimeCapStopsAtTheBudgetWhetherWritesSucceedOrFail(t *testing.T) {
	for _, c := range []struct {
		name    string
		failAll bool
	}{
		{"succeeding", false},
		{"failing", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const perWrite = repairWriteTimeBudget / 5
				store := &budgetTestStore{Store: beads.NewMemStore(), delay: perWrite, failAll: c.failAll}
				rows, stores := seedSlotRoutedBacklog(t, store, 4*repairWritesPerBuild)
				budget := newRepairWriteBudget().forDomain(t.Name())

				collapseSlotSuffixedRoutedWork(agreementConfig(), rows, stores, budget, io.Discard)

				if store.writes < 4 || store.writes > 6 {
					t.Fatalf("writes = %d, want about 5: each costs %s of the %s time budget", store.writes, perWrite, repairWriteTimeBudget)
				}
				if !budget.exhausted() {
					t.Error("exhausted() = false after the time cap was spent")
				}
			})
		})
	}
}

// Two workflow roots in one store are read and stamped one each: the memo keys
// a root on its id as well as on its store.
func TestStampRunSessionIdentityReadsAndStampsEachDistinctRoot(t *testing.T) {
	seed := []beads.Bead{}
	work := []beads.Bead{}
	for _, id := range []string{"dr-a", "dr-b"} {
		seed = append(seed, beads.Bead{ID: id + "-root", Type: "molecule", Status: "in_progress", Metadata: map[string]string{"gc.kind": "workflow"}})
		step := beads.Bead{
			ID: id + "-step", Type: "step", Status: "in_progress", Assignee: budgetSession,
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: id + "-root"},
		}
		seed = append(seed, step)
		work = append(work, step)
	}
	store := &budgetTestStore{Store: beads.NewMemStoreFrom(0, seed, nil)}
	stores := []beads.Store{store, store}
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession(budgetSession, budgetSessionWD)})

	stampRunSessionIdentity(gaConfig(), work, stores, sessions, nil, newRootGetMemo(), io.Discard)

	for _, rootID := range []string{"dr-a-root", "dr-b-root"} {
		if got := store.gets[rootID]; got != 1 {
			t.Errorf("Get(%s) ran %d times, want 1", rootID, got)
		}
		root, err := store.Store.Get(rootID)
		if err != nil {
			t.Fatalf("Get(%s): %v", rootID, err)
		}
		if root.Metadata[beadmeta.SessionNameMetadataKey] != budgetSession {
			t.Errorf("root %s was not stamped with its step's session", rootID)
		}
	}
}

// Every desired-state build reads a workflow root afresh. A root whose read
// failed in one build is read again, and stamped, by the next: the memo of a
// failed read lives for one build only.
func TestRepairWriteBudgetBuildReadsAnUnreadableRootAgainOnTheNextBuild(t *testing.T) {
	const rootID = "nb-root"
	seed := []beads.Bead{
		{
			ID: "sess-budget", Type: sessionBeadType, Labels: []string{"gc:session"}, Status: "open",
			Metadata: map[string]string{"session_name": budgetSession, "work_dir": budgetSessionWD, "state": "active"},
		},
		{ID: rootID, Type: "molecule", Status: "in_progress", Metadata: map[string]string{"gc.kind": "workflow"}},
		{
			ID: "nb-step", Type: "step", Status: "in_progress", Assignee: budgetSession,
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
		},
	}
	store := &budgetTestStore{Store: beads.NewMemStoreFrom(0, seed, nil), hidden: map[string]bool{rootID: true}}
	cityDir := t.TempDir()
	build := func() {
		t.Helper()
		snapshot, err := loadSessionBeadSnapshot(store)
		if err != nil {
			t.Fatalf("loadSessionBeadSnapshot: %v", err)
		}
		buildDesiredStateWithSessionBeads("test-city", cityDir, time.Now().UTC(), gaConfig(), runtime.NewFake(), store, nil, snapshot, nil, io.Discard)
	}

	build()
	if got := store.gets[rootID]; got != 1 {
		t.Fatalf("the first build read %s %d times, want 1 (the fixture's step must reach the stamp pass)", rootID, got)
	}
	store.hidden = nil
	build()
	if got := store.gets[rootID]; got != 2 {
		t.Fatalf("two builds read %s %d times, want 2: the second build retries the read the first one failed", rootID, got)
	}
	root, err := store.Store.Get(rootID)
	if err != nil {
		t.Fatalf("Get(%s): %v", rootID, err)
	}
	if root.Metadata[beadmeta.SessionNameMetadataKey] != budgetSession {
		t.Errorf("the second build did not stamp the root that had become readable (metadata %v)", root.Metadata)
	}
}

// budgetLongBacklog seeds three budgets' worth of open slot-routed rows, which
// a bounded repair pass would leave partly unrepaired.
func budgetLongBacklog(t *testing.T) (*config.City, *budgetTestStore, []beads.Bead) {
	t.Helper()
	cfg := gaConfig()
	cfg.Agents = []config.Agent{poolAgent("planner", "rig-A", intPtr(5), 0)}
	rows := make([]beads.Bead, 3*repairWritesPerBuild)
	for i := range rows {
		rows[i] = workBead(fmt.Sprintf("ga-long-%03d", i), "rig-A/planner-2", "", "open", 5)
	}
	return cfg, &budgetTestStore{Store: beads.NewMemStoreFrom(0, rows, nil)}, rows
}

// budgetLongBacklogRepaired counts the backlog rows whose slot-suffixed route
// was collapsed to the pool's route.
func budgetLongBacklogRepaired(t *testing.T, store beads.Store, rows []beads.Bead) int {
	t.Helper()
	repaired := 0
	for _, row := range reloadRows(t, store, rows) {
		if row.Metadata[beadmeta.RoutedToMetadataKey] == "rig-A/planner" {
			repaired++
		}
	}
	return repaired
}

// The v2 backstop lane runs off the reconciler tick and passes no budget: one
// run repairs a backlog three budgets long.
func TestBackstopLaneRepairsABacklogLongerThanTheBuildBudget(t *testing.T) {
	cfg, store, rows := budgetLongBacklog(t)

	runBackstopDemandRepairs(t.Context(), externalReadsEnv{Cfg: cfg, CityStore: store}, io.Discard)

	if got := budgetLongBacklogRepaired(t, store.Store, rows); got != len(rows) {
		t.Fatalf("one lane run repaired %d of %d rows, want all of them: the lane is not bounded by the build budget", got, len(rows))
	}
}

// The doctor's fix sweep passes no budget either: one sweep repairs the whole
// backlog.
func TestDoctorV2DemandMigrationsFixRepairsABacklogLongerThanTheBuildBudget(t *testing.T) {
	cfg, store, rows := budgetLongBacklog(t)
	c := demandMigrationsCheckFor(t, cfg, t.TempDir(), store, nil)

	c.sweep(true, io.Discard)

	if got := budgetLongBacklogRepaired(t, store.Store, rows); got != len(rows) {
		t.Fatalf("one fix sweep repaired %d of %d rows, want all of them: the sweep is not bounded by the build budget", got, len(rows))
	}
}
