package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/scripts/prwatchdog"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validEnv() map[string]string {
	return map[string]string{"GH_TOKEN": "t", "REPOSITORY": "gastownhall/gascity", "HEAD_SHA": testSHA}
}

func TestConfigFromEnv_Valid(t *testing.T) {
	cfg, err := configFromEnv(envOf(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.token != "t" || cfg.repository != "gastownhall/gascity" || cfg.headSHA != testSHA || cfg.dryRun {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestConfigFromEnv_DryRun(t *testing.T) {
	for value, want := range map[string]bool{"": false, "false": false, "0": false, "true": true, "1": true, "TRUE": true} {
		env := validEnv()
		env["DRY_RUN"] = value
		cfg, err := configFromEnv(envOf(env))
		if err != nil {
			t.Fatalf("DRY_RUN=%q: %v", value, err)
		}
		if cfg.dryRun != want {
			t.Errorf("DRY_RUN=%q: dryRun = %v, want %v", value, cfg.dryRun, want)
		}
	}
	// An unreadable flag must not silently become "really re-run".
	env := validEnv()
	env["DRY_RUN"] = "maybe"
	if _, err := configFromEnv(envOf(env)); err == nil {
		t.Fatal("DRY_RUN=maybe: want an error")
	}
}

func TestConfigFromEnv_RejectsMissingAndMalformed(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"GH_TOKEN", ""},
		{"REPOSITORY", ""},
		{"REPOSITORY", "gascity"},
		{"REPOSITORY", "a/b;id"},
		{"HEAD_SHA", ""},
		{"HEAD_SHA", strings.Repeat("a", 39)},
		{"HEAD_SHA", strings.Repeat("a", 41)},
		{"HEAD_SHA", strings.ToUpper(testSHA)},
		{"HEAD_SHA", "$(id)"},
	} {
		env := validEnv()
		env[tc.key] = tc.value
		if _, err := configFromEnv(envOf(env)); err == nil {
			t.Errorf("%s=%q: want an error", tc.key, tc.value)
		}
	}
}

func TestRenderSummary_CarriesEveryFieldTheRulingNames(t *testing.T) {
	res := prwatchdog.RewatchResult{
		HeadSHA:    testSHA,
		PRNumber:   42,
		RunID:      900,
		RunAttempt: 2,
		Decision:   prwatchdog.DecisionRerun,
		Reason:     "evidence now passes",
		Checks: prwatchdog.Summary{
			Check:          "success",
			CIRequired:     "success",
			Mac:            "not requested (opt-in)",
			ReviewFormulas: "not explicitly requested; path routing may still apply",
		},
	}
	got := renderSummary(res, nil)
	for _, want := range []string{
		testSHA, "42", "900", "2", string(prwatchdog.DecisionRerun), "evidence now passes",
		prwatchdog.CheckName, prwatchdog.CIRequiredName, prwatchdog.MacCheckName, prwatchdog.ReviewFormulasCheckName,
		"not requested (opt-in)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
}

func TestRenderSummary_StillWrittenWhenTheRunFailed(t *testing.T) {
	res := prwatchdog.RewatchResult{HeadSHA: testSHA}
	got := renderSummary(res, errors.New("listing watchdog runs: boom"))
	if !strings.Contains(got, testSHA) || !strings.Contains(got, "listing watchdog runs: boom") {
		t.Fatalf("summary must carry the SHA and the error:\n%s", got)
	}
}
