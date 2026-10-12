package main

import (
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/resilience"
)

// fleetBreakerScope is the breaker.state_changed scope of the fleet start
// breaker.
const fleetBreakerScope = "fleet"

// fleetOutcome is what one automatic start says about fleet start health.
type fleetOutcome int

const (
	// fleetInconclusive counts nothing: a capacity refusal, a deferral, a
	// start that never reached the provider, or an unresolved collision.
	fleetInconclusive fleetOutcome = iota
	// fleetStartOK is a start that returned without error; it is only
	// confirmed once the session is observed alive for stabilityThreshold.
	fleetStartOK
	// fleetStartFailed is an observed start failure.
	fleetStartFailed
)

// classifyFleetOutcome maps a start result onto its fleet outcome.
func classifyFleetOutcome(_ startResult) fleetOutcome {
	return fleetInconclusive
}

// confirmVerdict is a start-confirmation tracker's reading of one session.
type confirmVerdict int

const (
	// confirmUntracked means the session has no start being confirmed.
	confirmUntracked confirmVerdict = iota
	// confirmPending means the session is alive but not yet for the threshold.
	confirmPending
	// confirmAlive means the session stayed alive for the threshold.
	confirmAlive
	// confirmDied means the session died before the threshold.
	confirmDied
)

// startConfirmTracker confirms started sessions: alive for a threshold after
// Begin confirms, dead before it fails.
type startConfirmTracker struct{}

// newStartConfirmTracker returns a tracker on the given clock.
func newStartConfirmTracker(_ func() time.Time, _ time.Duration) *startConfirmTracker {
	return &startConfirmTracker{}
}

// Begin starts confirming id from now.
func (t *startConfirmTracker) Begin(_ string) {}

// Observe reads id's liveness and returns its verdict; a confirmed or died
// session is forgotten.
func (t *startConfirmTracker) Observe(_ string, _ bool) confirmVerdict {
	return confirmUntracked
}

// Forget drops id without a verdict.
func (t *startConfirmTracker) Forget(_ string) {}

// fleetStatus is the fleet breaker's state as operators see it.
type fleetStatus struct {
	State         resilience.State
	Trips         int
	BackoffCap    time.Duration
	NextProbeAt   time.Time
	OpenSince     time.Time
	ProbeInFlight bool
	// Reason is why the breaker last opened: "failures" or "state-cache".
	Reason string
}

// fleetStartGuard is the fleet-wide breaker over automatic session starts.
type fleetStartGuard struct {
	registry *resilience.Registry
}

// fleetTicket is one admitted automatic start. Resolve it exactly once.
type fleetTicket struct{}

// newFleetStartGuard returns a guard on the given clock whose half-open probe
// window is startupTimeout plus a grace.
func newFleetStartGuard(now func() time.Time, _ time.Duration) *fleetStartGuard {
	settings := resilience.Settings{Now: now}
	return &fleetStartGuard{registry: resilience.NewRegistry(settings)}
}

// Eligible reports whether a start could be admitted now. Read-only.
func (g *fleetStartGuard) Eligible() bool { return true }

// Admit is the commitment point for one automatic start of sessionID, an
// instance of template.
func (g *fleetStartGuard) Admit(_, _ string) (*fleetTicket, bool) {
	return nil, true
}

// Probe reports whether the ticket is the half-open probe.
func (t *fleetTicket) Probe() bool { return false }

// Resolve applies the attempt's outcome. Idempotent and nil-safe.
func (t *fleetTicket) Resolve(_ fleetOutcome) {}

// ObserveLiveness feeds one session's liveness to the start confirmation.
func (g *fleetStartGuard) ObserveLiveness(_ string, _ bool) {}

// NoteStateCache feeds the age of the runtime's last successful state
// refresh; known is false when the runtime keeps no such cache.
func (g *fleetStartGuard) NoteStateCache(_ time.Duration, _ bool, _ int) {}

// Reset closes the breaker and forgets its failure window. It reports
// whether the breaker was open or half-open.
func (g *fleetStartGuard) Reset() bool { return false }

// Status returns the breaker's current state.
func (g *fleetStartGuard) Status() fleetStatus { return fleetStatus{Reason: fleetBreakerScope} }

// Flush emits queued transitions: breaker.state_changed events and the
// per-episode stderr lines. It runs outside every lock.
func (g *fleetStartGuard) Flush(_ events.Recorder, _ io.Writer) {}
