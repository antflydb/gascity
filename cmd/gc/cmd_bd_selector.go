package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Stock role recovery selectors address coordination metadata. On split storage
// they must consult the class binding as well as the selected work scope.
func parseBdRoleSelector(args []string) (beads.ListQuery, bool, bool, error) {
	q := beads.ListQuery{Metadata: map[string]string{}, TierMode: beads.FederatedReadTier, Sort: beads.SortCreatedAsc, Live: true, Limit: 50}
	if len(args) == 0 || args[0] != "list" {
		return q, false, false, nil
	}
	serves := false
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "gc.") && strings.Contains(arg, "=") || strings.HasPrefix(arg, "--metadata-field=gc.") {
			serves = true
		}
	}
	if !serves {
		return q, false, false, nil
	}
	jsonOutput, unassigned := false, false
	for i := 1; i < len(args); i++ {
		flag, value, inline := strings.Cut(args[i], "=")
		switch flag {
		case "--json":
			if inline {
				return q, true, false, fmt.Errorf("boolean flag values are unsupported: %s", args[i])
			}
			jsonOutput = true
		case "--all", "-a":
			if inline {
				return q, true, false, fmt.Errorf("boolean flag values are unsupported: %s", args[i])
			}
			q.IncludeClosed = true
		case "--no-assignee":
			if inline {
				return q, true, false, fmt.Errorf("boolean flag values are unsupported: %s", args[i])
			}
			unassigned = true
		case "--metadata-field", "--status", "--type", "--assignee", "--limit", "-n":
			if !inline {
				i++
				if i == len(args) {
					return q, true, false, fmt.Errorf("%s needs a value", flag)
				}
				value = args[i]
			}
			switch flag {
			case "--metadata-field":
				key, v, ok := strings.Cut(value, "=")
				if !ok || key == "" {
					return q, true, false, fmt.Errorf("invalid metadata predicate %q", value)
				}
				if old, exists := q.Metadata[key]; exists && old != v {
					return q, true, false, fmt.Errorf("conflicting metadata predicates for %q", key)
				}
				q.Metadata[key] = v
			case "--status":
				q.Status = value
			case "--type":
				q.Type = value
			case "--assignee":
				q.Assignee = value
			default:
				n, err := strconv.Atoi(value)
				if err != nil || n < 0 {
					return q, true, false, fmt.Errorf("invalid limit %q", value)
				}
				q.Limit = n
			}
		default:
			return q, true, false, fmt.Errorf("unsupported split-storage role selector flag %q", args[i])
		}
	}
	if !jsonOutput {
		return q, true, false, fmt.Errorf("split-storage role selectors require --json")
	}
	if unassigned && q.Assignee != "" {
		return q, true, false, fmt.Errorf("--assignee and --no-assignee conflict")
	}
	return q, true, unassigned, nil
}

func mergeBdRoleSelector(work, graph []beads.Bead, q beads.ListQuery, unassigned bool, rig string) []beads.Bead {
	byID := make(map[string]beads.Bead, len(work)+len(graph))
	for _, b := range work {
		byID[b.ID] = b
	}
	for _, b := range graph {
		// The binding owns migrated copies even when the current copy no longer
		// matches the selector. Remove a frozen work copy before applying filters.
		delete(byID, b.ID)
		if rig != "" && b.Metadata["gc.root_store_ref"] != "rig:"+rig {
			continue
		}
		byID[b.ID] = b
	}
	items := make([]beads.Bead, 0, len(byID))
	for _, b := range byID {
		if !unassigned || b.Assignee == "" {
			items = append(items, b)
		}
	}
	return beads.ApplyListQuery(items, q)
}

func maybeRouteBdRoleSelector(cityPath string, cfg *config.City, target execStoreTarget, args []string, stdout, stderr io.Writer) (bool, int) {
	q, served, unassigned, parseErr := parseBdRoleSelector(args)
	if !served {
		return false, 0
	}
	door, relocated, err := openBdByIDClassFrontDoor(cityPath)
	if err == nil && !relocated {
		return false, 0
	}
	fail := func(err error) (bool, int) {
		fmt.Fprintf(stderr, "gc bd: role selector: %v\n", err) //nolint:errcheck // best-effort stderr
		return true, 1
	}
	if err != nil {
		return fail(err)
	}
	if parseErr != nil {
		return fail(parseErr)
	}
	store, err := openStoreAtForCityWithConfig(target.ScopeRoot, cityPath, cfg)
	if err != nil {
		return fail(err)
	}
	// Read before filtering so a migrated record cannot resurrect a frozen copy.
	scan := beads.ListQuery{IncludeClosed: true, AllowScan: true, TierMode: beads.FederatedReadTier, Live: true}
	work, err := store.List(scan)
	if err != nil {
		return fail(err)
	}
	graph, err := door.Store.List(scan)
	if err != nil {
		return fail(err)
	}
	if err := writeJSON(stdout, mergeBdRoleSelector(work, graph, q, unassigned, target.RigName)); err != nil {
		return fail(err)
	}
	return true, 0
}
