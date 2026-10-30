package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/maintenance"
	"github.com/spf13/cobra"
)

type storageRetentionEngine interface {
	RunSnapshot(context.Context, func(beads.Store) error) error
	ViewSnapshot(context.Context, func(beads.Store) error) error
}

func retainStorageSnapshot(ctx context.Context, engine storageRetentionEngine, policy string, options maintenance.Policy, dryRun bool) (maintenance.Result, error) {
	var result maintenance.Result
	var operation func(beads.Store) error
	switch policy {
	case "reap":
		operation = func(store beads.Store) (err error) {
			result, err = maintenance.Reap(store, options)
			return err
		}
	case "sessions":
		operation = func(store beads.Store) error {
			rows, err := store.List(beads.ListQuery{Type: "session", Status: "closed", UpdatedBefore: options.ClosedBefore, IncludeClosed: true, TierMode: beads.TierBoth, Sort: beads.SortCreatedAsc, Limit: wispGCSessionPurgeScanCap})
			if err != nil {
				return err
			}
			result = maintenance.Result{Scanned: len(rows), Closed: []string{}, Deleted: []string{}, Promoted: []string{}}
			if _, err = purgeClosedInfraSessions(store, options.Now, options.Now.Sub(options.ClosedBefore), options.Limit); err != nil {
				return err
			}
			for _, row := range rows {
				if _, err = store.Get(row.ID); errors.Is(err, beads.ErrNotFound) {
					result.Deleted = append(result.Deleted, row.ID)
				} else if err != nil {
					return err
				} else {
					result.Held++
				}
			}
			return nil
		}
	case "compact":
		operation = func(store beads.Store) (err error) {
			result, err = maintenance.Compact(store, options.Now, options.Limit)
			return err
		}
	default:
		return result, fmt.Errorf("unknown retention policy %q", policy)
	}
	if dryRun {
		err := engine.ViewSnapshot(ctx, operation)
		if err != nil {
			return maintenance.Result{}, err
		}
		return result, nil
	}
	err := engine.RunSnapshot(ctx, operation)
	// A failed publication must never be reported as committed changes.
	if err != nil {
		return maintenance.Result{}, err
	}
	return result, nil
}

func newStorageRetentionCmd(stdout io.Writer) *cobra.Command {
	var policy, storeRef string
	var staleAge, closedAge time.Duration
	var limit int
	var dryRun, all bool
	cmd := &cobra.Command{Use: "retain [binding]", Short: "apply typed infrastructure retention in one conditional snapshot", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if policy != "reap" && policy != "compact" && policy != "sessions" {
			return fmt.Errorf("unknown retention policy %q", policy)
		}
		if staleAge <= 0 || closedAge < 7*24*time.Hour || limit <= 0 {
			return fmt.Errorf("retention requires positive stale age and limit, and at least seven days of closed history")
		}
		if all == (len(args) == 1) {
			return fmt.Errorf("name exactly one binding or use --all")
		}
		bindings := append([]string(nil), args...)
		if all {
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				return err
			}
			storage := request.Cfg.EffectiveStorage()
			seen := map[string]bool{}
			for _, class := range coordclass.Classes() {
				binding := storage.Classes.BindingFor(config.StorageClass(class.String()))
				if class.IsInfrastructure() && binding != "" && binding != "work" && !seen[binding] {
					seen[binding] = true
					bindings = append(bindings, binding)
				}
			}
			sort.Strings(bindings)
		}
		results := map[string]maintenance.Result{}
		now := time.Now().UTC()
		for _, binding := range bindings {
			resolved, err := resolveStorageMaintenanceEngine(binding)
			if err != nil {
				return err
			}
			engine, ok := resolved.(storageRetentionEngine)
			if !ok {
				return fmt.Errorf("binding %q does not expose atomic retention snapshots", binding)
			}
			result, err := retainStorageSnapshot(cmd.Context(), engine, policy, maintenance.Policy{Now: now, StaleBefore: now.Add(-staleAge), ClosedBefore: now.Add(-closedAge), StoreRef: storeRef, Limit: limit}, dryRun)
			if err != nil {
				return fmt.Errorf("binding %s retention: %w", binding, err)
			}
			results[binding] = result
		}
		if !all {
			return writeCLIJSONLine(stdout, results[args[0]])
		}
		return writeCLIJSONLine(stdout, results)
	}}
	cmd.Flags().BoolVar(&all, "all", false, "visit each dedicated infrastructure binding once")
	cmd.Flags().StringVar(&policy, "policy", "reap", "retention policy: reap, compact or sessions")
	cmd.Flags().StringVar(&storeRef, "store-ref", "", "canonical root store reference; foreign workflow ownership is preserved")
	cmd.Flags().DurationVar(&staleAge, "stale-age", 24*time.Hour, "minimum age for orphan closure")
	cmd.Flags().DurationVar(&closedAge, "closed-age", 7*24*time.Hour, "minimum age for closed ephemeral deletion")
	cmd.Flags().IntVar(&limit, "limit", 500, "maximum records changed per publication")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "select and simulate without publishing any writes")
	return cmd
}
