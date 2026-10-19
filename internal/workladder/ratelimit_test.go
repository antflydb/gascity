package workladder

import (
	"testing"
	"time"
)

// The rate-limit rule (audit-4 lever 2): a seat whose provider refuses its
// turns with a rate limit is not answering its nudges, it cannot. Its
// started claims are recycled to a fresh conversation at once, keeping the
// claims, within the same recycle budget as an unanswered started claim
// (WL3); a fresh seat the provider refuses again is the pool itself, so the
// claim's alert names that, once, and the claim is paced at Cap.

// rateLimited is o with the seat's latest n turns refused for a rate limit.
func rateLimited(o Observation, n int) Observation {
	o.RateLimited = n
	return o
}

func TestDecideRateLimited(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	notDue := func(e Entry) Entry { e.Since, e.Last = t0, now.Add(-time.Minute); return e } // inside its backoff
	isDue := func(e Entry) Entry { e.Since, e.Last = t0, t0; return e }
	cases := []struct {
		name  string
		e     Entry
		obs   func(Observation) Observation
		want  []Action
		check func(*testing.T, Entry, Decision)
	}{
		{
			name: "started claim, refused twice: a fresh recycle keeping the claim, at once",
			e:    notDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 1}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2) },
			want: []Action{Recycle{Key: key("x"), KeepClaim: true, Fresh: true}, Alert{Key: key("x"), Reason: AlertRateLimitedRecycled}},
			check: func(t *testing.T, e Entry, d Decision) {
				if e.Recycles != 1 || !e.RecycleAt.Equal(now) || !e.Last.Equal(now) || e.Rung != RungStarted || e.RateLimitAlerted {
					t.Errorf("entry %+v", e)
				}
				if d.Next.RecycledGen != 1 {
					t.Errorf("recycled_gen = %d, want the decision's generation", d.Next.RecycledGen)
				}
			},
		},
		{
			name: "started claim, refused once: below the threshold, the ladder as before",
			e:    isDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 1}),
			obs:  func(o Observation) Observation { return rateLimited(o, 1) },
			want: []Action{Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 2}},
		},
		{
			name: "recycled already and refused again: the pool itself, alert once, paced at Cap",
			e:    notDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 1, Recycles: 1}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2) },
			want: []Action{Alert{Key: key("x"), Reason: AlertProviderRateLimited}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if e.Rung != RungEscalated || !e.RateLimitAlerted || e.Recycles != 1 {
					t.Errorf("entry %+v", e)
				}
				if got, want := p.dueAt(e, false), e.Last.Add(DefaultCap); !got.Equal(want) {
					t.Errorf("next due %v, want Cap after its last nudge (%v)", got, want)
				}
			},
		},
		{
			name: "recycled already, refused again and due: alert and the nudge it is owed",
			e:    isDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 2, Recycles: 1}),
			obs:  func(o Observation) Observation { return rateLimited(o, 3) },
			want: []Action{Alert{Key: key("x"), Reason: AlertProviderRateLimited}, Nudge{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Attempt: 3}},
		},
		{
			name: "the pool alert was raised: never again, nudged at Cap",
			e:    isDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Attempts: 9, Recycles: 1, Escalated: true, RateLimitAlerted: true}),
			obs:  func(o Observation) Observation { return rateLimited(o, 5) },
			want: []Action{Nudge{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Attempt: 10}},
		},
		{
			name: "an escalated claim not yet named for the pool: the pool alert, once",
			e:    notDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Attempts: 9, Recycles: 1, Escalated: true}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2) },
			want: []Action{Alert{Key: key("x"), Reason: AlertProviderRateLimited}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if !e.RateLimitAlerted || !e.Escalated {
					t.Errorf("entry %+v", e)
				}
			},
		},
		{
			name: "degraded provider: WL4, no recycle and no alert, a paused nudge when due",
			e:    isDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 2}),
			obs: func(o Observation) Observation {
				o.Provider = ProviderState{Degraded: true, Reason: "provider-health"}
				return rateLimited(o, 4)
			},
			want: []Action{Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 2, Paused: true}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if e.Recycles != 0 || e.RateLimitAlerted {
					t.Errorf("entry %+v", e)
				}
			},
		},
		{
			name: "an unstarted claim is not the rule's: the release stands",
			e:    isDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungClaimed, Attempts: 3}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2) },
			want: []Action{Release{Key: key("x")}},
		},
		{
			name: "a trigger is not the rule's",
			e:    isDue(Entry{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Attempts: 1}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2) },
			want: []Action{Nudge{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Attempt: 2}},
		},
		{
			name: "a continuation is not the rule's",
			e:    isDue(Entry{Key: key("x"), Kind: KindContinuation, Rung: RungAssigned, Attempts: 1}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2) },
			want: []Action{Nudge{Key: key("x"), Kind: KindContinuation, Rung: RungAssigned, Attempt: 2}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := quiet(now, 1, Work{Key: tc.e.Key, Kind: tc.e.Kind})
			if tc.e.Rung >= RungStarted {
				o.Started[tc.e.Key] = SrcAgent
			}
			if tc.obs != nil {
				o = tc.obs(o)
			}
			d := Decide(rec(1, tc.e), o, p)
			wantEffects(t, d, tc.want...)
			if !d.Changed {
				t.Error("an act must write the cell first")
			}
			if tc.check != nil {
				tc.check(t, entry(t, d, tc.e.Key.Bead), d)
			}
		})
	}
}

// The rule waits for what every other act waits for: an open gate, a live
// runtime and a quiet seat. A seat that is starting, held, or working (it
// may be retrying the provider itself) is left alone.
func TestDecideRateLimitedNeedsAQuietOpenSeat(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	e := Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 1, Last: now.Add(-time.Minute)}
	for name, mut := range map[string]func(*Observation){
		"gate held":    func(o *Observation) { o.Gate = Gate{Held: true, Reason: "restart_pending"} },
		"not quiet":    func(o *Observation) { o.Quiet = Quiet{State: NotQuiet, Reason: "busy"} },
		"quiet unread": func(o *Observation) { o.Quiet = Quiet{} },
		"runtime dead": func(o *Observation) { o.Runtime = TriFalse },
		"runtime unknown": func(o *Observation) {
			o.Runtime = TriUnknown
		},
		"work held on its read": func(o *Observation) { o.Hold = map[ClaimKey]bool{key("x"): true} },
	} {
		t.Run(name, func(t *testing.T) {
			o := rateLimited(startedBy1(quiet(now, 1, claim("x")), "x", SrcAgent), 3)
			mut(&o)
			d := Decide(rec(1, e), o, p)
			for _, a := range d.Actions {
				switch a.(type) {
				case Recycle, Alert:
					t.Fatalf("acted on a seat it must leave alone: %#v", d.Actions)
				}
			}
		})
	}
}

// One fresh recycle restarts the whole seat, so every started claim on it
// is spent on it, and no nudge goes to a seat that is being restarted.
func TestDecideRateLimitedRecyclesTheSeatOnce(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	prev := rec(1,
		Entry{Key: key("a"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: t0},
		Entry{Key: key("b"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 6, Last: t0},
		Entry{Key: key("t"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 1, Last: t0},
	)
	o := quiet(now, 1, claim("a"), claim("b"), trig("t"))
	o.Started[key("a")], o.Started[key("b")] = SrcAgent, SrcAgent
	d := Decide(prev, rateLimited(o, 2), p)
	wantEffects(t, d,
		Recycle{Key: key("a"), KeepClaim: true, Fresh: true}, Alert{Key: key("a"), Reason: AlertRateLimitedRecycled},
		Recycle{Key: key("b"), KeepClaim: true, Fresh: true}, Alert{Key: key("b"), Reason: AlertRateLimitedRecycled},
	)
	for _, b := range []string{"a", "b"} {
		if e := entry(t, d, b); e.Recycles != 1 {
			t.Errorf("%s: recycles %d, want 1", b, e.Recycles)
		}
	}
	if e := entry(t, d, "t"); e.Attempts != 1 {
		t.Errorf("the trigger was nudged on a seat being restarted: %+v", e)
	}
}

// A recycle the rule already asked for at this generation is not asked for
// again while the restart is on its way: the budget is spent, and the pool
// alert waits for the fresh seat's own refusals (a new generation).
func TestDecideRateLimitedWaitsForTheFreshSeat(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	e := Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 1, Last: now.Add(-time.Minute), Recycles: 1, RecycleAt: now.Add(-time.Minute)}
	prev := rec(1, e)
	prev.RecycledGen = 1
	d := Decide(prev, rateLimited(startedBy1(quiet(now, 1, claim("x")), "x", SrcAgent), 3), p)
	if len(d.Actions) != 0 {
		t.Fatalf("acted again before the fresh seat answered: %#v", d.Actions)
	}
	// The fresh seat (generation 2), refused twice in its own conversation.
	d = Decide(prev, rateLimited(startedBy1(quiet(now.Add(5*time.Minute), 2, claim("x")), "x", SrcAgent), 2), p)
	wantEffects(t, d, Alert{Key: key("x"), Reason: AlertProviderRateLimited})
}

func TestRateLimitPolicyDefaults(t *testing.T) {
	// Two refusals in a row: the agent's own turn, and its answer to the
	// ladder's first nudge.
	if got, want := DefaultPolicy().RateLimitThreshold, 2; got != want {
		t.Fatalf("RateLimitThreshold = %d, want %d", got, want)
	}
	if got := (Policy{}).withDefaults().RateLimitThreshold; got != DefaultRateLimitThreshold {
		t.Fatalf("an unset threshold fills to %d, got %d", DefaultRateLimitThreshold, got)
	}
	if got := (Policy{RateLimitThreshold: -1}).withDefaults().RateLimitThreshold; got != DefaultRateLimitThreshold {
		t.Fatalf("a negative threshold fills to %d, got %d", DefaultRateLimitThreshold, got)
	}
}

func TestCodecKeepsTheRateLimitAlert(t *testing.T) {
	r := Record{V: 1, Gen: 3, Entries: []Entry{{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Since: t0, Escalated: true, Recycles: 1, RateLimitAlerted: true}}}
	got, err := DecodeLadder(EncodeLadder(r))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Entries[0].RateLimitAlerted {
		t.Fatalf("decoded %+v lost rate_limit_alerted", got.Entries[0])
	}
	if sameRecord(r, Record{V: 1, Gen: 3, Entries: []Entry{{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Since: t0, Escalated: true, Recycles: 1}}}) {
		t.Fatal("sameRecord must see a change of rate_limit_alerted")
	}
}
