package main

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// Native queue selection is an explicit engine capability, never inferred from
// a file's absence. Existing engines retain their deployed queue behavior.
func nativeBeadsNudgeMode(store beads.Store) bool {
	for depth := 0; store != nil && depth < 16; depth++ {
		if mode, ok := store.(interface{ BeadsNudgeQueueMode() bool }); ok {
			return mode.BeadsNudgeQueueMode()
		}
		if backing, ok := store.(interface{ Backing() beads.Store }); ok {
			store = backing.Backing()
			continue
		}
		if inner, _, ok := unwrapBeadPolicyStore(store); ok {
			store = inner
			continue
		}
		if emitting, ok := store.(*emittingClassStore); ok {
			store = emitting.Store
			continue
		}
		return false
	}
	return false
}

func nativeNudgeFrontDoors(store beads.NudgesStore) (storebinding.NudgeFrontDoors, error) {
	return storebinding.NewBeadsNudgeFrontDoors(store)
}

func openNativeNudgeQueue(cityPath string, store beads.NudgesStore) (nudgeQueueHandle, bool) {
	if store.Store == nil {
		if routed, ok := cliStorageRoutes(cityPath).storeFor(coordclass.ClassNudges); ok {
			store.Store = routed
		}
	}
	if refused, ok := storeEngine(store.Store).(refusedClassStore); ok {
		failed := &failedNativeNudgeQueue{err: refused.err}
		return nudgeQueueHandle{Queue: failed, Location: "refused:nudges", Skips: failed, Repin: failed, Probe: failed}, true
	}
	if !nativeBeadsNudgeMode(store.Store) {
		return nudgeQueueHandle{}, false
	}
	legacy := &fileNudgeQueue{cityPath: cityPath, store: store, clk: clock.Real{}}
	front, err := nativeNudgeFrontDoors(store)
	if err == nil {
		// Reuse the existing migration protocol: pending legacy batches move as
		// whole units; existing in-flight work remains on its old authority.
		_, err = shimNudgeQueue(context.Background(), nudgeQueueShimToBeads, legacy, front.Queue.(nudgeQueueShimBeads), time.Now().UTC())
	}
	if err != nil {
		failed := &failedNativeNudgeQueue{err: fmt.Errorf("opening native nudge queue: %w", err)}
		return nudgeQueueHandle{Queue: failed, Location: "beads:nudges", Skips: legacy, Repin: failed, Probe: failed}, true
	}
	return nudgeQueueHandle{Queue: front.Queue, Location: "beads:nudges", Skips: legacy, Repin: front.Queue.(nudgeClaimRepinner), Probe: front.Queue.(nudgeClaimProber)}, true
}

// A failed native selection must not redirect delivery state into a file.
type failedNativeNudgeQueue struct{ err error }

func (q *failedNativeNudgeQueue) Enqueue(nudgequeue.Item) error         { return q.err }
func (q *failedNativeNudgeQueue) EnqueueDeferred(nudgequeue.Item) error { return q.err }
func (q *failedNativeNudgeQueue) ClaimDue(storebinding.ClaimTarget, time.Time) ([]nudgequeue.Item, error) {
	return nil, q.err
}

func (q *failedNativeNudgeQueue) ClaimBatch(storebinding.ClaimTarget, time.Time) ([]nudgequeue.Item, error) {
	return nil, q.err
}

func (q *failedNativeNudgeQueue) ListForAgent(string, time.Time) ([]nudgequeue.Item, []nudgequeue.Item, []nudgequeue.Item, error) {
	return nil, nil, nil, q.err
}

func (q *failedNativeNudgeQueue) ListFor(storebinding.ClaimTarget, time.Time) ([]nudgequeue.Item, []nudgequeue.Item, []nudgequeue.Item, error) {
	return nil, nil, nil, q.err
}

func (q *failedNativeNudgeQueue) Lookup([]string, time.Time) (map[string]nudgequeue.Bucket, error) {
	return nil, q.err
}
func (q *failedNativeNudgeQueue) DueKeys(time.Time) ([]string, error) { return nil, q.err }
func (q *failedNativeNudgeQueue) Empty() (bool, error)                { return false, q.err }
func (q *failedNativeNudgeQueue) Snapshot() (nudgequeue.State, error) {
	return nudgequeue.State{}, q.err
}
func (q *failedNativeNudgeQueue) Ack([]string, string, string, string) error { return q.err }
func (q *failedNativeNudgeQueue) ReleaseClaims([]string, time.Time) error    { return q.err }
func (q *failedNativeNudgeQueue) RecordFailure([]string, error, time.Time) ([]nudgequeue.Item, error) {
	return nil, q.err
}
func (q *failedNativeNudgeQueue) Rollback(nudgequeue.Item, string) error  { return q.err }
func (q *failedNativeNudgeQueue) WithdrawQueuedWaitNudges([]string) error { return q.err }
func (q *failedNativeNudgeQueue) Maintain(context.Context, time.Time) (storebinding.NudgeMaintenance, error) {
	return storebinding.NudgeMaintenance{}, q.err
}
func (q *failedNativeNudgeQueue) RepinClaims([]string, string) error { return q.err }
func (q *failedNativeNudgeQueue) WouldClaimBatch(storebinding.ClaimTarget, time.Time) (bool, error) {
	return false, q.err
}

func (q *failedNativeNudgeQueue) RecordDispatchSkips(map[string]int64) error { return q.err }
func (q *failedNativeNudgeQueue) DispatchSkips() (map[string]int64, error)   { return nil, q.err }

type failedNativeNudgeShadows struct{ err error }

func (q *failedNativeNudgeShadows) Save(nudgequeue.Item) (string, bool, error) {
	return "", false, q.err
}

func (q *failedNativeNudgeShadows) Terminalize(nudgequeue.Item, string, string, string, time.Time) error {
	return q.err
}
func (q *failedNativeNudgeShadows) RollbackEnqueue(string) error               { return q.err }
func (q *failedNativeNudgeShadows) SweepStale(string, string, time.Time) error { return q.err }
func (q *failedNativeNudgeShadows) StaleShadowsBefore(time.Time, int, map[string]bool) ([]nudgequeue.NudgeShadow, error) {
	return nil, q.err
}

func (q *failedNativeNudgeShadows) ShadowHistorySince(time.Time) ([]nudgequeue.NudgeShadow, error) {
	return nil, q.err
}

func (q *failedNativeNudgeShadows) Find(string) (nudgequeue.NudgeShadow, bool, error) {
	return nudgequeue.NudgeShadow{}, false, q.err
}

func (q *failedNativeNudgeShadows) FindIncludingTerminal(string) (nudgequeue.NudgeShadow, bool, error) {
	return nudgequeue.NudgeShadow{}, false, q.err
}
