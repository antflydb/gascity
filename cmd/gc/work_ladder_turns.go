package main

// The work ladder's rate-limit rule reads (audit 4, lever 2): what a seat's
// own transcript says its provider did with its latest turns. A seat whose
// turns the provider refuses with a rate limit is not ignoring its nudges, it
// cannot answer them, and the refusal may stick to its conversation;
// workladder.Decide recycles it to a fresh one. The reads go through the
// worker boundary (worker.TurnHandle), never the transcript directly, and the
// classification is the agent CLI's own (sessionlog): this file only counts.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

const (
	// ladderTurnReadBound bounds the parts of one seat's turn read that take
	// a context: the runtime probes of a turn still open. The session load
	// and the transcript's file reads take none.
	ladderTurnReadBound = 5 * time.Second
	// ladderReadMaxAge is how long a quiet seat's reading stands. A seat is
	// read again sooner when its activity, generation, conversation or the
	// ladder's last nudge to it moves; the bound catches the rest, such as a
	// refusal tmux's activity clock discounts (output right after a nudge).
	ladderReadMaxAge = 5 * time.Minute
	// ladderCanaryFlight is how long a provider's fresh-recycle canary may
	// stay unresolved before another seat on the provider may try its own.
	ladderCanaryFlight = 30 * time.Minute
)

// ladderTurnReader reads a seat's current and recent turns.
type ladderTurnReader func(ctx context.Context, row beads.Bead) (runtime.TurnHistory, error)

// rateLimitReading is one read of a seat's turns for the rate-limit rule, in
// the conversation and generation it was taken in.
type rateLimitReading struct {
	gen       int64
	key       string    // the session_key: the conversation read
	activity  time.Time // the seat's last activity when it was read
	lastNudge time.Time // the ladder's last nudge to the seat when it was read
	readAt    time.Time
	// refused is how many of the latest ended turns, newest first, the
	// provider refused with a rate limit; refusedAt when the newest ended,
	// and last its error.
	refused   int
	refusedAt time.Time
	last      runtime.TurnFailure
	// servedAt is when the newest ended turn ended, when the provider
	// answered it (it completed); zero otherwise.
	servedAt time.Time
	// err is a read that failed: no refusal, until the reading is read again.
	err error
}

// in reports whether the reading was taken in the conversation and
// generation meta's row is at now.
func (r rateLimitReading) in(meta map[string]string) bool {
	return r.gen == rowGeneration(meta) && r.key == strings.TrimSpace(meta["session_key"])
}

// rateLimitedTurns reads hist for the rule: how many of its latest ended
// turns, newest first, the provider refused with a rate limit, up to the
// first it did not, when the newest of them ended and its error; and when
// the newest ended turn ended if the provider answered it. A turn still open
// has no answer yet: no refusal, nothing served.
func rateLimitedTurns(hist runtime.TurnHistory) (refused int, refusedAt time.Time, last runtime.TurnFailure, servedAt time.Time) {
	if hist.Current.Active() || len(hist.Recent) == 0 {
		return 0, time.Time{}, runtime.TurnFailure{}, time.Time{}
	}
	if newest := hist.Recent[0]; newest.Status == runtime.TurnStatusCompleted {
		servedAt = newest.EndedAt
	}
	for _, t := range hist.Recent {
		if t.Status != runtime.TurnStatusFailed || !t.Failure.RateLimited {
			break
		}
		refused++
	}
	if refused == 0 {
		return 0, time.Time{}, runtime.TurnFailure{}, servedAt
	}
	return refused, hist.Recent[0].EndedAt, hist.Recent[0].Failure, servedAt
}

// ladderRateLimits caches each seat's last reading, and the last read that
// failed, so a seat whose turns cannot be read here is reported once per
// conversation rather than every pass.
type ladderRateLimits struct {
	mu     sync.Mutex
	last   map[string]rateLimitReading // by session ID
	failed map[string]string           // by session ID: the conversation and error last reported
}

// read is row's reading: the cached one while the seat has not moved (its
// activity, generation, conversation and the ladder's last nudge to it all
// as read) and is younger than ladderReadMaxAge, else a fresh read through
// read. A read that fails is cached the same way, as no refusal.
func (c *ladderRateLimits) read(ctx context.Context, read ladderTurnReader, row beads.Bead, activity, lastNudge, now time.Time) (rateLimitReading, error) {
	r := rateLimitReading{gen: rowGeneration(row.Metadata), key: strings.TrimSpace(row.Metadata["session_key"]), activity: activity, lastNudge: lastNudge, readAt: now}
	c.mu.Lock()
	cached, ok := c.last[row.ID]
	c.mu.Unlock()
	if ok && cached.gen == r.gen && cached.key == r.key && cached.activity.Equal(activity) && cached.lastNudge.Equal(lastNudge) &&
		now.Sub(cached.readAt) < ladderReadMaxAge {
		return cached, cached.err
	}
	ctx, cancel := context.WithTimeout(ctx, ladderTurnReadBound)
	defer cancel()
	hist, err := read(ctx, row)
	if err != nil {
		r.err = err
	} else {
		r.refused, r.refusedAt, r.last, r.servedAt = rateLimitedTurns(hist)
	}
	c.mu.Lock()
	if c.last == nil {
		c.last = make(map[string]rateLimitReading)
	}
	c.last[row.ID] = r
	c.mu.Unlock()
	return r, r.err
}

// firstFailure records that row's turns could not be read (err), and
// reports whether that is news: the first failure in its conversation, or a
// different one.
func (c *ladderRateLimits) firstFailure(row beads.Bead, err error) bool {
	note := fmt.Sprintf("%d|%s|%v", rowGeneration(row.Metadata), strings.TrimSpace(row.Metadata["session_key"]), err)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed == nil {
		c.failed = make(map[string]string)
	}
	if c.failed[row.ID] == note {
		return false
	}
	c.failed[row.ID] = note
	return true
}

// keep forgets every seat a pass no longer governs.
func (c *ladderRateLimits) keep(ids map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.last {
		if !ids[id] {
			delete(c.last, id)
		}
	}
	for id := range c.failed {
		if !ids[id] {
			delete(c.failed, id)
		}
	}
}

// ladderCanaries is the rule's per-provider evidence. A fresh conversation
// helps a seat whose refusal sticks to its conversation (2026-10-19: fresh
// seats on the cooling GLM pool were served) and nothing when the provider's
// whole pool refuses; then it only loses the seat's context. So one seat per
// provider at a time is recycled fresh, the provider's canary; other refused
// seats on the provider wait while it is in flight. If the canary's fresh
// conversation is refused too, the provider is refusing: its seats are paced
// as on a degraded provider (WL4: nudged at PausedCap, uncounted, never
// escalated or recycled) until a seat on it is served again. A turn served on
// the provider after the canary ends it either way. The state lives in
// memory: a controller restart costs at most one more canary per provider.
type ladderCanaries struct {
	mu sync.Mutex
	by map[string]providerCanary
}

// providerCanary is a provider's canary seat and when it was recycled, or,
// refusing, when its fresh conversation was refused too.
type providerCanary struct {
	seat     string
	since    time.Time
	refusing bool
}

// refusing reports whether provider's pool refused a fresh conversation and
// has served no turn since.
func (c *ladderCanaries) refusing(provider string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.by[provider].refusing
}

// held reports whether seat's fresh recycle must wait on provider's canary:
// another seat's is in flight, or the provider is refusing.
func (c *ladderCanaries) held(provider, seat string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.by[provider]
	switch {
	case !ok:
		return false
	case pc.refusing:
		return true
	case now.Sub(pc.since) >= ladderCanaryFlight:
		delete(c.by, provider)
		return false
	}
	return pc.seat != seat
}

// recycled records seat's fresh recycle at now as provider's canary.
func (c *ladderCanaries) recycled(provider, seat string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.by == nil {
		c.by = make(map[string]providerCanary)
	}
	if !c.by[provider].refusing {
		c.by[provider] = providerCanary{seat: seat, since: now}
	}
}

// refused records, at now, that provider refused a canary's fresh
// conversation too.
func (c *ladderCanaries) refused(provider, seat string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.by == nil {
		c.by = make(map[string]providerCanary)
	}
	c.by[provider] = providerCanary{seat: seat, since: now, refusing: true}
}

// served records a turn provider served that ended at at: a canary or a
// refusal recorded before it is over.
func (c *ladderCanaries) served(provider string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pc, ok := c.by[provider]; ok && at.After(pc.since) {
		delete(c.by, provider)
	}
}

// workerLadderTurns is the production turn source for one pass over env:
// each seat's turns read through its worker handle (the transcript of its
// current conversation, or its runtime's own turn record). The worker
// factory is built on the first read, so a pass with nothing to read builds
// none.
func workerLadderTurns(env externalReadsEnv) ladderTurnReader {
	var (
		factory *worker.Factory
		err     error
		built   bool
	)
	return func(ctx context.Context, row beads.Bead) (runtime.TurnHistory, error) {
		if !built {
			built = true
			factory, err = workerFactoryWithConfig(env.CityPath, env.CityStore, env.SP, env.Cfg)
		}
		if err != nil {
			return runtime.TurnHistory{}, fmt.Errorf("worker factory: %w", err)
		}
		h, err := factory.SessionByID(row.ID)
		if err != nil {
			return runtime.TurnHistory{}, err
		}
		return h.CurrentTurn(ctx)
	}
}
