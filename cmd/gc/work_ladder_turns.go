package main

// The work ladder's rate-limit rule reads (audit 4, lever 2): what a seat's
// own transcript says its provider did with its latest turns. A seat whose
// turns the provider refuses with a rate limit (a pool cooling down) is not
// ignoring its nudges, it cannot answer them, and the refusal sticks to its
// conversation; workladder.Decide recycles it to a fresh one. The reads go
// through the worker boundary (worker.TurnHandle), never the transcript
// directly, and the classification is the agent CLI's own (sessionlog): this
// file only counts.

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

// ladderTurnReadBound bounds one seat's turn read: its session reload and
// runtime probes (the transcript's own file reads take no context).
const ladderTurnReadBound = 5 * time.Second

// ladderTurnReader reads a seat's current and recent turns.
type ladderTurnReader func(ctx context.Context, row beads.Bead) (runtime.TurnHistory, error)

// rateLimitReading is one read of a seat's turns for the rate-limit rule:
// how many of its latest turns its provider refused with a rate limit, in
// the conversation and generation it was read in, and the newest refusal.
type rateLimitReading struct {
	gen      int64
	key      string    // the session_key: the conversation read
	activity time.Time // the seat's last activity when it was read
	refused  int
	last     runtime.TurnFailure
	// err is a read that failed: no refusal, until the seat moves.
	err error
}

// in reports whether the reading was taken in the conversation and
// generation meta's row is at now.
func (r rateLimitReading) in(meta map[string]string) bool {
	return r.gen == rowGeneration(meta) && r.key == strings.TrimSpace(meta["session_key"])
}

// rateLimitedTurns counts hist's latest ended turns, newest first, that the
// provider refused with a rate limit, up to the first it did not, and
// returns the newest refusal. A turn still open has no answer yet: none.
func rateLimitedTurns(hist runtime.TurnHistory) (int, runtime.TurnFailure) {
	if hist.Current.Active() {
		return 0, runtime.TurnFailure{}
	}
	n := 0
	for _, t := range hist.Recent {
		if t.Status != runtime.TurnStatusFailed || !t.Failure.RateLimited {
			break
		}
		n++
	}
	if n == 0 {
		return 0, runtime.TurnFailure{}
	}
	return n, hist.Recent[0].Failure
}

// ladderRateLimits caches each seat's last reading, so a quiet seat's turns
// are read once per change of its activity, generation or conversation, and
// the last read that failed, so a seat whose turns cannot be read here is
// reported once per conversation rather than every pass.
type ladderRateLimits struct {
	mu     sync.Mutex
	last   map[string]rateLimitReading // by session ID
	failed map[string]string           // by session ID: the conversation and error last reported
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

// read is row's reading at activity: the cached one while the seat has not
// moved, else a fresh read through read. A read that fails is no refusal,
// and is cached the same way: the seat is read again when it moves.
func (c *ladderRateLimits) read(ctx context.Context, read ladderTurnReader, row beads.Bead, activity time.Time) (rateLimitReading, error) {
	r := rateLimitReading{gen: rowGeneration(row.Metadata), key: strings.TrimSpace(row.Metadata["session_key"]), activity: activity}
	c.mu.Lock()
	cached, ok := c.last[row.ID]
	c.mu.Unlock()
	if ok && cached.gen == r.gen && cached.key == r.key && cached.activity.Equal(activity) {
		return cached, cached.err
	}
	ctx, cancel := context.WithTimeout(ctx, ladderTurnReadBound)
	defer cancel()
	hist, err := read(ctx, row)
	if err != nil {
		r.err = err
	} else {
		r.refused, r.last = rateLimitedTurns(hist)
	}
	c.mu.Lock()
	if c.last == nil {
		c.last = make(map[string]rateLimitReading)
	}
	c.last[row.ID] = r
	c.mu.Unlock()
	return r, r.err
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
