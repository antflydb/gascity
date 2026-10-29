package storebinding

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// WouldClaimBatch is a read-only view of ClaimBatch's eligibility rules. It
// never repairs expired items or acquires a lease while a poller probes.
func (q *beadsNudgeQueue) WouldClaimBatch(target ClaimTarget, now time.Time) (bool, error) {
	entries, corrupt, err := q.keyedEntries(target.QueueKeys, now)
	if err != nil {
		return false, err
	}
	fence := newNudgeClaimFence(target)
	seen := map[string]bool{}
	unpinned := false
	for _, entry := range entries {
		if entry.state != nudgeQueueStateLive || nudgeQueueItemExpired(entry.item, now) || entry.item.DeliverAfter.After(now) || !fence.passes(entry.item) {
			continue
		}
		key := entry.item.DeliveryBatch
		if key == "" {
			unpinned = true
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		_, whole, skipped, e := q.wholeBatch(key, target, fence, now)
		corrupt = append(corrupt, skipped...)
		if e != nil {
			return false, e
		}
		if whole {
			return true, nudgeQueueQuarantined("WouldClaimBatch", corrupt)
		}
	}
	return unpinned, nudgeQueueQuarantined("WouldClaimBatch", corrupt)
}

// RepinClaims changes the delivery key of every named in-flight record in one
// guarded publication. A lost claim or oversized request writes nothing.
func (q *beadsNudgeQueue) RepinClaims(ids []string, batch string) error {
	if len(ids) == 0 {
		return nil
	}
	applier, ok := beads.GuardedBatcherFor(q.store.Store)
	if !ok {
		return beads.ErrGuardedBatchUnsupported
	}
	now := time.Now().UTC()
	seen := map[string]bool{}
	items := []beads.GuardedItem{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		entry, found, err := q.entry(id, now)
		if err != nil {
			return err
		}
		if !found || entry.state != nudgeQueueInFlight {
			return fmt.Errorf("nudge %q is no longer in flight", id)
		}
		revision := entry.bead.Revision
		items = append(items, beads.GuardedItem{Update: &beads.GuardedUpdate{ID: entry.bead.ID, ExpectedRevision: &revision, Opts: beads.UpdateOpts{Metadata: map[string]string{nudgeMetaDeliveryBatch: batch}}}})
	}
	_, err := applier.ApplyGuarded(context.Background(), beads.GuardedBatch{Reason: "repin nudge delivery", Items: items})
	return err
}
