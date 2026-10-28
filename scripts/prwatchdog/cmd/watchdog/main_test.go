package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/scripts/prwatchdog"
)

func TestRenderSummary_PassAndFail(t *testing.T) {
	summary := prwatchdog.Summary{
		Check:          "success",
		CIRequired:     "incomplete",
		Mac:            "not requested (opt-in)",
		ReviewFormulas: "not explicitly requested; path routing may still apply",
	}
	for _, tc := range []struct {
		name string
		eval prwatchdog.Evaluation
		want string
	}{
		{"pass", prwatchdog.Evaluation{Pass: true, Terminal: true, Reason: "all required CI evidence succeeded", Summary: summary}, "## PR evidence watchdog: PASS"},
		{"fail", prwatchdog.Evaluation{Terminal: true, Reason: "incomplete comprehensive evidence", Summary: summary}, "## PR evidence watchdog: FAIL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderSummary(tc.eval)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("summary missing %q:\n%s", tc.want, got)
			}
			for _, row := range []string{
				"| " + prwatchdog.CheckName + " | success |",
				"| " + prwatchdog.CIRequiredName + " | incomplete |",
				"| " + prwatchdog.MacCheckName + " | not requested (opt-in) |",
			} {
				if !strings.Contains(got, row) {
					t.Fatalf("summary missing row %q:\n%s", row, got)
				}
			}
			if !strings.Contains(got, "**Reason:** "+tc.eval.Reason) {
				t.Fatalf("summary missing reason:\n%s", got)
			}
		})
	}
}
