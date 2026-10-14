package main

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// repairWritesPerBuild caps the durable repair writes one desired-state
	// build may attempt, across every repair writer that draws from the same
	// repairWriteBudget. Each bd/Dolt mutation can take seconds, so an
	// unbounded upgrade backlog would otherwise hold the build, and the
	// reconciler tick behind it, for as long as the backlog is long.
	repairWritesPerBuild = 25
	// repairWriteTimeBudget caps the wall time one build spends inside those
	// writes. It is checked before a write starts; a write already in flight
	// finishes.
	repairWriteTimeBudget = 15 * time.Second
)

// repairRotationHints holds, per repair domain and writer site, the list index
// the next build resumes from after a build ran out of budget at that index.
// It is process-lifetime state shared by every build of a city, keyed like
// controlDispatcherRouteRepairCursors.
var repairRotationHints sync.Map // map[string]*atomic.Uint64

// repairWriteBudget (RED stub: pass-through, no caps, no rotation, no failure memo) is the one write budget of a single desired-state build:
// at most repairWritesPerBuild attempts and repairWriteTimeBudget of time spent
// inside writes, drawn on by every repair writer the build runs. A nil budget
// is unbounded, which is what the off-tick callers (the v2 allocator lane and
// the doctor sweep) pass.
type repairWriteBudget struct {
	mu       sync.Mutex
	domain   string
	attempts int
	spent    time.Duration
	deferred int
	stores   map[beads.Store]struct{}
}

// newRepairWriteBudget returns a fresh, unspent budget for one build.
func newRepairWriteBudget() *repairWriteBudget {
	return &repairWriteBudget{stores: map[beads.Store]struct{}{}}
}

// forDomain names the repair domain (the city) whose rotation hints this
// budget's sweeps read and write, and returns the budget for chaining.
func (b *repairWriteBudget) forDomain(domain string) *repairWriteBudget {
	if b != nil {
		b.domain = domain
	}
	return b
}

// write runs fn as one durable repair write if the budget allows it and
// reports whether it ran. Both caps are checked before the write; a refused
// write does not run, is not an error, and is not counted as an attempt. The
// attempt, and the store it targets, are recorded before fn runs, and a failed
// write keeps its attempt. The time charged is the time fn takes. A nil budget
// just runs fn.
func (b *repairWriteBudget) write(store beads.Store, fn func() error) (attempted bool, err error) {
	if b == nil {
		return true, fn()
	}
	if !b.admit(store) {
		return false, nil
	}
	start := time.Now()
	err = fn()
	b.charge(time.Since(start))
	return true, err
}

// admit records one attempt against store and reports true, or records one
// deferred repair and reports false when either cap is already spent.
func (b *repairWriteBudget) admit(store beads.Store) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts++
	b.stores[demandLabelKey(store)] = struct{}{}
	return true
}

// charge adds the time one write took to the budget.
func (b *repairWriteBudget) charge(d time.Duration) {
	b.mu.Lock()
	b.spent += d
	b.mu.Unlock()
}

// spentLocked reports whether either cap is spent. The caller holds b.mu.
func (b *repairWriteBudget) spentLocked() bool {
	return false
}

// attempted reports whether this build has attempted a durable write against
// store, looking through the bead-policy wrapper to the physical store. A nil
// budget reports true for every store: with no record kept, the caller must
// assume the repairs may have written.
func (b *repairWriteBudget) attempted(store beads.Store) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.stores[demandLabelKey(store)]
	return ok
}

// exhausted reports whether either cap is spent, so no further write will run.
func (b *repairWriteBudget) exhausted() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spentLocked()
}

// attemptCount is the number of write attempts this build has made.
func (b *repairWriteBudget) attemptCount() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

// report writes one stderr line when this build declined any repair for want
// of budget, naming the attempts made, the time spent in writes, and the
// repairs deferred. A build that fit its repairs inside the budget, including
// the steady-state build that wrote nothing, prints nothing.
func (b *repairWriteBudget) report(stderr io.Writer) {
	if b == nil || stderr == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deferred == 0 {
		return
	}
	fmt.Fprintf(stderr, "repairWriteBudget: exhausted after %d write attempts and %s in writes; %d repairs deferred to the next build\n", b.attempts, b.spent.Round(time.Millisecond), b.deferred) //nolint:errcheck
}

// rotationHint returns the resume-index cell for one writer site in this
// budget's repair domain.
func (b *repairWriteBudget) rotationHint(site string) *atomic.Uint64 {
	key := controlDispatcherRouteRepairDomainKey(b.domain) + "|" + site
	actual, _ := repairRotationHints.LoadOrStore(key, &atomic.Uint64{})
	return actual.(*atomic.Uint64)
}

// repairSweep is one writer's pass over its list under a build's budget. It
// starts the pass at the index the previous build ran out of budget at, so a
// backlog longer than the budget is worked through over successive builds
// rather than re-trying the same head every time, and records the index at
// which this pass ran out. The stored hint is an index, not a bead identity.
type repairSweep struct {
	budget  *repairWriteBudget
	hint    *atomic.Uint64
	n       int
	start   int
	stopped bool
	stopAt  int
}

// sweep begins a pass over a list of n beads at the named writer site. With a
// nil budget the pass starts at the head and records nothing.
func (b *repairWriteBudget) sweep(site string, n int) *repairSweep {
	s := &repairSweep{budget: b, n: n}
	if b == nil || n == 0 {
		return s
	}
	s.hint = b.rotationHint(site)
	return s
}

// index maps the offset-th step of the pass to a list index, wrapping from the
// resume point back around to the head.
func (s *repairSweep) index(offset int) int {
	if s.n == 0 {
		return offset
	}
	return (s.start + offset) % s.n
}

// write is repairWriteBudget.write for the bead at list index i. The first
// write the budget refuses fixes the index the next build resumes from; the
// pass keeps classifying the remaining beads.
func (s *repairSweep) write(store beads.Store, i int, fn func() error) (attempted bool, err error) {
	attempted, err = s.budget.write(store, fn)
	if !attempted && !s.stopped {
		s.stopped = true
		s.stopAt = i
	}
	return attempted, err
}

// finish stores the resume index when the pass ran out of budget. A pass that
// finished within budget leaves the hint as it was.
func (s *repairSweep) finish() {
	if s.stopped && s.hint != nil {
		s.hint.Store(uint64(s.stopAt))
	}
}

// rootGetKey names a workflow root as one build reads it: the physical store
// and the root's bead id.
type rootGetKey struct {
	store  beads.Store
	rootID string
}

// rootGetEntry is one remembered root read, success or failure, and whether
// this build has already stamped the root.
type rootGetEntry struct {
	root    beads.Bead
	err     error
	stamped bool
}

// rootGetMemo remembers, for one desired-state build, what reading each
// workflow root returned, so a run of in-progress steps sharing a root reads it
// once. A failed read is remembered too, for the rest of this build only: the
// next build makes a fresh memo and tries again.
type rootGetMemo struct {
	entries map[rootGetKey]*rootGetEntry
}

// newRootGetMemo returns an empty memo for one build.
func newRootGetMemo() *rootGetMemo {
	return &rootGetMemo{entries: map[rootGetKey]*rootGetEntry{}}
}

// resolve returns the memoized read of rootID in store, reading it on first
// use.
func (m *rootGetMemo) resolve(store beads.Store, rootID string) *rootGetEntry {
	key := rootGetKey{store: demandLabelKey(store), rootID: rootID}
	if entry, ok := m.entries[key]; ok {
		return entry
	}
	root, err := store.Get(rootID)
	entry := &rootGetEntry{root: root, err: err}
	if err == nil {
		m.entries[key] = entry
	}
	return entry
}

// runRootStamp is the session identity one step stamps onto its workflow root.
type runRootStamp struct {
	sessionName         string
	workDir             string
	allowUnownedWorkDir bool
}

// rootMemoOrNew returns roots, or a memo for the calling pass alone when the
// caller keeps none.
func rootMemoOrNew(roots *rootGetMemo) *rootGetMemo {
	if roots == nil {
		return newRootGetMemo()
	}
	return roots
}
