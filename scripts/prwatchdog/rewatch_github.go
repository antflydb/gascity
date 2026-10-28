package prwatchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// watchdogWorkflowPath is the workflow file whose runs the rewatch helper
// looks up and re-runs.
const watchdogWorkflowPath = "pr-evidence-watchdog.yml"

// GitHubAPI is the REST client behind Rewatch. Every method is a single
// request: it never retries and never pages on its own.
type GitHubAPI struct {
	HTTPClient *http.Client
	APIBase    string
	Repo       string
	Token      string
}

// FetchCheckRuns returns the four tracked check runs on headSHA.
func (a *GitHubAPI) FetchCheckRuns(ctx context.Context, headSHA string) ([]CheckRun, error) {
	f := &GitHubFetcher{
		HTTPClient: a.HTTPClient,
		APIBase:    a.APIBase,
		Repo:       a.Repo,
		Token:      a.Token,
		CheckNames: TrackedCheckNames(),
	}
	return f.FetchCheckRuns(ctx, headSHA)
}

type workflowRunsResponse struct {
	WorkflowRuns []struct {
		ID         int64     `json:"id"`
		RunAttempt int       `json:"run_attempt"`
		Status     string    `json:"status"`
		Conclusion string    `json:"conclusion"`
		CreatedAt  time.Time `json:"created_at"`
	} `json:"workflow_runs"`
}

// ListWatchdogRuns returns the watchdog workflow's pull_request_target runs
// for headSHA (the first 100, which is every run for one commit in practice).
func (a *GitHubAPI) ListWatchdogRuns(ctx context.Context, headSHA string) ([]WorkflowRun, error) {
	q := url.Values{}
	q.Set("head_sha", headSHA)
	q.Set("event", "pull_request_target")
	q.Set("per_page", "100")
	reqURL := fmt.Sprintf("%s/repos/%s/actions/workflows/%s/runs?%s", a.APIBase, a.Repo, watchdogWorkflowPath, q.Encode())
	body, err := doGitHub(ctx, a.HTTPClient, http.MethodGet, reqURL, a.Token)
	if err != nil {
		return nil, err
	}
	var resp workflowRunsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decoding workflow runs: %w", err)
	}
	runs := make([]WorkflowRun, 0, len(resp.WorkflowRuns))
	for _, r := range resp.WorkflowRuns {
		runs = append(runs, WorkflowRun{ID: r.ID, RunAttempt: r.RunAttempt, Status: r.Status, Conclusion: r.Conclusion, CreatedAt: r.CreatedAt})
	}
	return runs, nil
}

type pullRequestsResponse []struct {
	Number int `json:"number"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// ListOpenPullRequests returns one page of the repository's open pull
// requests. The listing is the only source of a head sha that holds for fork
// pull requests too: commits/{sha}/pulls and workflow_run.pull_requests are
// empty for forks.
func (a *GitHubAPI) ListOpenPullRequests(ctx context.Context, page int) ([]PullRequest, error) {
	q := url.Values{}
	q.Set("state", "open")
	q.Set("per_page", strconv.Itoa(openPullRequestsPerPage))
	q.Set("page", strconv.Itoa(page))
	reqURL := fmt.Sprintf("%s/repos/%s/pulls?%s", a.APIBase, a.Repo, q.Encode())
	body, err := doGitHub(ctx, a.HTTPClient, http.MethodGet, reqURL, a.Token)
	if err != nil {
		return nil, err
	}
	var resp pullRequestsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decoding pull requests: %w", err)
	}
	prs := make([]PullRequest, 0, len(resp))
	for _, p := range resp {
		pr := PullRequest{Number: p.Number, HeadSHA: p.Head.SHA}
		for _, l := range p.Labels {
			pr.Labels = append(pr.Labels, l.Name)
		}
		prs = append(prs, pr)
	}
	return prs, nil
}

// Rerun asks GitHub to re-run every job of the workflow run.
func (a *GitHubAPI) Rerun(ctx context.Context, runID int64) error {
	reqURL := fmt.Sprintf("%s/repos/%s/actions/runs/%d/rerun", a.APIBase, a.Repo, runID)
	_, err := doGitHub(ctx, a.HTTPClient, http.MethodPost, reqURL, a.Token)
	return err
}
