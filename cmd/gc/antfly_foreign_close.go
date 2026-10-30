package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The native work engine retains foreign references but cannot read their
// owners. The CLI checks them before forwarding a close; the work engine still
// performs its local dependency/lease/version checks atomically. Cross-store
// observations do not constitute a distributed transaction.
func checkAntflyForeignClose(ids []string, read func(string) (beads.Bead, error)) error {
	for _, id := range ids {
		source, err := read(id)
		if err != nil {
			return err
		}
		if source.ID != id {
			return fmt.Errorf("foreign-close source identity mismatch: requested %s, received %s", id, source.ID)
		}
		if source.Status == "closed" {
			continue
		}
		for _, dep := range source.Dependencies {
			if dep.Type != "blocks" {
				continue
			}
			target, capability := beads.ExternalBlockerTarget(dep.DependsOnID)
			if capability {
				continue
			}
			if target == dep.DependsOnID && beads.BdIDPrefix(target) == beads.BdIDPrefix(id) {
				continue
			}
			blocker, err := read(target)
			if err != nil {
				return fmt.Errorf("cannot verify foreign blocker %s for %s: %w", target, id, err)
			}
			if blocker.ID != target {
				return fmt.Errorf("foreign blocker identity mismatch: requested %s, received %s", target, blocker.ID)
			}
			if blocker.Status != "closed" {
				return fmt.Errorf("cannot close %s: foreign blocker %s is %s", id, target, blocker.Status)
			}
		}
	}
	return nil
}

func antflyCloseForced(args []string) bool {
	if len(args) == 0 {
		return false
	}
	values := bdSubcmdValueFlags(args[0])
	forced := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		name, value, inline := strings.Cut(arg, "=")
		if name == "--force" || name == "-f" {
			forced = !inline || value == "true"
			continue
		}
		if !inline && values[arg] && i+1 < len(args) {
			i++
		}
	}
	return forced
}

func guardAntflyForeignClose(cityPath, scopeRoot string, cfg *config.City, args []string) error {
	ids, closes := workRecordCloseTargets(args)
	if !closes || antflyCloseForced(args) || !providerUsesAntflyWorkStore(authoritativeBeadsProviderForScope(scopeRoot, cityPath)) {
		return nil
	}
	source, err := openStoreAtForCityWithConfig(scopeRoot, cityPath, cfg)
	if err != nil {
		return err
	}
	if closer, ok := source.(interface{ CloseStore() error }); ok {
		defer func() { _ = closer.CloseStore() }()
	}
	return checkAntflyForeignClose(ids, func(id string) (beads.Bead, error) {
		if beads.BdIDPrefix(id) == beads.BdIDPrefix(ids[0]) {
			return source.Get(id)
		}
		return loadWaitDependencyBead(cityPath, nil, id)
	})
}
