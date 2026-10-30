package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/spf13/cobra"
	antfly "github.com/steveyegge/beads/backend/antfly"
)

// The root silences Cobra errors, so operator actions must render failures.
func storageMaintenanceCommand(cmd *cobra.Command, stderr io.Writer) *cobra.Command {
	action := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		err := action(cmd, args)
		if err != nil && !errors.Is(err, errExit) {
			fmt.Fprintf(stderr, "gc storage %s: %v\n", cmd.Name(), err) //nolint:errcheck // best-effort stderr
			return errExit
		}
		return err
	}
	return cmd
}

type storageSnapshotEngine interface {
	BackupSnapshot(context.Context) ([]byte, error)
	RestoreSnapshot(context.Context, []byte) error
}

// Snapshot operations use the binding's own domain validator. Restore is into
// a fresh workspace only, so no outstanding claim token can become current
// again through a rollback of a live ledger.
func newStorageSnapshotCmd(restore bool, stdout io.Writer) *cobra.Command {
	verb := "backup"
	if restore {
		verb = "restore"
	}
	var file string
	cmd := &cobra.Command{Use: verb + " <binding>", Short: verb + " a logical infrastructure workspace archive", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		resolved, e := resolveStorageMaintenanceEngine(args[0])
		if e != nil {
			return e
		}
		engine, ok := resolved.(storageSnapshotEngine)
		if !ok {
			return fmt.Errorf("binding %q does not expose workspace archives", args[0])
		}
		if restore {
			raw, e := os.ReadFile(file)
			if e != nil {
				return e
			}
			if e = engine.RestoreSnapshot(cmd.Context(), raw); e != nil {
				return e
			}
		} else {
			archive, e := engine.BackupSnapshot(cmd.Context())
			if e != nil {
				return e
			}
			if e = writeStorageSnapshot(file, archive); e != nil {
				return e
			}
		}
		_, e = fmt.Fprintf(stdout, "%s completed for binding %s: %s\n", verb, args[0], file)
		return e
	}}
	cmd.Flags().StringVar(&file, "file", "", "archive file (backup refuses an existing file; restore requires a fresh target workspace)")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func writeStorageSnapshot(path string, archive []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(archive); err != nil {
		return err
	}
	return f.Sync()
}

func resolveStorageMaintenanceEngine(binding string) (any, error) {
	request, err := resolveStorageOperatorRequest()
	if err != nil {
		return nil, err
	}
	storage := request.Cfg.EffectiveStorage()
	if _, ok := storage.Bindings[binding]; !ok {
		return nil, fmt.Errorf("unknown storage binding %q", binding)
	}
	for _, class := range coordclass.Classes() {
		if !class.IsInfrastructure() || storage.Classes.BindingFor(config.StorageClass(class.String())) != binding {
			continue
		}
		if store, ok := cliStorageRoutes(request.CityPath).storeFor(class); ok {
			return bindingEngine(store), nil
		}
	}
	return nil, fmt.Errorf("binding %q has no infrastructure engine", binding)
}

func newStorageGCCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{Use: "gc <binding>", Short: "collect unreachable immutable infrastructure records and resume interrupted collection", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		engine, err := resolveStorageMaintenanceEngine(args[0])
		if err != nil {
			return err
		}
		collector, ok := engine.(interface {
			CollectGarbage(context.Context) (antfly.SnapshotGCResult, error)
		})
		if !ok {
			return fmt.Errorf("binding %q does not expose immutable history collection", args[0])
		}
		result, err := collector.CollectGarbage(cmd.Context())
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "collected binding %s: scanned=%d deleted=%d retained=%d\n", args[0], result.Scanned, result.Deleted, result.Retained)
		return err
	}}
}
