package main

import (
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// nudgeQueueHandle is the city's nudge queue as one invocation sees it.
type nudgeQueueHandle struct {
	// Queue is the queue's contract surface.
	Queue storebinding.NudgeQueue
	// Location tells an operator where a queued item lives.
	Location string
	// Skips persists the dispatch tick's skip counters, an operator
	// diagnostic outside the queue contract.
	Skips nudgeDispatchSkipRecorder
	// Repin moves in-flight claims onto a new delivery key without a
	// maintenance pass or a store open (rekeyQueuedNudgesAfterFailedBookkeeping).
	Repin nudgeClaimRepinner
	// Probe answers whether a claim would claim anything, from a lock-free
	// snapshot (the poll loop's due-work gate, nudgePollGate).
	Probe nudgeClaimProber
}

// nudgeClaimProber reports, without the queue lock and without claiming,
// whether ClaimBatch for ct at now would claim anything.
type nudgeClaimProber interface {
	WouldClaimBatch(ct storebinding.ClaimTarget, now time.Time) (bool, error)
}

// nudgeDispatchSkipRecorder persists and reads the dispatch skip counters.
type nudgeDispatchSkipRecorder interface {
	RecordDispatchSkips(map[string]int64) error
	DispatchSkips() (map[string]int64, error)
}

// nudgeClaimRepinner pins in-flight claims to a delivery key, all or nothing.
type nudgeClaimRepinner interface {
	RepinClaims(ids []string, batch string) error
}

// openNudgeQueue is the only constructor of the city's nudge queue in cmd/gc.
// store is the nudges-class store the caller already holds. With the zero
// value, Enqueue opens its own to save the shadow, the verbs whose commit owes
// shadow debts open their own after the queue lock to pay them
// (settleNudgeShadowDebts), and so does the shim's import (importMigrated),
// which saves the moved items' shadows. WithdrawQueuedWaitNudges does not, and
// reaches shadows only through a caller-passed store: with the zero value it
// retires the queue item and leaves its shadow open, as does Rollback of an
// item no longer queued.
//
// Engines opting into BeadsNudgeQueueMode use the native bead queue and its
// state-aware shadow view. The existing shim moves eligible legacy pending
// batches into it and reinstates retired migrated records. Other engines keep
// their deployed file queue. A general reversible config-mode rollout remains
// separate; the native binding cannot be silently switched back to a file.
func openNudgeQueue(cityPath string, store beads.NudgesStore) nudgeQueueHandle {
	if handle, ok := openNativeNudgeQueue(cityPath, store); ok {
		return handle
	}
	q := &fileNudgeQueue{cityPath: cityPath, store: store, clk: clock.Real{}}
	return nudgeQueueHandle{
		Queue:    q,
		Location: q.location(),
		Skips:    q,
		Repin:    q,
		Probe:    q,
	}
}

// nudgeShadows is the shadow projection over store. It takes no city path, so
// the wait helpers that hold only a store reach it.
func nudgeShadows(store beads.NudgesStore) storebinding.NudgeShadows {
	if nativeBeadsNudgeMode(store.Store) {
		front, err := nativeNudgeFrontDoors(store)
		if err == nil {
			return front.Shadows
		}
		return &failedNativeNudgeShadows{err: err}
	}
	return nudgeFrontDoor(store)
}

// claimTarget is the contract's view of t: its queue keys, its fence, and a
// census of cityPath's sessions, taken at most once, that lets it adopt a
// superseded session's nudges. A target that carries a census of its own
// (fenceCensus, the dispatcher's skip-only snapshot census) answers with it
// instead.
func (t nudgeTarget) claimTarget(cityPath string) storebinding.ClaimTarget {
	if t.fenceCensus != nil {
		return storebinding.ClaimTarget{
			QueueKeys:         t.queueKeys(),
			SessionID:         t.sessionID,
			ContinuationEpoch: t.continuationEpoch,
			LiveFenceSessions: t.fenceCensus,
		}
	}
	var once sync.Once
	var ids map[string]struct{}
	return storebinding.ClaimTarget{
		QueueKeys:         t.queueKeys(),
		SessionID:         t.sessionID,
		ContinuationEpoch: t.continuationEpoch,
		LiveFenceSessions: func() (map[string]struct{}, bool) {
			once.Do(func() { ids = liveNudgeFenceSessionIDsForCity(cityPath) })
			return ids, ids != nil
		},
	}
}
