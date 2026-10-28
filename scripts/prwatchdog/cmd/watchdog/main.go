// Command watchdog observes the GitHub Checks API for a pull request's head
// commit and fails closed if complete CI evidence never appears for it.
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

// pollInterval is how often the watchdog re-checks the Checks API while
// waiting for evidence to complete.
const pollInterval = 30 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "prwatchdog: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	repo := os.Getenv("REPOSITORY")
	headSHA := os.Getenv("PR_HEAD_SHA")
	token := os.Getenv("GH_TOKEN")
	if repo == "" || headSHA == "" || token == "" {
		return fmt.Errorf("REPOSITORY, PR_HEAD_SHA, and GH_TOKEN must all be set")
	}

	fetcher := &prwatchdog.GitHubFetcher{
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		APIBase:    "https://api.github.com",
		Repo:       repo,
		Token:      token,
		CheckNames: prwatchdog.TrackedCheckNames(),
	}

	eval := prwatchdog.Watch(context.Background(), fetcher, realClock{}, realSleeper{}, prwatchdog.PollOptions{
		HeadSHA:                  headSHA,
		NeedsMacLabel:            parseBoolEnv("NEEDS_MAC_LABEL"),
		NeedsReviewFormulasLabel: parseBoolEnv("NEEDS_REVIEW_FORMULAS_LABEL"),
		Deadline:                 prwatchdog.ObservationDeadline,
		Interval:                 pollInterval,
	})

	summary := renderSummary(eval)
	fmt.Println(summary)
	if err := prwatchdog.WriteStepSummary(summary); err != nil {
		fmt.Fprintln(os.Stderr, "prwatchdog: writing step summary: "+err.Error())
	}

	if !eval.Pass {
		return fmt.Errorf("%s", eval.Reason)
	}
	return nil
}

func parseBoolEnv(name string) bool {
	v, _ := strconv.ParseBool(os.Getenv(name))
	return v
}

// realClock reports wall-clock time.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// realSleeper sleeps for a real duration, honoring context cancellation.
type realSleeper struct{}

func (realSleeper) Sleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// renderSummary produces the Markdown rendering shared by stdout and
// $GITHUB_STEP_SUMMARY.
func renderSummary(eval prwatchdog.Evaluation) string {
	var b strings.Builder
	if eval.Pass {
		b.WriteString("## PR evidence watchdog: PASS\n\n")
	} else {
		b.WriteString("## PR evidence watchdog: FAIL\n\n")
	}
	fmt.Fprintf(&b, "**Reason:** %s\n\n", eval.Reason)
	b.WriteString("| Check | State |\n")
	b.WriteString("| --- | --- |\n")
	fmt.Fprintf(&b, "| %s | %s |\n", prwatchdog.CheckName, eval.Summary.Check)
	fmt.Fprintf(&b, "| %s | %s |\n", prwatchdog.CIRequiredName, eval.Summary.CIRequired)
	fmt.Fprintf(&b, "| %s | %s |\n", prwatchdog.MacCheckName, eval.Summary.Mac)
	fmt.Fprintf(&b, "| %s | %s |\n", prwatchdog.ReviewFormulasCheckName, eval.Summary.ReviewFormulas)
	return b.String()
}
