// Command rewatch re-runs the PR evidence watchdog for a commit whose
// watchdog run failed or was canceled while the evidence it waited for has
// since passed. See .github/workflows/pr-evidence-rewatch.yml.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/scripts/prwatchdog"
)

// config is the program's whole input, read from the environment.
type config struct {
	token      string
	repository string
	headSHA    string
	dryRun     bool
}

func main() {
	cfg, err := configFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rewatch: "+err.Error())
		os.Exit(1)
	}
	api := &prwatchdog.GitHubAPI{
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		APIBase:    "https://api.github.com",
		Repo:       cfg.repository,
		Token:      cfg.token,
	}
	res, err := prwatchdog.Rewatch(context.Background(), api, prwatchdog.RewatchRequest{HeadSHA: cfg.headSHA, DryRun: cfg.dryRun})

	summary := renderSummary(res, err)
	fmt.Println(summary)
	if werr := prwatchdog.WriteStepSummary(summary); werr != nil {
		fmt.Fprintln(os.Stderr, "rewatch: writing step summary: "+werr.Error())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rewatch: "+err.Error())
		os.Exit(1)
	}
}

// configFromEnv reads and validates GH_TOKEN, REPOSITORY, HEAD_SHA and the
// optional DRY_RUN. A DRY_RUN it cannot read is an error, never "re-run for
// real".
func configFromEnv(getenv func(string) string) (config, error) {
	cfg := config{
		token:      getenv("GH_TOKEN"),
		repository: getenv("REPOSITORY"),
		headSHA:    getenv("HEAD_SHA"),
	}
	if cfg.token == "" {
		return config{}, fmt.Errorf("GH_TOKEN must be set")
	}
	if err := prwatchdog.ValidateRepository(cfg.repository); err != nil {
		return config{}, fmt.Errorf("REPOSITORY: %w", err)
	}
	if err := prwatchdog.ValidateHeadSHA(cfg.headSHA); err != nil {
		return config{}, fmt.Errorf("HEAD_SHA: %w", err)
	}
	if v := getenv("DRY_RUN"); v != "" {
		dry, err := strconv.ParseBool(v)
		if err != nil {
			return config{}, fmt.Errorf("DRY_RUN %q is not a boolean: %w", v, err)
		}
		cfg.dryRun = dry
	}
	return cfg, nil
}

// renderSummary produces the Markdown shared by stdout and
// $GITHUB_STEP_SUMMARY. It is written even when the run failed.
func renderSummary(res prwatchdog.RewatchResult, err error) string {
	var b strings.Builder
	b.WriteString("## PR evidence rewatch\n\n")
	fmt.Fprintf(&b, "- **Head SHA:** %s\n", res.HeadSHA)
	fmt.Fprintf(&b, "- **PR:** #%d\n", res.PRNumber)
	fmt.Fprintf(&b, "- **Watchdog run:** %d (attempt %d)\n", res.RunID, res.RunAttempt)
	fmt.Fprintf(&b, "- **Decision:** %s\n", res.Decision)
	fmt.Fprintf(&b, "- **Reason:** %s\n", res.Reason)
	if err != nil {
		fmt.Fprintf(&b, "- **Error:** %s\n", err)
	}
	b.WriteString("\n| Check | State |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| %s | %s |\n", prwatchdog.CheckName, res.Checks.Check)
	fmt.Fprintf(&b, "| %s | %s |\n", prwatchdog.CIRequiredName, res.Checks.CIRequired)
	fmt.Fprintf(&b, "| %s | %s |\n", prwatchdog.MacCheckName, res.Checks.Mac)
	fmt.Fprintf(&b, "| %s | %s |\n", prwatchdog.ReviewFormulasCheckName, res.Checks.ReviewFormulas)
	return b.String()
}
