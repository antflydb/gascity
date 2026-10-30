// Package maintenance implements typed retention operations for a caller-owned
// atomic snapshot. Policy is supplied by the pack; selection executes no SQL.
package maintenance

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Policy contains the pack's time cutoffs and bounded write budget.
type Policy struct {
	Now          time.Time
	StaleBefore  time.Time
	ClosedBefore time.Time
	StoreRef     string
	Limit        int
}

// Result identifies committed changes, or the same simulated changes in a view.
type Result struct {
	Scanned  int      `json:"scanned"`
	Closed   []string `json:"closed"`
	Deleted  []string `json:"deleted"`
	Promoted []string `json:"promoted"`
	Held     int      `json:"held"`
}

func protected(b beads.Bead) bool {
	for _, label := range b.Labels {
		if strings.HasPrefix(label, "gc:extmsg-") {
			return true
		}
	}
	return slices.Contains([]string{"op", "session", "message", "nudge"}, b.Type) || slices.Contains(b.Labels, "gc:nudge") || slices.Contains(b.Labels, "gc:nudge-queue") || slices.Contains(b.Labels, "gc:session")
}

func timestamp(b beads.Bead) time.Time {
	if !b.UpdatedAt.IsZero() {
		return b.UpdatedAt
	}
	return b.CreatedAt
}

func before(value, cutoff time.Time) bool {
	return !value.IsZero() && !cutoff.IsZero() && value.Before(cutoff)
}

// Reap closes old ownership orphans leaf-first and purges old closed ephemeral
// records without leaving dangling local edges. Call inside a transaction/view;
// its reads and writes must share a snapshot. Foreign workflow roots, durable
// domain records, queue delivery state, op journals and sessions remain owned
// by their respective lifecycle services.
func Reap(store beads.Store, policy Policy) (Result, error) {
	result := Result{Closed: []string{}, Deleted: []string{}, Promoted: []string{}}
	if policy.Now.IsZero() || policy.Limit <= 0 {
		return result, fmt.Errorf("retention requires a clock and positive write limit")
	}
	if !policy.ClosedBefore.IsZero() && policy.Now.Sub(policy.ClosedBefore) < 7*24*time.Hour {
		return result, fmt.Errorf("closed retention must be at least seven days")
	}
	rows, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		return result, err
	}
	result.Scanned = len(rows)
	byID := map[string]beads.Bead{}
	children := map[string][]string{}
	deps := map[string][]beads.Dep{}
	for _, b := range rows {
		byID[b.ID] = b
		if root := b.Metadata["gc.root_bead_id"]; root != "" && root != b.ID && root != b.ParentID {
			children[root] = append(children[root], b.ID)
		}
		if b.ParentID != "" {
			children[b.ParentID] = append(children[b.ParentID], b.ID)
		}
		edges, e := store.DepList(b.ID, "down")
		if e != nil {
			return result, e
		}
		deps[b.ID] = edges
	}
	candidates := map[string]bool{}
	eligible := func(b beads.Bead) bool {
		return !protected(b) && before(b.CreatedAt, policy.StaleBefore) &&
			slices.Contains([]string{"open", "hooked", "in_progress"}, b.Status) &&
			(b.Metadata["gc.root_store_ref"] == "" || b.Metadata["gc.root_store_ref"] == policy.StoreRef)
	}
	ownership := func(b beads.Bead, d beads.Dep) bool {
		return d.Type == "parent-child" || d.Type == "tracks" && d.DependsOnID == b.Metadata["gc.root_bead_id"]
	}
	for _, b := range rows {
		if !eligible(b) {
			continue
		}
		for _, d := range deps[b.ID] {
			if !ownership(b, d) {
				continue
			}
			owner, exists := byID[d.DependsOnID]
			if b.Ephemeral && exists && owner.Status == "closed" && (owner.Metadata["gc.root_store_ref"] == "" || owner.Metadata["gc.root_store_ref"] == policy.StoreRef) {
				candidates[b.ID] = true
			}
		}
		// Root-only or fully drained workflows are safe to close, but never when
		// their store reference points elsewhere or their assignee is active.
		root := b.Type == "molecule" || b.Metadata["gc.kind"] == "workflow"
		if root && b.Assignee == "" && (b.Metadata["gc.root_store_ref"] == "" || b.Metadata["gc.root_store_ref"] == policy.StoreRef) {
			candidates[b.ID] = true
		}
	}
	// Add old ephemeral ownership descendants only beneath a selected orphan.
	for changed := true; changed; {
		changed = false
		for _, b := range rows {
			if candidates[b.ID] || !b.Ephemeral || !eligible(b) {
				continue
			}
			for _, d := range deps[b.ID] {
				if ownership(b, d) && candidates[d.DependsOnID] {
					candidates[b.ID] = true
					changed = true
				}
			}
		}
	}
	keys := make([]string, 0, len(candidates))
	for id := range candidates {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for progress := true; progress && len(result.Closed) < policy.Limit; {
		progress = false
		for _, id := range keys {
			b := byID[id]
			if b.Status == "closed" {
				continue
			}
			safe := true
			for _, child := range children[id] {
				if byID[child].Status != "closed" {
					safe = false
				}
			}
			for _, d := range deps[id] {
				if d.Type == "blocks" {
					target, exists := byID[d.DependsOnID]
					if !exists || target.Status != "closed" {
						safe = false
					}
				}
			}
			if !safe {
				continue
			}
			if err = store.SetMetadata(id, "close_reason", "retention: stale ownership or drained workflow"); err != nil {
				return result, err
			}
			if err = store.Close(id); err != nil {
				return result, err
			}
			b.Status = "closed"
			b.UpdatedAt = policy.Now
			byID[id] = b
			result.Closed = append(result.Closed, id)
			progress = true
			if len(result.Closed) >= policy.Limit {
				break
			}
		}
	}
	result.Held = len(candidates) - len(result.Closed)
	doomed := map[string]bool{}
	for _, b := range rows {
		if ref := b.Metadata["gc.root_store_ref"]; ref != "" && ref != policy.StoreRef {
			continue
		}
		if !protected(b) && b.Status == "closed" && (b.Ephemeral || b.NoHistory) && before(timestamp(b), policy.ClosedBefore) {
			doomed[b.ID] = true
		}
	}
	for _, b := range rows {
		for _, target := range []string{b.ParentID, b.Metadata["gc.root_bead_id"]} {
			if target != "" && target != b.ID {
				deps[b.ID] = append(deps[b.ID], beads.Dep{IssueID: b.ID, DependsOnID: target, Type: "retention-reference"})
			}
		}
	}
	// A younger/durable neighbor protects its target. Repeat to preserve the
	// whole incoming dependency chain when a candidate is held back.
	for changed := true; changed; {
		changed = false
		for _, edges := range deps {
			for _, d := range edges {
				if doomed[d.DependsOnID] && !doomed[d.IssueID] {
					delete(doomed, d.DependsOnID)
					changed = true
				}
			}
		}
	}
	ids := make([]string, 0, len(doomed))
	for id := range doomed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	remaining := policy.Limit - len(result.Closed)
	selected := map[string]bool{}
	if len(ids) <= remaining {
		for _, id := range ids {
			selected[id] = true
		}
	} else {
		// Retire sources before targets so a bounded run makes progress while
		// preserving every dependency whose source will survive this commit.
		for changed := true; changed && len(selected) < remaining; {
			changed = false
			for _, id := range ids {
				if selected[id] {
					continue
				}
				safe := true
				for _, edges := range deps {
					for _, d := range edges {
						if d.DependsOnID == id && !selected[d.IssueID] {
							safe = false
						}
					}
				}
				if safe {
					selected[id] = true
					changed = true
					if len(selected) >= remaining {
						break
					}
				}
			}
		}
	}
	for _, id := range ids {
		if !selected[id] {
			continue
		}
		for _, edges := range deps {
			for _, d := range edges {
				if d.Type != "retention-reference" && (d.IssueID == id || d.DependsOnID == id) {
					if err = store.DepRemove(d.IssueID, d.DependsOnID); err != nil {
						return result, err
					}
				}
			}
		}
		if err = store.Delete(id); err != nil {
			return result, err
		}
		result.Deleted = append(result.Deleted, id)
	}
	return result, nil
}

// Compact applies the pack's ephemeral TTL policy. Closed old records without
// a keep marker are removed only when no local row references them. Open or
// explicitly retained records are promoted without changing their assignment.
func Compact(store beads.Store, now time.Time, limit int) (Result, error) {
	result := Result{Closed: []string{}, Deleted: []string{}, Promoted: []string{}}
	if now.IsZero() || limit <= 0 {
		return result, fmt.Errorf("compaction requires a clock and positive write limit")
	}
	rows, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		return result, err
	}
	result.Scanned = len(rows)
	incoming := map[string]bool{}
	for _, b := range rows {
		for _, target := range []string{b.ParentID, b.Metadata["gc.root_bead_id"]} {
			if target != "" && target != b.ID {
				incoming[target] = true
			}
		}
		deps, e := store.DepList(b.ID, "down")
		if e != nil {
			return result, e
		}
		for _, d := range deps {
			incoming[d.DependsOnID] = true
		}
	}
	for _, b := range rows {
		if len(result.Deleted)+len(result.Promoted) >= limit {
			break
		}
		if !b.Ephemeral || protected(b) {
			continue
		}
		ttl := 24 * time.Hour
		for _, label := range b.Labels {
			switch label {
			case "wisp_type:heartbeat", "wisp_type:ping":
				ttl = 6 * time.Hour
			case "wisp_type:recovery", "wisp_type:error", "wisp_type:escalation":
				ttl = 7 * 24 * time.Hour
			}
		}
		keep := slices.Contains(b.Labels, "keep")
		if !keep && !before(timestamp(b), now.Add(-ttl)) {
			continue
		}
		switch {
		case keep || b.Status != "closed" || b.Metadata["comment_count"] != "" && b.Metadata["comment_count"] != "0":
			setter, ok := store.(interface {
				SetStorage(string, beads.StorageClass) error
			})
			if !ok {
				return result, fmt.Errorf("store does not support retention tier changes")
			}
			if err = setter.SetStorage(b.ID, beads.StorageHistory); err != nil {
				return result, err
			}
			result.Promoted = append(result.Promoted, b.ID)

		case !incoming[b.ID]:
			deps, e := store.DepList(b.ID, "down")
			if e != nil {
				return result, e
			}
			for _, d := range deps {
				if err = store.DepRemove(d.IssueID, d.DependsOnID); err != nil {
					return result, err
				}
			}
			if err = store.Delete(b.ID); err != nil {
				return result, err
			}
			result.Deleted = append(result.Deleted, b.ID)

		default:
			result.Held++
		}
	}
	return result, nil
}
