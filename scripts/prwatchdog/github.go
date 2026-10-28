package prwatchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// TrackedCheckNames lists the check runs the watchdog and the rewatch helper
// both gate on, in the order they are rendered.
func TrackedCheckNames() []string {
	return []string{CheckName, CIRequiredName, MacCheckName, ReviewFormulasCheckName}
}

// APIError is a non-2xx GitHub API response that is not a rate limit. Status
// and Body are kept apart so a caller can classify a refusal on both.
type APIError struct {
	Status int
	Body   string
}

// Error reports the status and the response body.
func (e *APIError) Error() string {
	return fmt.Sprintf("unexpected status %d: %s", e.Status, e.Body)
}

// GitHubFetcher retrieves check runs from the GitHub Checks API: one
// paginated request per poll for every run on the commit, filtered here to
// the tracked names. Asking per name cost one request per tracked check per
// poll against the token's rate limit for the same data.
type GitHubFetcher struct {
	HTTPClient *http.Client
	APIBase    string
	Repo       string
	Token      string
	CheckNames []string
}

type checkRunsResponse struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []checkRun `json:"check_runs"`
}

type checkRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	StartedAt  string `json:"started_at"`
}

// FetchCheckRuns returns every tracked check run on headSHA, paging until a
// short page.
func (f *GitHubFetcher) FetchCheckRuns(ctx context.Context, headSHA string) ([]CheckRun, error) {
	tracked := make(map[string]bool, len(f.CheckNames))
	for _, name := range f.CheckNames {
		tracked[name] = true
	}
	var out []CheckRun
	for pageNum := 1; ; pageNum++ {
		page, err := f.fetchPage(ctx, headSHA, pageNum)
		if err != nil {
			return nil, fmt.Errorf("fetching check runs: %w", err)
		}
		for _, r := range page.CheckRuns {
			if !tracked[r.Name] {
				continue
			}
			startedAt, _ := time.Parse(time.RFC3339, r.StartedAt)
			out = append(out, CheckRun{
				Name:       r.Name,
				HeadSHA:    r.HeadSHA,
				Status:     Status(r.Status),
				Conclusion: Conclusion(r.Conclusion),
				StartedAt:  startedAt,
				ID:         r.ID,
			})
		}
		if len(page.CheckRuns) < 100 {
			return out, nil
		}
	}
}

func (f *GitHubFetcher) fetchPage(ctx context.Context, headSHA string, pageNum int) (checkRunsResponse, error) {
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("page", strconv.Itoa(pageNum))
	reqURL := fmt.Sprintf("%s/repos/%s/commits/%s/check-runs?%s", f.APIBase, f.Repo, headSHA, q.Encode())

	body, err := doGitHub(ctx, f.HTTPClient, http.MethodGet, reqURL, f.Token)
	if err != nil {
		return checkRunsResponse{}, err
	}
	var page checkRunsResponse
	if err := json.Unmarshal(body, &page); err != nil {
		return checkRunsResponse{}, fmt.Errorf("decoding response: %w", err)
	}
	return page, nil
}

// doGitHub sends one authenticated request and returns the body of a 2xx
// response. A rate-limit refusal comes back as a *RateLimitError wrapping the
// *APIError; any other non-2xx comes back as a *APIError. It never retries.
func doGitHub(ctx context.Context, client *http.Client, method, reqURL, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
		if reset, ok := rateLimitReset(resp, time.Now()); ok {
			return nil, &RateLimitError{Reset: reset, Err: apiErr}
		}
		return nil, apiErr
	}
	return body, nil
}

// rateLimitReset reports when a 403/429 rate-limit refusal lifts, per
// GitHub's documented headers: Retry-After (seconds, secondary limits) or an
// exhausted primary limit (X-RateLimit-Remaining: 0 with X-RateLimit-Reset,
// a Unix time). Any other refusal is not a rate limit.
func rateLimitReset(resp *http.Response, now time.Time) (time.Time, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs >= 0 {
		return now.Add(time.Duration(secs) * time.Second), true
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return time.Time{}, false
	}
	unix, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(unix, 0), true
}

// WriteStepSummary appends summary to the file named by $GITHUB_STEP_SUMMARY.
// Outside GitHub Actions the variable is unset and it does nothing.
func WriteStepSummary(summary string) error {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(summary)
	return err
}
