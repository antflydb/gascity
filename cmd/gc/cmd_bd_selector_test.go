package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdRoleSelectorMerge(t *testing.T) {
	q, served, unassigned, err := parseBdRoleSelector([]string{"list", "--metadata-field", "gc.routed_to=alpha/operator", "--all", "--json", "--no-assignee", "--limit", "1"})
	if err != nil || !served || !unassigned {
		t.Fatalf("parse: %v %v %v", served, unassigned, err)
	}
	matching := func(id string) beads.Bead {
		return beads.Bead{ID: id, Status: "open", Metadata: map[string]string{"gc.routed_to": "alpha/operator", "gc.root_store_ref": "rig:alpha"}}
	}
	stale := matching("stale")
	current := stale
	current.Metadata = map[string]string{"gc.routed_to": "other", "gc.root_store_ref": "rig:alpha"}
	closed := matching("closed")
	closed.Status = "closed"
	assigned := matching("assigned")
	assigned.Assignee = "worker"
	foreign := matching("foreign")
	foreign.Metadata["gc.root_store_ref"] = "rig:beta"
	got := mergeBdRoleSelector([]beads.Bead{stale}, []beads.Bead{current, closed, assigned, foreign}, q, unassigned, "alpha")
	if len(got) != 1 || got[0].ID != "closed" {
		t.Fatalf("unexpected projection: %+v", got)
	}
}

func TestBdRoleSelectorRejectsDroppedFlags(t *testing.T) {
	for _, extra := range [][]string{{"--ready"}, {"--sort", "priority"}, {"--metadata-field", "bad"}, {"--limit", "-1"}, {"--assignee", "worker", "--no-assignee"}} {
		args := append([]string{"list", "--metadata-field=gc.run_target=operator", "--json"}, extra...)
		if _, served, _, err := parseBdRoleSelector(args); !served || err == nil {
			t.Fatalf("accepted %v: %v", args, err)
		}
	}
	if _, served, _, _ := parseBdRoleSelector([]string{"list", "--json"}); served {
		t.Fatal("intercepted unrelated query")
	}
}
