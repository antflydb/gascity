package prwatchdog

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const rewatchWorkflowFile = "pr-evidence-rewatch.yml"

func workflowsDir() string { return filepath.Join("..", "..", ".github", "workflows") }

func readRewatchWorkflow(t *testing.T) ([]byte, map[string]any) {
	t.Helper()
	path := filepath.Join(workflowsDir(), rewatchWorkflowFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return body, doc
}

func loadRewatchWorkflow(t *testing.T) map[string]any {
	t.Helper()
	_, doc := readRewatchWorkflow(t)
	return doc
}

func rewatchJob(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	jobs, ok := doc["jobs"].(map[string]any)
	if !ok || len(jobs) != 1 {
		t.Fatalf("workflow must define exactly one job, got: %v", doc["jobs"])
	}
	job, ok := jobs["rewatch"].(map[string]any)
	if !ok {
		t.Fatalf("workflow must define the job id %q, got jobs: %v", "rewatch", jobs)
	}
	return job
}

func rewatchSteps(t *testing.T, job map[string]any) []map[string]any {
	t.Helper()
	raw, ok := job["steps"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatalf("job must have steps, got: %v", job["steps"])
	}
	steps := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		step, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("step is not a mapping: %v", r)
		}
		steps = append(steps, step)
	}
	return steps
}

func stringMap(t *testing.T, what string, v any) map[string]string {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s must be a mapping, got %T (%v)", what, v, v)
	}
	out := make(map[string]string, len(m))
	for k, raw := range m {
		s, ok := raw.(string)
		if !ok {
			t.Fatalf("%s[%q] must be a string, got %T (%v)", what, k, raw, raw)
		}
		out[k] = s
	}
	return out
}

func requireStringMapEquals(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want exactly %v", what, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s[%q] = %q, want %q (full: %v)", what, k, got[k], v, got)
		}
	}
}

func TestRewatchWorkflow_TriggerSetIsExact(t *testing.T) {
	doc := loadRewatchWorkflow(t)
	on, ok := doc["on"].(map[string]any)
	if !ok {
		t.Fatalf("workflow 'on' must be a mapping, got %T", doc["on"])
	}
	if len(on) != 1 {
		t.Fatalf("workflow must trigger on workflow_run only, got triggers: %v", on)
	}
	wr, ok := on["workflow_run"].(map[string]any)
	if !ok {
		t.Fatalf("workflow must trigger on workflow_run, got: %v", on)
	}
	types, ok := wr["types"].([]any)
	if !ok || len(types) != 1 || types[0] != "completed" {
		t.Fatalf("workflow_run.types = %v, want exactly [completed]", wr["types"])
	}
	for key := range wr {
		if key != "types" && key != "workflows" {
			t.Fatalf("workflow_run carries unexpected filter %q (a branches filter or similar narrows who gets healed): %v", key, wr)
		}
	}
}

// trackedCheckNames are the job names whose check runs the watchdog reads.
func trackedWorkflowNames(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(workflowsDir(), "*.y*ml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob workflows: %v (%d files)", err, len(paths))
	}
	tracked := map[string]bool{}
	for _, n := range TrackedCheckNames() {
		tracked[n] = true
	}
	found := map[string]bool{}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Name string `yaml:"name"`
			Jobs map[string]struct {
				Name string `yaml:"name"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, job := range doc.Jobs {
			if tracked[job.Name] {
				if doc.Name == "" {
					t.Fatalf("%s defines tracked check %q but has no workflow name", path, job.Name)
				}
				found[doc.Name] = true
			}
		}
	}
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func TestRewatchWorkflow_WorkflowsListIsDerived(t *testing.T) {
	doc := loadRewatchWorkflow(t)
	on, _ := doc["on"].(map[string]any)
	wr, _ := on["workflow_run"].(map[string]any)
	raw, ok := wr["workflows"].([]any)
	if !ok {
		t.Fatalf("workflow_run.workflows must be a literal list, got %v", wr["workflows"])
	}
	var got []string
	for _, r := range raw {
		s, ok := r.(string)
		if !ok {
			t.Fatalf("workflow_run.workflows entry %v is not a string", r)
		}
		got = append(got, s)
	}
	sort.Strings(got)

	want := trackedWorkflowNames(t)
	if len(want) != 3 {
		t.Fatalf("derived workflows = %v, want CI, Mac Regression and Review Formulas; the derivation is broken or a tracked check moved", want)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("workflow_run.workflows = %v, want %v: the workflows that define the tracked checks %v", got, want, TrackedCheckNames())
	}

	// Listing the watchdog (or this workflow) would loop: the watchdog's
	// completion would trigger a re-run of the watchdog.
	for _, own := range []string{"PR evidence watchdog", "PR evidence rewatch"} {
		if slices.Contains(got, own) {
			t.Fatalf("workflow_run.workflows lists %q; the watchdog's own name must never be listed", own)
		}
	}
}

func TestRewatchWorkflow_JobGuardRequiresPullRequestEvent(t *testing.T) {
	job := rewatchJob(t, loadRewatchWorkflow(t))
	guard, ok := job["if"].(string)
	if !ok {
		t.Fatalf("job must carry an if guard, got %v", job["if"])
	}
	normalized := strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(guard), "${{"), "}}")), " ")
	if normalized != "github.event.workflow_run.event == 'pull_request'" {
		t.Fatalf("job if = %q, want exactly the pull_request event check", guard)
	}
	if strings.Contains(guard, "conclusion") {
		t.Fatalf("job if = %q must not filter on conclusion: a re-run to green and a late green both complete, and the helper decides", guard)
	}
}

func TestRewatchWorkflow_PermissionsAreJobScoped(t *testing.T) {
	doc := loadRewatchWorkflow(t)
	requireStringMapEquals(t, "workflow permissions", stringMap(t, "workflow permissions", doc["permissions"]),
		map[string]string{"contents": "read"})
	job := rewatchJob(t, doc)
	requireStringMapEquals(t, "job permissions", stringMap(t, "job permissions", job["permissions"]),
		map[string]string{"actions": "write", "checks": "read", "contents": "read", "pull-requests": "read"})
}

func TestRewatchWorkflow_CheckoutIsWorkflowSHAOnly(t *testing.T) {
	steps := rewatchSteps(t, rewatchJob(t, loadRewatchWorkflow(t)))
	checkouts := 0
	for _, step := range steps {
		uses, _ := step["uses"].(string)
		if !strings.HasPrefix(uses, "actions/checkout@") {
			continue
		}
		checkouts++
		with := stringMapAny(t, "checkout with", step["with"])
		if len(with) != 2 {
			t.Fatalf("checkout with = %v, want exactly ref and persist-credentials", with)
		}
		// Under workflow_run, github.sha and github.event.workflow_run.head_sha
		// are not this workflow file's commit; only workflow_sha is trusted
		// default-branch content that also carries ./.github/actions.
		if with["ref"] != "${{ github.workflow_sha }}" {
			t.Fatalf("checkout ref = %v, want ${{ github.workflow_sha }}", with["ref"])
		}
		if persist, ok := with["persist-credentials"].(bool); !ok || persist {
			t.Fatalf("checkout persist-credentials = %v, want false", with["persist-credentials"])
		}
	}
	if checkouts != 1 {
		t.Fatalf("found %d checkout steps, want exactly 1", checkouts)
	}
}

func stringMapAny(t *testing.T, what string, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s must be a mapping, got %T", what, v)
	}
	return m
}

var eventContext = regexp.MustCompile(`github\.event\.[A-Za-z0-9_.\-\[\]*'"]+`)

func TestRewatchWorkflow_NoPayloadInRunScripts(t *testing.T) {
	doc := loadRewatchWorkflow(t)
	job := rewatchJob(t, doc)
	for i, step := range rewatchSteps(t, job) {
		if run, ok := step["run"].(string); ok && strings.Contains(run, "${{") {
			t.Fatalf("step %d run script interpolates an expression; pass it through env instead:\n%s", i, run)
		}
	}

	// Only head_sha (through env, and the concurrency key) and the event
	// name (in the if) may be read from the payload.
	body, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"github.event.workflow_run.head_sha": true,
		"github.event.workflow_run.event":    true,
	}
	for _, ref := range eventContext.FindAllString(string(body), -1) {
		if !allowed[ref] {
			t.Fatalf("workflow reads payload field %q; only head_sha and event are allowed", ref)
		}
	}

	for i, step := range rewatchSteps(t, job) {
		env, ok := step["env"].(map[string]any)
		if !ok {
			continue
		}
		for k, v := range env {
			s, _ := v.(string)
			if strings.Contains(s, "github.event") && !(k == "HEAD_SHA" && s == "${{ github.event.workflow_run.head_sha }}") {
				t.Fatalf("step %d env %s = %q reads the payload; only HEAD_SHA may, from workflow_run.head_sha", i, k, s)
			}
		}
	}
}

func TestRewatchWorkflow_ConcurrencyKeyedByHeadSHAAndDoesNotCancel(t *testing.T) {
	doc := loadRewatchWorkflow(t)
	concurrency, ok := doc["concurrency"].(map[string]any)
	if !ok {
		t.Fatalf("workflow must declare top-level concurrency, got: %v", doc["concurrency"])
	}
	if group := concurrency["group"]; group != "pr-evidence-rewatch-${{ github.event.workflow_run.head_sha }}" {
		t.Fatalf("concurrency.group = %v, want pr-evidence-rewatch-${{ github.event.workflow_run.head_sha }}", group)
	}
	cancel, ok := concurrency["cancel-in-progress"].(bool)
	if !ok || cancel {
		t.Fatalf("concurrency.cancel-in-progress = %v, want an explicit false (never kill a helper mid-POST)", concurrency["cancel-in-progress"])
	}
}

func TestRewatchWorkflow_JobNameDoesNotCollideWithTrackedChecks(t *testing.T) {
	doc := loadRewatchWorkflow(t)
	if doc["name"] != "PR evidence rewatch" {
		t.Fatalf("workflow name = %v, want %q", doc["name"], "PR evidence rewatch")
	}
	job := rewatchJob(t, doc)
	if job["name"] != "Rewatch / re-run PR evidence watchdog" {
		t.Fatalf("job name = %v, want %q", job["name"], "Rewatch / re-run PR evidence watchdog")
	}
	reserved := append([]string{RequiredCheckName}, TrackedCheckNames()...)
	for _, name := range reserved {
		if job["name"] == name || doc["name"] == name {
			t.Fatalf("name %q collides with a check name a PR could read as the watchdog or its evidence", name)
		}
	}
}

func TestRewatchWorkflow_InvokesTheRewatchProgram(t *testing.T) {
	job := rewatchJob(t, loadRewatchWorkflow(t))
	var runs []map[string]any
	for _, step := range rewatchSteps(t, job) {
		if run, _ := step["run"].(string); strings.Contains(run, "scripts/prwatchdog/cmd/rewatch") {
			runs = append(runs, step)
		}
	}
	if len(runs) != 1 {
		t.Fatalf("want exactly one step running scripts/prwatchdog/cmd/rewatch, found %d", len(runs))
	}
	env := stringMap(t, "rewatch step env", runs[0]["env"])
	requireStringMapEquals(t, "rewatch step env", env, map[string]string{
		"GH_TOKEN":   "${{ github.token }}",
		"REPOSITORY": "${{ github.repository }}",
		"HEAD_SHA":   "${{ github.event.workflow_run.head_sha }}",
	})
}

func TestRewatchWorkflow_BoundedTimeout(t *testing.T) {
	job := rewatchJob(t, loadRewatchWorkflow(t))
	timeout, ok := job["timeout-minutes"].(int)
	if !ok || timeout <= 0 || timeout > 15 {
		t.Fatalf("timeout-minutes = %v, want a bound > 0 and <= 15", job["timeout-minutes"])
	}
}

func TestRewatchWorkflow_NoSecretsAndNoWriteBeyondActions(t *testing.T) {
	doc := loadRewatchWorkflow(t)
	marshaled, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	// The parsed document, not the file: header comments may name these.
	text := string(marshaled)
	if strings.Contains(text, "secrets") {
		t.Fatalf("workflow must not reference secrets (no PAT, no app key): GITHUB_TOKEN only")
	}
	if strings.Count(text, ": write") != 1 || !strings.Contains(text, "actions: write") {
		t.Fatalf("the only write grant allowed is actions: write on the job:\n%s", text)
	}

	job := rewatchJob(t, doc)
	allowed := []string{"actions/checkout@", "actions/setup-go@", "./.github/actions/go-mod-download"}
	for i, step := range rewatchSteps(t, job) {
		uses, ok := step["uses"].(string)
		if !ok {
			continue
		}
		okUse := false
		for _, prefix := range allowed {
			if strings.HasPrefix(uses, prefix) {
				okUse = true
			}
		}
		if !okUse {
			t.Fatalf("step %d uses %q; the allowlist is closed to %v", i, uses, allowed)
		}
	}
	for i, step := range rewatchSteps(t, job) {
		if env, ok := step["env"].(map[string]any); ok {
			for k, v := range env {
				if s, _ := v.(string); strings.Contains(s, "secrets.") {
					t.Fatalf("step %d env %s reads a secret", i, k)
				}
			}
		}
	}
}

func TestRewatchWorkflow_HeaderCommentCoversWhySecurityAndBead(t *testing.T) {
	body, _ := readRewatchWorkflow(t)
	var header []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "#") {
			header = append(header, line)
			continue
		}
		if strings.HasPrefix(line, "name:") || len(strings.TrimSpace(line)) == 0 {
			continue
		}
		break
	}
	text := strings.Join(header, "\n")
	for _, want := range []string{"ga-j1jd7k", "workflow_run", "workflow_sha", "GITHUB_TOKEN"} {
		if !strings.Contains(text, want) {
			t.Fatalf("header comment must mention %q (why it exists, the security model, the bead); header:\n%s", want, text)
		}
	}
}
