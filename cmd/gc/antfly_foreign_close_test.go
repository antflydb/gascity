package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestAntflyForeignCloseUsesLiveOwnerAndFailsClosed(t *testing.T) {
	source := beads.Bead{ID: "aa-1", Status: "open", Dependencies: []beads.Dep{{IssueID: "aa-1", DependsOnID: "external:bb-2", Type: "blocks"}}}
	rows := map[string]beads.Bead{"aa-1": source, "bb-2": {ID: "bb-2", Status: "open"}}
	read := func(id string) (beads.Bead, error) {
		if b, ok := rows[id]; ok {
			return b, nil
		}
		return beads.Bead{}, beads.ErrNotFound
	}
	if err := checkAntflyForeignClose([]string{"aa-1"}, read); err == nil {
		t.Fatal("foreign open blocker bypassed")
	}
	rows["bb-2"] = beads.Bead{ID: "bb-2", Status: "closed"}
	if err := checkAntflyForeignClose([]string{"aa-1"}, read); err != nil {
		t.Fatal(err)
	}
	delete(rows, "bb-2")
	if err := checkAntflyForeignClose([]string{"aa-1"}, read); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("missing foreign owner ignored: %v", err)
	}
	source.Dependencies[0].DependsOnID = "external:rig:capability"
	rows["aa-1"] = source
	if err := checkAntflyForeignClose([]string{"aa-1"}, read); err != nil {
		t.Fatalf("capability reference blocked: %v", err)
	}
	source.Status = "closed"
	rows["aa-1"] = source
	if err := checkAntflyForeignClose([]string{"aa-1"}, read); err != nil {
		t.Fatalf("idempotent close refused: %v", err)
	}
}

func TestAntflyForeignCloseForceParsing(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"close", "aa-1", "--force"}, true},
		{[]string{"close", "aa-1", "-f"}, true},
		{[]string{"close", "aa-1", "--force=false"}, false},
		{[]string{"close", "aa-1", "--reason", "--force"}, false},
		{[]string{"close", "aa-1", "--", "--force"}, false},
		{[]string{"close", "aa-1", "--force", "--force=false"}, false},
	} {
		if got := antflyCloseForced(tc.args); got != tc.want {
			t.Errorf("%v: %v want %v", tc.args, got, tc.want)
		}
	}
}
