package main

import (
	"context"
	"maps"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
)

// The cumulative prepare-time budget (ga-r7s62t, Refs #7170): one planner call
// stops preparing new candidates once the calls to prepareStartCandidateForCity
// have together spent the budget, and defers the rest to the next tick before
// any slot, lease, ticket, breaker accounting or write.

const (
	prepareBudgetOutcome = "deferred_by_prepare_budget"
	prepareBudgetReason  = "prepare_budget_spent"
	testPrepareBudget    = 60 * time.Second
)

// budgetClock is a clock.Fake safe for the async start goroutines, which read
// the clock while the executor advances it. clock.Fake has no mutex and races
// under -race.
type budgetClock struct {
	mu sync.Mutex
	f  *clock.Fake
}

func (c *budgetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.f.Now()
}

func (c *budgetClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.f.Advance(d)
}

// prepareLatencyStore makes a session bead's prepare slow: the first Get of an
// id in latency advances the clock by that duration (later Gets are free, so
// the cost of a prepare is exact), and every Get of an id in failGet fails.
// prepareStartCandidateForCity re-reads the bead inside the session mutation
// lock, and nothing reads a never-started row's id earlier in the pass, so the
// clock moves while prepare runs (TestPrepareLatencyStore_ChargesInsidePrepare).
type prepareLatencyStore struct {
	beads.Store
	clk     *budgetClock
	mu      sync.Mutex
	latency map[string]time.Duration
	failGet map[string]error
	charged map[string]bool
}

func (s *prepareLatencyStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	d := s.latency[id]
	if s.charged[id] {
		d = 0
	}
	s.charged[id] = true
	err := s.failGet[id]
	s.mu.Unlock()
	if d != 0 {
		s.clk.Advance(d)
	}
	if err != nil {
		return beads.Bead{}, err
	}
	return s.Store.Get(id)
}

// prepareBudgetEnv is a capacityEnv whose store injects prepare latency and
// whose clock is goroutine-safe.
type prepareBudgetEnv struct {
	*capacityEnv
	lat *prepareLatencyStore
	clk *budgetClock
}

func newPrepareBudgetEnv(t *testing.T, names ...string) *prepareBudgetEnv {
	t.Helper()
	e := newCapacityEnv(t, false, names...)
	lc := &budgetClock{f: e.reconcilerTestEnv.clk}
	lat := &prepareLatencyStore{
		Store:   e.store,
		clk:     lc,
		latency: map[string]time.Duration{},
		failGet: map[string]error{},
		charged: map[string]bool{},
	}
	maxWakes := 10
	e.cfg.Daemon.MaxWakesPerTick = &maxWakes
	return &prepareBudgetEnv{capacityEnv: e, lat: lat, clk: lc}
}

// candidate stores a never-started pending create for name whose prepare costs
// d of clock time.
func (e *prepareBudgetEnv) candidate(t *testing.T, name string, d time.Duration) startCandidate {
	t.Helper()
	c := e.pendingCreate(t, name, "", nil)
	e.lat.latency[c.info.ID] = d
	return c
}

// run is one planner call with the prepare budget and any extra options.
func (e *prepareBudgetEnv) run(budget time.Duration, async bool, extra []startExecutionOption, candidates ...startCandidate) int {
	return e.runWith(append(prepareBudgetOpts(budget), extra...), async, candidates...)
}

// runWith is one planner call with exactly the given extra options.
func (e *prepareBudgetEnv) runWith(extra []startExecutionOption, async bool, candidates ...startCandidate) int {
	opts := append([]startExecutionOption(nil), e.startOptions...)
	opts = append(opts, extra...)
	var tracker asyncStartTracker
	if async {
		opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker))
	}
	woken := executePlannedStartsTraced(context.Background(), candidates, e.cfg, e.desiredState, e.sp, e.lat, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, opts...)
	if async && !tracker.wait(hangBudget) {
		panic("async starts did not finish within the hang budget")
	}
	return woken
}

func (e *prepareBudgetEnv) deferredByBudget() []string {
	var names []string
	for _, r := range e.records(TraceSiteLifecycleStartRun) {
		if string(r.OutcomeCode) == prepareBudgetOutcome && string(r.ReasonCode) == prepareBudgetReason {
			names = append(names, r.SessionName)
		}
	}
	return names
}

func (e *prepareBudgetEnv) requireDeferred(t *testing.T, want ...string) {
	t.Helper()
	got := e.deferredByBudget()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("trace deferred_by_prepare_budget = %v, want %v", got, want)
	}
	for _, name := range want {
		line := "session=" + name + " "
		var logged bool
		for _, l := range strings.Split(e.log.String(), "\n") {
			if strings.Contains(l, line) && strings.Contains(l, "outcome="+prepareBudgetOutcome) {
				logged = true
			}
		}
		if !logged {
			t.Fatalf("stderr has no outcome=%s line for %s:\n%s", prepareBudgetOutcome, name, e.log.String())
		}
	}
}

// requireUntouched fails when a deferred candidate was started or written.
func (e *prepareBudgetEnv) requireUntouched(t *testing.T, c startCandidate, before beads.Bead) {
	t.Helper()
	name := c.info.SessionName
	if n := e.sp.CountCalls("Start", name); n != 0 {
		t.Fatalf("deferred %s was started %d times", name, n)
	}
	after := e.bead(t, c.info.ID)
	if after.Status != before.Status || !maps.Equal(after.Metadata, before.Metadata) {
		t.Fatalf("deferred %s was written:\nbefore %v\nafter  %v", name, before.Metadata, after.Metadata)
	}
}

// Kills: no bound on the cumulative time a planner call spends preparing.
func TestExecutePlannedStarts_PrepareBudgetDefersRestOnceSpent(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			e := newPrepareBudgetEnv(t, "a", "b", "c")
			a := e.candidate(t, "a", testPrepareBudget+time.Second)
			b := e.candidate(t, "b", time.Second)
			c := e.candidate(t, "c", time.Second)
			bBefore, cBefore := e.bead(t, b.info.ID), e.bead(t, c.info.ID)

			if woken := e.run(testPrepareBudget, async, nil, a, b, c); woken != 1 {
				t.Fatalf("woken = %d, want 1 (only a is prepared)", woken)
			}
			if n := e.sp.CountCalls("Start", "a"); n != 1 {
				t.Fatalf("a Start calls = %d, want 1", n)
			}
			e.requireDeferred(t, "b", "c")
			e.requireUntouched(t, b, bBefore)
			e.requireUntouched(t, c, cBefore)
			if n := strings.Count(e.log.String(), "prepare budget exhausted"); n != 1 {
				t.Fatalf("summary lines = %d, want exactly one per planner call:\n%s", n, e.log.String())
			}
		})
	}
}

// The slot, tracker entry, lease and capacity ticket of an async start are all
// taken after the gate, so a deferred candidate holds none of them.
func TestExecutePlannedStarts_PrepareBudgetDeferralTakesNoAsyncSlotOrLease(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b")
	a := e.candidate(t, "a", testPrepareBudget)
	b := e.candidate(t, "b", time.Second)
	limiter := newAsyncStartLimiter(1)

	e.run(testPrepareBudget, true, []startExecutionOption{withAsyncStartLimiter(limiter)}, a, b)

	e.requireDeferred(t, "b")
	// a's start released its slot; with b never reserving one the limiter is
	// empty and a one-slot limiter admitted a without ever refusing b for a
	// full slot.
	if strings.Contains(e.log.String(), "session=b template=b outcome=deferred_by_async_start_limit") {
		t.Fatalf("b was refused a slot instead of being deferred by the budget:\n%s", e.log.String())
	}
	limiter.mu.Lock()
	inFlight := limiter.inFlight
	limiter.mu.Unlock()
	if inFlight != 0 {
		t.Fatalf("async slots in flight = %d, want 0", inFlight)
	}
	if rec := e.sessionInfo(b.info.ID); rec.SessionName != "b" {
		t.Fatalf("b row unreadable: %+v", rec)
	}
	if _, err := os.Stat(e.city + "/.gc/runtime-leases"); err == nil {
		entries, _ := os.ReadDir(e.city + "/.gc/runtime-leases")
		for _, ent := range entries {
			if strings.Contains(ent.Name(), "b") && !strings.Contains(ent.Name(), "a") {
				t.Fatalf("b took a runtime lease: %s", ent.Name())
			}
		}
	}
}

// The first candidate of a planner call is always prepared, even when the
// budget is smaller than any single prepare.
func TestExecutePlannedStarts_PrepareBudgetAlwaysAdmitsFirstCandidate(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a")
	a := e.candidate(t, "a", 10*time.Minute)

	if woken := e.run(time.Nanosecond, false, nil, a); woken != 1 {
		t.Fatalf("woken = %d, want 1: the first candidate is always admitted", woken)
	}
	if got := e.deferredByBudget(); len(got) != 0 {
		t.Fatalf("deferred = %v, want none", got)
	}
}

// Kills: a budget that defers work that fit.
func TestExecutePlannedStarts_PrepareBudgetWithinBudgetStartsEverything(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e"}
	e := newPrepareBudgetEnv(t, names...)
	var candidates []startCandidate
	for _, n := range names {
		candidates = append(candidates, e.candidate(t, n, time.Second))
	}

	if woken := e.run(testPrepareBudget, false, nil, candidates...); woken != len(names) {
		t.Fatalf("woken = %d, want %d", woken, len(names))
	}
	if got := e.deferredByBudget(); len(got) != 0 {
		t.Fatalf("deferred = %v, want none within budget", got)
	}
	if strings.Contains(e.log.String(), "prepare budget exhausted") {
		t.Fatalf("summary logged within budget:\n%s", e.log.String())
	}
}

// The spend carries across the dependency-recheck batches of one planner call.
func TestExecutePlannedStarts_PrepareBudgetCarriesAcrossBatches(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f"}
	e := newPrepareBudgetEnv(t, names...)
	var candidates []startCandidate
	for _, n := range names {
		// Three candidates per batch; the first batch spends the budget.
		candidates = append(candidates, e.candidate(t, n, 30*time.Second))
	}

	woken := e.run(testPrepareBudget, false, nil, candidates...)
	if woken < 2 || woken > 3 {
		t.Fatalf("woken = %d, want the candidates prepared before the budget ran out", woken)
	}
	if len(e.deferredByBudget()) != len(names)-woken {
		t.Fatalf("deferred = %v, want the remaining %d", e.deferredByBudget(), len(names)-woken)
	}
	for _, n := range e.deferredByBudget() {
		if n == "a" || n == "b" {
			t.Fatalf("%s was deferred but fit the budget", n)
		}
	}
}

// The spend carries across dependency waves in sync mode: b waits for a to be
// running, a's slow prepare spends the budget, so b's wave defers.
func TestExecutePlannedStarts_PrepareBudgetCarriesAcrossWaves(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b")
	e.cfg.Agents = []config.Agent{{Name: "a"}, {Name: "b", DependsOn: []string{"a"}}}
	a := e.candidate(t, "a", testPrepareBudget+time.Second)
	b := e.candidate(t, "b", time.Second)
	bBefore := e.bead(t, b.info.ID)

	if woken := e.run(testPrepareBudget, false, nil, a, b); woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	e.requireDeferred(t, "b")
	e.requireUntouched(t, b, bBefore)
}

// A prepare that fails still cost its time.
func TestExecutePlannedStarts_PrepareBudgetCountsSlowFailingPrepare(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b")
	a := e.candidate(t, "a", testPrepareBudget+time.Second)
	e.lat.failGet[a.info.ID] = os.ErrDeadlineExceeded
	b := e.candidate(t, "b", time.Second)
	bBefore := e.bead(t, b.info.ID)

	if woken := e.run(testPrepareBudget, false, nil, a, b); woken != 0 {
		t.Fatalf("woken = %d, want 0 (a's prepare failed, b deferred)", woken)
	}
	if !strings.Contains(e.log.String(), "session=a template=a outcome=failed") {
		t.Fatalf("a's failed prepare not logged:\n%s", e.log.String())
	}
	e.requireDeferred(t, "b")
	e.requireUntouched(t, b, bBefore)
}

// A deferral is not a failure: no breaker accounting, named-session state or
// metadata moves for the deferred candidate, while the same candidate within
// budget does write (the contrast that proves the check is not vacuous).
func TestExecutePlannedStarts_PrepareBudgetDeferralWritesNothingButStartDoes(t *testing.T) {
	deferred := newPrepareBudgetEnv(t, "a", "b")
	a := deferred.candidate(t, "a", testPrepareBudget+time.Second)
	b := deferred.candidate(t, "b", time.Second)
	bBefore := deferred.bead(t, b.info.ID)
	inMemory := b.info
	deferred.run(testPrepareBudget, false, nil, a, b)
	deferred.requireUntouched(t, b, bBefore)
	if b.info.State != inMemory.State || b.info.Generation != inMemory.Generation {
		t.Fatalf("in-memory candidate mutated: %+v", b.info)
	}

	within := newPrepareBudgetEnv(t, "a", "b")
	wa := within.candidate(t, "a", time.Second)
	wb := within.candidate(t, "b", time.Second)
	wbBefore := within.bead(t, wb.info.ID)
	within.run(testPrepareBudget, false, nil, wa, wb)
	if after := within.bead(t, wb.info.ID); maps.Equal(after.Metadata, wbBefore.Metadata) {
		t.Fatal("contrast: a start within budget wrote nothing, so the unchanged-row assertion proves nothing")
	}
}

// The gate sits after the on-death and dependency gates: a candidate they
// already defer keeps its outcome when the budget is also spent.
func TestExecutePlannedStarts_PrepareBudgetGateOrder(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "hooked")
	gate := newOnDeathGate()
	gate.enqueue([]deathEdge{{name: "hooked"}})
	a := e.candidate(t, "a", testPrepareBudget+time.Second)
	hooked := e.candidate(t, "hooked", time.Second)

	e.run(testPrepareBudget, false, []startExecutionOption{withOnDeathGate(gate)}, a, hooked)

	if got := e.deferredByBudget(); len(got) != 0 {
		t.Fatalf("budget deferred %v; the on-death gate must keep its outcome", got)
	}
	if !strings.Contains(e.log.String(), "session=hooked template=hooked outcome=deferred_by_on_death_hook") {
		t.Fatalf("on-death deferral lost:\n%s", e.log.String())
	}
}

func TestExecutePlannedStarts_PrepareBudgetKeepsDependencyBlockedOutcome(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "blocked")
	e.cfg.Agents = []config.Agent{{Name: "a"}, {Name: "blocked", DependsOn: []string{"missing-dep"}}, {Name: "missing-dep"}}
	a := e.candidate(t, "a", testPrepareBudget+time.Second)
	blocked := e.candidate(t, "blocked", time.Second)

	e.run(testPrepareBudget, false, nil, a, blocked)

	if got := e.deferredByBudget(); len(got) != 0 {
		t.Fatalf("budget deferred %v; the dependency gate must keep its outcome", got)
	}
	if !strings.Contains(e.log.String(), "session=blocked template=blocked outcome=blocked_on_dependencies") {
		t.Fatalf("dependency block lost:\n%s", e.log.String())
	}
}

// The option is opt-in: omitted, zero or negative leaves prepare unbounded,
// and the meter's edge: spent exactly at the limit defers, a nanosecond under
// does not.
func TestExecutePlannedStarts_PrepareBudgetOptInAndEdges(t *testing.T) {
	cases := []struct {
		name       string
		opts       func(*prepareBudgetEnv) []startExecutionOption
		wantWoken  int
		wantDefers []string
	}{
		{"omitted", func(*prepareBudgetEnv) []startExecutionOption { return nil }, 2, nil},
		{"zero", func(*prepareBudgetEnv) []startExecutionOption { return prepareBudgetOpts(0) }, 2, nil},
		{"negative", func(*prepareBudgetEnv) []startExecutionOption { return prepareBudgetOpts(-time.Second) }, 2, nil},
		{"exactly at the limit defers", func(*prepareBudgetEnv) []startExecutionOption { return prepareBudgetOpts(testPrepareBudget) }, 1, []string{"b"}},
		{"just under the limit admits", func(*prepareBudgetEnv) []startExecutionOption {
			return prepareBudgetOpts(testPrepareBudget + time.Nanosecond)
		}, 2, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPrepareBudgetEnv(t, "a", "b")
			a := e.candidate(t, "a", testPrepareBudget)
			b := e.candidate(t, "b", 0)

			if woken := e.runWith(tc.opts(e), false, a, b); woken != tc.wantWoken {
				t.Fatalf("woken = %d, want %d", woken, tc.wantWoken)
			}
			if got := e.deferredByBudget(); strings.Join(got, ",") != strings.Join(tc.wantDefers, ",") {
				t.Fatalf("deferred = %v, want %v", got, tc.wantDefers)
			}
		})
	}
}

// A clock that moves backwards during prepare charges nothing: the spend
// never goes negative and cannot cancel an earlier charge.
func TestExecutePlannedStarts_PrepareBudgetIgnoresBackwardsClock(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b", "c")
	a := e.candidate(t, "a", testPrepareBudget-time.Second)
	b := e.candidate(t, "b", -time.Hour)
	c := e.candidate(t, "c", 2*time.Second)

	e.run(testPrepareBudget, false, nil, a, b, c)

	// a spent just under the budget; b's rewind must not refund it, so c runs
	// (spent < limit) and nothing is deferred before c; c itself is admitted.
	if got := e.deferredByBudget(); len(got) != 0 {
		t.Fatalf("deferred = %v, want none: spent stayed under the limit", got)
	}
	if n := e.sp.CountCalls("Start", "c"); n != 1 {
		t.Fatalf("c Start calls = %d, want 1", n)
	}
}

// The option is wired at the two controller reconcile sites and not at the
// one-shot `gc start`.
func TestPrepareBudgetWiring(t *testing.T) {
	const wiring = "withPrepareBudget(defaultStartPrepareBudget)"
	rt, err := os.ReadFile("city_runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(rt), wiring); n != 2 {
		t.Fatalf("city_runtime.go wires the prepare budget %d times, want 2 (reconcileStartOptions, controlDispatcherTick)", n)
	}
	start, err := os.ReadFile("cmd_start.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(start), "withPrepareBudget") {
		t.Fatal("cmd_start.go (one-shot gc start) must not opt in to the prepare budget")
	}
}

// The new outcome is documented beside the other start deferrals.
func TestPrepareBudgetOutcomeDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../engdocs/design/dependency-aware-bounded-parallel-lifecycle.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "`"+prepareBudgetOutcome+"`") {
		t.Fatalf("engdoc does not mention `%s`", prepareBudgetOutcome)
	}
}

// The harness charges the clock inside prepare itself, which is what the
// budget tests rely on.
func TestPrepareLatencyStore_ChargesInsidePrepare(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a")
	a := e.candidate(t, "a", 7*time.Second)
	before := e.clk.Now()

	if _, err := prepareStartCandidateForCity(a, e.city, "", e.cfg, e.sp, e.lat, e.clk, &e.log, nil, dispatchOptionSources{}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := e.clk.Now().Sub(before); got != 7*time.Second {
		t.Fatalf("prepare advanced the clock by %s, want exactly 7s", got)
	}
}
