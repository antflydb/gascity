package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// cliRowRing is one journal-row doorbell the nudge's store rang.
type cliRowRing struct {
	conv string
	ids  []string
	// stdoutBefore counts the stdout writes that came before it: report()
	// has read the row back and printed by the time this is non-zero.
	stdoutBefore int
}

// emittingNudge runs one lane nudge whose session store is the one-shot
// emitter, as it is over SQLite or native Dolt: the journal-row writes of
// Propose and of the inline pass ring the controller through
// classStoreJournalRing. poked is the answer that seam returns. It returns
// the rings it recorded, with the exit code, stdout and stderr.
func (c *cliLaneCity) emittingNudge(t *testing.T, mode nudgeDeliveryMode, key string, poked bool) ([]cliRowRing, int, string, string) {
	t.Helper()
	var rings []cliRowRing
	var stdout, stderr bytes.Buffer
	stdoutWrites := 0
	prev := classStoreJournalRing
	classStoreJournalRing = func(_, conv string, ids []string) bool {
		rings = append(rings, cliRowRing{conv: conv, ids: slices.Clone(ids), stdoutBefore: stdoutWrites})
		c.events = append(c.events, "rowring")
		return poked
	}
	t.Cleanup(func() { classStoreJournalRing = prev })

	sessions := &emittingClassStore{Store: c.store, cityPath: c.dir}
	st, err := opjournal.NewLaneStore(beads.SessionStore{Store: sessions}, cliOpClock)
	if err != nil {
		t.Fatalf("opening the lane store: %v", err)
	}
	n := &cliLaneNudge{
		target: c.target, sessions: sessions, sp: c.sp, st: st,
		message: cliLaneTestMessage, mode: mode, key: key,
		stdout: writeCounter{w: eventWriter{buf: &stdout, c: c}, n: &stdoutWrites},
		stderr: &stderr,
	}
	code := n.run()
	return rings, code, stdout.String(), stderr.String()
}

// writeCounter counts the writes made through it.
type writeCounter struct {
	w interface{ Write([]byte) (int, error) }
	n *int
}

func (w writeCounter) Write(p []byte) (int, error) {
	*w.n++
	return w.w.Write(p)
}

// journalRowIDs lists every journal row (op, fact and runtime row) of the
// fixture's store.
func (c *cliLaneCity) journalRowIDs(t *testing.T) []string {
	t.Helper()
	rows, err := c.store.List(beads.ListQuery{Type: opjournal.BeadType, IncludeClosed: true})
	if err != nil {
		t.Fatalf("listing journal rows: %v", err)
	}
	var ids []string
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	return ids
}

// A queue-mode nudge on an emitting store rings the controller for its rows
// once, after report() has printed, naming the op row; nothing rings during
// Propose, and the plain lane ring is not rung when the row ring poked.
func TestSessionNudgeLaneQueueModeRingsRowsOnceAfterReport(t *testing.T) {
	c := newCLILaneCity(t)
	rings, code, stdout, stderr := c.emittingNudge(t, nudgeDeliveryQueue, "", true)
	if code != 0 {
		t.Fatalf("nudge = %d; stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "Queued nudge") {
		t.Fatalf("stdout %q, want a queued report", stdout)
	}
	op := c.only(t)
	if len(rings) != 1 {
		t.Fatalf("row rings = %+v, want exactly one, after report", rings)
	}
	if r := rings[0]; r.stdoutBefore == 0 || r.conv != c.target.sessionID || !slices.Contains(r.ids, op.ID) {
		t.Fatalf("row ring %+v, want one for %s naming %s after the report", r, c.target.sessionID, op.ID)
	}
	if len(c.rings) != 0 {
		t.Fatalf("plain lane rings %v, want none when the row ring poked", c.rings)
	}
	if stderr != "" {
		t.Fatalf("stderr %q, want no warning", stderr)
	}
}

// A live-mode nudge's Propose, Take and Done write rows that ring nothing
// until report() has read the op back; then one doorbell names the op row and
// every row the pass wrote.
func TestSessionNudgeLaneInlinePassRingsRowsOnceAfterReport(t *testing.T) {
	for _, mode := range []nudgeDeliveryMode{nudgeDeliveryWaitIdle, nudgeDeliveryImmediate} {
		t.Run(string(mode), func(t *testing.T) {
			c := newCLILaneCity(t)
			rings, code, _, stderr := c.emittingNudge(t, mode, "", true)
			if code != 0 {
				t.Fatalf("nudge = %d; stderr=%s", code, stderr)
			}
			op := c.only(t)
			if op.Status != opjournal.StatusClosed {
				t.Fatalf("op %s, want the pass to have delivered and closed it", op.Status)
			}
			if len(rings) != 1 {
				t.Fatalf("row rings = %+v, want exactly one, after report", rings)
			}
			r := rings[0]
			if r.stdoutBefore == 0 || r.conv != c.target.sessionID {
				t.Fatalf("row ring %+v, want one for %s after the report", r, c.target.sessionID)
			}
			for _, id := range c.journalRowIDs(t) {
				if !slices.Contains(r.ids, id) {
					t.Fatalf("row ring names %v, missing journal row %s", r.ids, id)
				}
			}
			if !slices.Contains(r.ids, op.ID) {
				t.Fatalf("row ring names %v, missing op %s", r.ids, op.ID)
			}
			if len(c.rings) != 0 {
				t.Fatalf("plain lane rings %v, want none when the row ring poked", c.rings)
			}
		})
	}
}

// A row ring no controller poked falls back to the plain ring for the op,
// once; an op still open then warns that no lane host answered, and one this
// run closed warns of nothing.
func TestSessionNudgeLaneUnpokedRowRingFallsBackToOpRing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     nudgeDeliveryMode
		wantWarn bool
	}{
		{name: "open op warns", mode: nudgeDeliveryQueue, wantWarn: true},
		{name: "closed op is silent", mode: nudgeDeliveryWaitIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			c.ringReply = "no-host"
			rings, code, _, stderr := c.emittingNudge(t, tc.mode, "", false)
			if code != 0 {
				t.Fatalf("nudge = %d; stderr=%s", code, stderr)
			}
			op := c.only(t)
			if len(rings) != 1 {
				t.Fatalf("row rings = %+v, want one, after report", rings)
			}
			if want := []string{c.target.sessionID + ":" + op.ID}; !slices.Equal(c.rings, want) {
				t.Fatalf("plain lane rings %v, want %v", c.rings, want)
			}
			warned := strings.Contains(stderr, "no controller lane host answered (no-host); op "+op.ID+" waits in the conversation lane")
			if warned != tc.wantWarn {
				t.Fatalf("warned %t, want %t; stderr=%q", warned, tc.wantWarn, stderr)
			}
			if !tc.wantWarn && stderr != "" {
				t.Fatalf("stderr %q, want none for a closed op", stderr)
			}
		})
	}
}

// Rerunning an op under its key rings once after report: a rerun of an open
// op whose pass delivers rings its rows, one that writes nothing rings the
// plain doorbell for the op instead, and the replay of a closed op rings
// nothing.
func TestSessionNudgeLaneRerunOfOpenOpRingsOnce(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         nudgeDeliveryMode
		firstBusy    bool
		wantRowRings int
		wantPlain    int
	}{
		{name: "rerun delivers", mode: nudgeDeliveryWaitIdle, firstBusy: true, wantRowRings: 1},
		{name: "rerun in queue mode writes nothing", mode: nudgeDeliveryQueue, wantPlain: 1},
		{name: "replay of a closed op rings nothing", mode: nudgeDeliveryWaitIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			if tc.firstBusy {
				c.busy()
			}
			if _, code, _, stderr := c.emittingNudge(t, tc.mode, "rerun-key", true); code != 0 {
				t.Fatalf("first nudge = %d; stderr=%s", code, stderr)
			}
			if tc.firstBusy {
				c.sp.SetActivity(c.name, time.Now().Add(-time.Minute))
			}
			c.rings, c.events = nil, nil
			rings, code, _, stderr := c.emittingNudge(t, tc.mode, "rerun-key", true)
			if code != 0 {
				t.Fatalf("rerun = %d; stderr=%s", code, stderr)
			}
			if len(rings) != tc.wantRowRings || len(c.rings) != tc.wantPlain {
				t.Fatalf("row rings %+v, plain rings %v; want %d and %d", rings, c.rings, tc.wantRowRings, tc.wantPlain)
			}
			for _, r := range rings {
				if r.stdoutBefore == 0 || !slices.Contains(r.ids, c.only(t).ID) {
					t.Fatalf("row ring %+v, want one after the report naming the op", r)
				}
			}
		})
	}
}

// A refusal rings nothing, however the store is wrapped.
func TestSessionNudgeLaneRefusalRingsNothingOnEmittingStore(t *testing.T) {
	c := newCLILaneCity(t)
	rings, code, _, _ := c.emittingNudge(t, nudgeDeliveryWaitIdle, " bad-key", true)
	if code != 1 {
		t.Fatalf("a bad key = %d, want refused", code)
	}
	if len(rings) != 0 || len(c.rings) != 0 {
		t.Fatalf("row rings %+v, plain rings %v after a refusal", rings, c.rings)
	}
}
