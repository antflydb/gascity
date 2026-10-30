package maintenance

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestTypedReaperProtectsYoungDescendantsJournalsAndReferences(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	rows := []beads.Bead{
		{ID: "closed-owner", Status: "closed", CreatedAt: old, UpdatedAt: old, Ephemeral: true},
		{ID: "orphan", Status: "open", CreatedAt: old, Ephemeral: true, ParentID: "closed-owner"},
		{ID: "young", Status: "open", CreatedAt: now, Ephemeral: true, ParentID: "orphan"},
		{ID: "leaf", Status: "open", CreatedAt: old, Ephemeral: true, ParentID: "closed-owner"},
		{ID: "old-unreferenced", Status: "closed", CreatedAt: old, UpdatedAt: old, Ephemeral: true},
		{ID: "op", Type: "op", Status: "closed", CreatedAt: old, NoHistory: true},
		{ID: "extmsg", Status: "closed", CreatedAt: old, NoHistory: true, Labels: []string{"gc:extmsg-binding"}},
		{ID: "foreign-root", Type: "molecule", Status: "open", CreatedAt: old, Metadata: map[string]string{"gc.root_store_ref": "elsewhere"}},
		{ID: "weak-owner-only", Status: "open", CreatedAt: old, Ephemeral: true, ParentID: "closed-owner"},
	}
	deps := []beads.Dep{{IssueID: "orphan", DependsOnID: "closed-owner", Type: "parent-child"}, {IssueID: "young", DependsOnID: "orphan", Type: "parent-child"}, {IssueID: "leaf", DependsOnID: "closed-owner", Type: "parent-child"}}
	store := beads.NewMemStoreFrom(0, rows, deps)
	report, e := Reap(store, Policy{Now: now, StaleBefore: now.Add(-24 * time.Hour), ClosedBefore: now.Add(-7 * 24 * time.Hour), Limit: 100})
	if e != nil {
		t.Fatal(e)
	}
	if len(report.Closed) != 1 || report.Closed[0] != "leaf" {
		t.Fatalf("closed=%v", report.Closed)
	}
	if len(report.Deleted) != 1 || report.Deleted[0] != "old-unreferenced" {
		t.Fatalf("deleted=%v", report.Deleted)
	}
	for _, id := range []string{"orphan", "young", "op", "extmsg", "closed-owner", "foreign-root", "weak-owner-only"} {
		if _, e = store.Get(id); e != nil {
			t.Fatalf("lost protected %s: %v", id, e)
		}
	}
}

func TestTypedReaperClosesOwnershipSubtreeLeafFirst(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "root", Status: "closed", CreatedAt: old}, {ID: "mid", Status: "open", CreatedAt: old, Ephemeral: true, ParentID: "root"}, {ID: "leaf", Status: "open", CreatedAt: old, Ephemeral: true, ParentID: "mid"}}, []beads.Dep{{IssueID: "mid", DependsOnID: "root", Type: "parent-child"}, {IssueID: "leaf", DependsOnID: "mid", Type: "parent-child"}})
	report, e := Reap(store, Policy{Now: now, StaleBefore: now.Add(-24 * time.Hour), Limit: 100})
	if e != nil {
		t.Fatal(e)
	}
	if len(report.Closed) != 2 || report.Closed[0] != "leaf" || report.Closed[1] != "mid" {
		t.Fatalf("close order=%v", report.Closed)
	}
}

func TestTypedReaperDeleteBudgetDoesNotStrandDependency(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-30 * 24 * time.Hour)
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "a-target", Status: "closed", CreatedAt: old, Ephemeral: true}, {ID: "z-source", Status: "closed", CreatedAt: old, Ephemeral: true}}, []beads.Dep{{IssueID: "z-source", DependsOnID: "a-target", Type: "blocks"}})
	report, e := Reap(store, Policy{Now: now, ClosedBefore: now.Add(-7 * 24 * time.Hour), Limit: 1})
	if e != nil {
		t.Fatal(e)
	}
	if len(report.Deleted) != 1 || report.Deleted[0] != "z-source" {
		t.Fatalf("budget did not retire the source first: %+v", report)
	}
	if _, e = store.Get("a-target"); e != nil {
		t.Fatal("deleted a still-protected target")
	}
}

func TestTypedCompactionPromotesRetainedAndPreservesQueue(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "open", Status: "in_progress", Assignee: "worker", CreatedAt: old, Ephemeral: true}, {ID: "kept", Status: "closed", CreatedAt: old, Ephemeral: true, Labels: []string{"keep"}}, {ID: "expired", Status: "closed", CreatedAt: old, Ephemeral: true}, {ID: "queue", Status: "closed", CreatedAt: old, Ephemeral: true, Labels: []string{"gc:nudge-queue"}}}, nil)
	report, e := Compact(store, now, 100)
	if e != nil {
		t.Fatal(e)
	}
	if len(report.Promoted) != 2 || len(report.Deleted) != 1 {
		t.Fatalf("compaction=%+v", report)
	}
	b, e := store.Get("open")
	if e != nil || b.Ephemeral || b.Assignee != "worker" || b.Status != "in_progress" {
		t.Fatalf("promotion changed work: %+v %v", b, e)
	}
	if _, e = store.Get("queue"); e != nil {
		t.Fatal(e)
	}
}

func TestTypedReaperDoesNotAuthorizeForeignOrWeakDescendants(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	rows := []beads.Bead{
		{ID: "owner", Status: "closed", CreatedAt: old},
		{ID: "orphan", Status: "open", CreatedAt: old, Ephemeral: true, ParentID: "owner"},
		{ID: "weak", Status: "open", CreatedAt: old, Ephemeral: true, ParentID: "orphan"},
		{ID: "foreign", Status: "open", CreatedAt: old, Ephemeral: true, ParentID: "orphan", Metadata: map[string]string{"gc.root_store_ref": "foreign"}},
		{ID: "blocked", Status: "blocked", CreatedAt: old, Ephemeral: true, ParentID: "orphan"},
	}
	deps := []beads.Dep{{IssueID: "orphan", DependsOnID: "owner", Type: "parent-child"}, {IssueID: "foreign", DependsOnID: "orphan", Type: "parent-child"}, {IssueID: "blocked", DependsOnID: "orphan", Type: "parent-child"}}
	store := beads.NewMemStoreFrom(0, rows, deps)
	result, err := Reap(store, Policy{Now: now, StaleBefore: now.Add(-24 * time.Hour), Limit: 100})
	if err != nil || len(result.Closed) != 0 {
		t.Fatalf("protected descendant was closed: %+v %v", result, err)
	}
}

func TestTypedRetentionPreservesWeakReferenceTargets(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-30 * 24 * time.Hour)
	for _, compact := range []bool{false, true} {
		store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "target", Status: "closed", Ephemeral: true, CreatedAt: old, UpdatedAt: old}, {ID: "survivor", Status: "open", ParentID: "target", CreatedAt: now}}, nil)
		var result Result
		var err error
		if compact {
			result, err = Compact(store, now, 100)
		} else {
			result, err = Reap(store, Policy{Now: now, ClosedBefore: now.Add(-7 * 24 * time.Hour), Limit: 100})
		}
		if err != nil || len(result.Deleted) != 0 {
			t.Fatalf("compact=%v deleted weak target: %+v %v", compact, result, err)
		}
	}
}
