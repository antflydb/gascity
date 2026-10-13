package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// reconcileIdleSessionsWithFloor reconciles n quiet pool-style sessions, each
// holding a 72h-old activity stamp, through the real idle tracker with the
// given activity floor, and returns how many the pass stopped.
func reconcileIdleSessionsWithFloor(t *testing.T, env *reconcilerTestEnv, names []string, sessions []beads.Bead, it *memoryIdleTracker, floor activityFloor) int {
	t.Helper()
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeadsAtPath(
		context.Background(), env.city, sessions, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withAsyncDrainAckStopTracker(env.stops),
		withActivityFloor(floor),
	)
	stopped := 0
	for _, name := range names {
		if !env.sp.IsRunning(name) {
			stopped++
		}
	}
	return stopped
}

func newQuietIdleFixture(t *testing.T, n int) (*reconcilerTestEnv, []string, []beads.Bead, *memoryIdleTracker) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	it := newIdleTracker()
	var names []string
	var sessions []beads.Bead
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("worker-%d", i)
		env.addDesired(name, "worker", true)
		session := env.createSessionBead(name, "worker")
		env.markSessionActive(&session)
		if err := env.sp.SetMeta(name, "GC_SESSION_ID", session.ID); err != nil {
			t.Fatalf("SetMeta(%s, GC_SESSION_ID): %v", name, err)
		}
		env.sp.SetActivity(name, env.clk.Now().Add(-72*time.Hour))
		it.setTimeout(name, 15*time.Minute)
		names = append(names, name)
		sessions = append(sessions, session)
	}
	return env, names, sessions, it
}

// TestReconcileSessionBeads_ResumeFloorSparesQuietSessionsOnFirstPass is the
// ga-w21nzz incident: after a host suspend every session's wall-clock
// activity stamp is hours old, and without a floor the first pass stops all
// of them at once.
func TestReconcileSessionBeads_ResumeFloorSparesQuietSessionsOnFirstPass(t *testing.T) {
	env, names, sessions, it := newQuietIdleFixture(t, 20)
	floor := activityFloor{at: env.clk.Now().Add(-5 * time.Second)}
	if got := reconcileIdleSessionsWithFloor(t, env, names, sessions, it, floor); got != 0 {
		t.Fatalf("first pass after a detected resume stopped %d of %d sessions, want 0", got, len(names))
	}
}

func TestReconcileSessionBeads_ResumeFloorSessionsStopOnceIdleTimeoutElapsesPastResume(t *testing.T) {
	env, names, sessions, it := newQuietIdleFixture(t, 20)
	floor := activityFloor{at: env.clk.Now().Add(-16 * time.Minute)}
	if got := reconcileIdleSessionsWithFloor(t, env, names, sessions, it, floor); got != len(names) {
		t.Fatalf("stopped %d of %d sessions once idle_timeout elapsed past the resume, want all", got, len(names))
	}
}

func TestReconcileSessionBeads_NoResumeControlStopsStaleSessionsFirstPass(t *testing.T) {
	env, names, sessions, it := newQuietIdleFixture(t, 20)
	if got := reconcileIdleSessionsWithFloor(t, env, names, sessions, it, activityFloor{}); got != len(names) {
		t.Fatalf("control: stopped %d of %d stale sessions with no resume, want all", got, len(names))
	}
}

func TestReconcileSessionBeads_ResumeFloorProgressStallClaimless(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	env.startOptions = append(env.startOptions, withActivityFloor(activityFloor{at: env.clk.Now().Add(-time.Minute)}))

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("claim-less progress-stall recycled a session 1m after a detected resume; stall time is measured from the resume")
	}
}

func TestReconcileSessionBeads_ResumeFloorProgressStallRecyclesOncePastResume(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	env.startOptions = append(env.startOptions, withActivityFloor(activityFloor{at: env.clk.Now().Add(-31 * time.Minute)}))

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("progress-stall did not recycle a session stalled 31m past the resume (threshold 30m)")
	}
}
