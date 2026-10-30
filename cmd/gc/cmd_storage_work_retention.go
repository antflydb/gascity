package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/backend"
	antfly "github.com/steveyegge/beads/backend/antfly"
)

// Native work retention has a typed operator door. The opaque `antfly
// provider` protocol remains refused by generic gc bd on a split city.
func newStorageWorkRetentionCmd(stdout, stderr io.Writer) *cobra.Command {
	var policy, rig string
	var staleAge, closedAge, staleIssueAge time.Duration
	var limit int
	var dryRun bool
	cmd := &cobra.Command{Use: "retain-work", Short: "apply typed retention to one native Antfly work scope", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if (policy != "reap" && policy != "compact") || staleAge <= 0 || staleIssueAge <= 0 || closedAge < 7*24*time.Hour || limit <= 0 {
			return fmt.Errorf("retention requires a known policy, positive ages/limit and at least seven days of closed history")
		}
		request, err := resolveStorageOperatorRequest()
		if err != nil {
			return err
		}
		target, err := resolveBdScopeTarget(request.Cfg, request.CityPath, rig, nil, true, stderr)
		if err != nil {
			return err
		}
		provider := rawBeadsProviderForScope(target.ScopeRoot, request.CityPath)
		if !strings.HasPrefix(provider, "exec:") || filepath.Base(strings.TrimPrefix(provider, "exec:")) != "gc-beads-antfly" {
			return fmt.Errorf("scope %s does not select the native Antfly work provider", target.ScopeRoot)
		}
		workspace, err := antfly.Open(cmd.Context(), filepath.Join(target.ScopeRoot, ".beads"), backend.OpenOptions{})
		if err != nil {
			return err
		}
		defer func() { _ = workspace.Close() }()
		args := []string{"--policy", policy, "--stale-age", staleAge.String(), "--closed-age", closedAge.String(), "--stale-issue-age", staleIssueAge.String(), "--limit", strconv.Itoa(limit)}
		if rig == "" {
			args = append(args, "--city-scope")
		} else {
			args = append(args, "--store-ref", "rig:"+rig)
		}
		if dryRun {
			args = append(args, "--dry-run")
		}
		result, err := workspace.ExecuteFactory(cmd.Context(), "retention", args, nil, "gc-maintenance")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, string(result))
		return err
	}}
	cmd.Flags().StringVar(&policy, "policy", "reap", "retention policy: reap or compact")
	cmd.Flags().StringVar(&rig, "work-rig", "", "explicit rig work scope; empty selects city work")
	cmd.Flags().DurationVar(&staleAge, "stale-age", 24*time.Hour, "minimum orphan age")
	cmd.Flags().DurationVar(&closedAge, "closed-age", 7*24*time.Hour, "minimum closed ephemeral age")
	cmd.Flags().DurationVar(&staleIssueAge, "stale-issue-age", 30*24*time.Hour, "minimum stale city work age")
	cmd.Flags().IntVar(&limit, "limit", 500, "maximum records changed per publication")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "select and simulate without publishing")
	return cmd
}
