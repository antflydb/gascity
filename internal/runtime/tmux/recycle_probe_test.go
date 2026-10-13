package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// RUNTIME-CONTRACT-009: a start path may kill or recycle an existing session
// only on a definitive "not running". These tests drive ensureFreshSession
// with probes that answer "unknown" and prove nothing is killed, the prompt
// file is cleaned up, and the error tells the caller to defer.

// unknownProbeError is the cause behind an unknown probe: a ps that is not
// installed, or a tmux server that went away between the probes. Both causes
// carry text IsSessionGone reads as gone, so a test that lets the cause text
// reach the error message would turn a deferred start into a dropped one.
func unknownProbeError(cause string) error {
	return &quietCauseError{
		msg:  `probing session "gc-test": ` + runtime.ErrRuntimeUnavailable.Error(),
		errs: []error{runtime.ErrRuntimeUnavailable, errors.New(cause)},
	}
}

func TestEnsureFreshSession_UnknownProbeNeverRecycles(t *testing.T) {
	running := true
	notRunning := false
	cases := []struct {
		name         string
		sessionState *bool
		sessionErr   error
		agentErr     error
		wantCalls    []string
	}{
		{
			name:         "agent probe unknown, pane running",
			sessionState: &running,
			agentErr:     unknownProbeError(`exec: "ps": executable file not found in $PATH`),
			wantCalls:    []string{"createSession", "isSessionRunning", "isRuntimeRunning"},
		},
		{
			name:         "session probe unknown",
			sessionState: &notRunning,
			sessionErr:   unknownProbeError("no tmux server running on /tmp/tmux-1000/default"),
			wantCalls:    []string{"createSession", "isSessionRunning"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			ops := &fakeStartOps{
				createErrs:             []error{ErrSessionExists},
				isSessionRunningResult: tc.sessionState,
				isSessionRunningErr:    tc.sessionErr,
				isRuntimeRunningErr:    tc.agentErr,
			}
			err := ensureFreshSession(ops, "gc-test", runtime.Config{
				WorkDir:      workDir,
				Command:      "claude",
				ProcessNames: []string{"claude"},
				PromptSuffix: "'" + strings.Repeat("x", maxInlinePromptLen+100) + "'",
			})
			if err == nil {
				t.Fatal("ensureFreshSession = nil, want an error that defers the start")
			}
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Errorf("ensureFreshSession = %v, want runtime.ErrRuntimeUnavailable", err)
			}
			if errors.Is(err, runtime.ErrSessionExists) {
				t.Errorf("ensureFreshSession = %v, must not read as runtime.ErrSessionExists", err)
			}
			if runtime.IsSessionGone(err) {
				t.Errorf("runtime.IsSessionGone(%v) = true; an unknown probe must not read as gone", err)
			}
			assertCallSequence(t, ops, tc.wantCalls)
			left, readErr := os.ReadDir(filepath.Join(workDir, ".gc", "tmp"))
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatalf("reading prompt dir: %v", readErr)
			}
			if len(left) != 0 {
				t.Errorf("prompt files left behind after the deferred start: %v", left)
			}
		})
	}
}

// A definitive "not running" still recycles; only the unknown is fenced.
func TestEnsureFreshSession_DefinitiveNotRunningStillRecycles(t *testing.T) {
	running := true
	notRunning := false
	cases := []struct {
		name         string
		sessionState *bool
		wantCalls    []string
	}{
		{
			name:         "dead pane",
			sessionState: &notRunning,
			wantCalls:    []string{"createSession", "isSessionRunning", "killSession", "createSession"},
		},
		{
			name:         "zombie",
			sessionState: &running,
			wantCalls:    []string{"createSession", "isSessionRunning", "isRuntimeRunning", "killSession", "createSession"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := &fakeStartOps{
				createErrs:             []error{ErrSessionExists},
				isSessionRunningResult: tc.sessionState,
			}
			err := ensureFreshSession(ops, "gc-test", runtime.Config{
				Command:      "claude",
				ProcessNames: []string{"claude"},
			})
			if err != nil {
				t.Fatalf("ensureFreshSession = %v, want a recycle", err)
			}
			assertCallSequence(t, ops, tc.wantCalls)
		})
	}
}

func TestEnsureFreshSession_LiveAgentStillExists(t *testing.T) {
	running := true
	ops := &fakeStartOps{
		createErrs:             []error{ErrSessionExists},
		isSessionRunningResult: &running,
		isRuntimeRunningResult: true,
	}
	err := ensureFreshSession(ops, "gc-test", runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
	})
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("ensureFreshSession = %v, want runtime.ErrSessionExists", err)
	}
	assertCallSequence(t, ops, []string{"createSession", "isSessionRunning", "isRuntimeRunning"})
}

// The deferral survives the whole start sequence, not just the recycle gate:
// callers classify with errors.Is(err, runtime.ErrRuntimeUnavailable).
func TestDoStartSession_UnknownProbeStaysUnavailable(t *testing.T) {
	running := true
	ops := &fakeStartOps{
		createErrs:             []error{ErrSessionExists},
		isSessionRunningResult: &running,
		isRuntimeRunningErr:    unknownProbeError(`exec: "ps": executable file not found in $PATH`),
	}
	err := doStartSession(context.Background(), ops, "gc-test", runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
	}, DefaultConfig().SetupTimeout)
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("doStartSession = %v, want runtime.ErrRuntimeUnavailable", err)
	}
	if errors.Is(err, runtime.ErrSessionExists) {
		t.Errorf("doStartSession = %v, must not read as runtime.ErrSessionExists", err)
	}
	assertCallSequence(t, ops, []string{"createSession", "isSessionRunning", "isRuntimeRunning"})
}
