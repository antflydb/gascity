package readywork

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// narrowingStore is a leg whose backend APPLIES the pushed predicates, as a
// pushdown-capable store does: it honors ReadyQuery's advisory narrowing
// predicates exactly, and it cuts a bounded read in order after its readiness
// filter (beads.OrderedReadyReader), the way NativeDoltStore does. Its plain
// Ready keeps MemStore's own Limit semantics (insertion order), which is exactly
// the cut a bounded read must never be answered with.
type narrowingStore struct {
	*beads.MemStore
	mu      sync.Mutex
	ready   []beads.ReadyQuery
	ordered []orderedCall
}

type orderedCall struct {
	query beads.ReadyQuery
	order beads.ReadyOrder
}

func (s *narrowingStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	q := beads.ReadyQuery{}
	if len(query) > 0 {
		q = query[0]
	}
	s.mu.Lock()
	s.ready = append(s.ready, q)
	s.mu.Unlock()
	return s.narrowed(q)
}

func (s *narrowingStore) narrowed(q beads.ReadyQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.Ready(beads.ReadyQuery{Assignee: q.Assignee, TierMode: q.TierMode})
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, b := range rows {
		if q.Unassigned && b.Assignee != "" {
			continue
		}
		keep := true
		for k, v := range q.MetadataEquals {
			if got, ok := b.Metadata[k]; !ok || got != v {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, b)
		}
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (s *narrowingStore) ReadyOrdered(q beads.ReadyQuery, order beads.ReadyOrder) ([]beads.Bead, bool, error) {
	s.mu.Lock()
	s.ordered = append(s.ordered, orderedCall{query: q, order: order})
	s.mu.Unlock()
	limit := q.Limit
	q.Limit = 0
	rows, err := s.narrowed(q)
	if err != nil {
		return nil, false, err
	}
	switch order {
	case beads.ReadyOrderCanonical:
		beads.SortBeadsReadyOrder(rows)
	case beads.ReadyOrderOldest:
		beads.SortBeads(rows, beads.SortCreatedAsc)
	default:
		return rows, false, nil
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, true, nil
}

func (s *narrowingStore) readyQueries() []beads.ReadyQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ready)
}

func (s *narrowingStore) orderedCalls() []orderedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ordered)
}

// ignoringStore is a leg whose backend applies NONE of what the reader pushes:
// no advisory predicate, no assignee, no bound. It is the unpushed reader's
// reference answer, which every pushed answer must equal.
type ignoringStore struct {
	*beads.MemStore
}

func (s ignoringStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	q := beads.ReadyQuery{}
	if len(query) > 0 {
		q = query[0]
	}
	return s.MemStore.Ready(beads.ReadyQuery{TierMode: q.TierMode})
}

func (s ignoringStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	query.Assignee = ""
	return s.MemStore.List(query)
}

func newNarrowingStore(rows ...beads.Bead) *narrowingStore {
	return &narrowingStore{MemStore: beads.NewMemStoreFrom(0, rows, nil)}
}

// TestReadPushesTheSelectivePredicatesIntoEveryLeg pins what each leg is ASKED:
// the assignee, --unassigned and every plain-string key=value pair travel to
// the store, so a claim tier reads the rows routed or assigned to it rather than
// the city's whole ready set.
func TestReadPushesTheSelectivePredicatesIntoEveryLeg(t *testing.T) {
	city, rig := newNarrowingStore(), newNarrowingStore()
	legs := []Leg{testLeg("city", city), testLeg("rig r", rig)}
	_, err := Read(legs, Query{
		Assignee:       "  worker-1 ",
		Unassigned:     true,
		MetadataFields: []string{"gc.routed_to=gascity/worker", "gc.kind=workflow"},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, store := range []*narrowingStore{city, rig} {
		got := store.readyQueries()
		if len(got) != 1 {
			t.Fatalf("leg asked %d ready queries, want 1: %+v", len(got), got)
		}
		want := beads.ReadyQuery{
			Assignee:       "worker-1",
			TierMode:       beads.FederatedReadTier,
			Unassigned:     true,
			MetadataEquals: map[string]string{"gc.routed_to": "gascity/worker", "gc.kind": "workflow"},
		}
		if !reflect.DeepEqual(got[0], want) {
			t.Fatalf("leg asked %+v, want %+v", got[0], want)
		}
	}
}

// TestReadNeverPushesAPredicateABackendCouldCompareDifferently pins the other
// half: a predicate whose backend comparison is not provably the Go comparison
// stays Go-side, so no store can drop a row the Go filter keeps.
func TestReadNeverPushesAPredicateABackendCouldCompareDifferently(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query Query
	}{
		{name: "bare key", query: Query{MetadataFields: []string{"gc.routed_to"}}},
		{name: "JSON boolean value", query: Query{MetadataFields: []string{"gc.flag=true"}}},
		{name: "JSON number value", query: Query{MetadataFields: []string{"gc.count=42"}}},
		{name: "JSON null value", query: Query{MetadataFields: []string{"gc.x=null"}}},
		{name: "JSON object value", query: Query{MetadataFields: []string{`gc.x={"a":1}`}}},
		{name: "key outside bd's grammar", query: Query{MetadataFields: []string{"gc route=x"}}},
		{name: "excluded type", query: Query{ExcludeTypes: []string{"epic"}}},
		{name: "excluded label", query: Query{ExcludeLabels: []string{"hold:mayor"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newNarrowingStore()
			if _, err := Read([]Leg{testLeg("city", store)}, tc.query); err != nil {
				t.Fatalf("Read: %v", err)
			}
			got := store.readyQueries()
			if len(got) != 1 || len(got[0].MetadataEquals) != 0 || got[0].Unassigned || got[0].Assignee != "" {
				t.Fatalf("leg asked %+v; this predicate must stay Go-side", got)
			}
		})
	}
}

// TestReadPushesABoundedOrderedReadAndKeepsTheTopN pins the limit pushdown: a
// store that can cut in order is asked for Limit+boundedSlack rows in the
// query's order, and the merged answer is still the city's top N.
func TestReadPushesABoundedOrderedReadAndKeepsTheTopN(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var cityRows, rigRows []beads.Bead
	for i := range 60 {
		cityRows = append(cityRows, readyRow(fmt.Sprintf("c-%02d", i), i%4, base.Add(time.Duration(i)*time.Minute)))
		rigRows = append(rigRows, readyRow(fmt.Sprintf("r-%02d", i), (i+1)%4, base.Add(time.Duration(i)*time.Minute+time.Second)))
	}
	for _, sortOrder := range []string{"", SortOldest} {
		t.Run("sort="+sortOrder, func(t *testing.T) {
			city, rig := newNarrowingStore(cityRows...), newNarrowingStore(rigRows...)
			legs := []Leg{testLeg("city", city), testLeg("rig r", rig)}
			plain := []Leg{testLeg("city", beads.NewMemStoreFrom(0, cityRows, nil)), testLeg("rig r", beads.NewMemStoreFrom(0, rigRows, nil))}
			q := Query{Limit: 5, Sort: sortOrder}
			got, want := readIDs(t, legs, q), readIDs(t, plain, q)
			if !slices.Equal(got, want) {
				t.Fatalf("bounded answer = %v, want the unbounded top 5 %v", got, want)
			}
			wantOrder := beads.ReadyOrderCanonical
			if sortOrder == SortOldest {
				wantOrder = beads.ReadyOrderOldest
			}
			for _, store := range []*narrowingStore{city, rig} {
				calls := store.orderedCalls()
				if len(calls) != 1 || calls[0].query.Limit != 5+boundedSlack || calls[0].order != wantOrder {
					t.Fatalf("leg asked %+v, want one ordered read of %d rows in order %v", calls, 5+boundedSlack, wantOrder)
				}
				if len(store.readyQueries()) != 0 {
					t.Fatalf("leg also read its whole ready set: %+v", store.readyQueries())
				}
			}
		})
	}
}

// TestReadNeverPushesTheLimitUnderNewest: no store cuts in created_at
// descending order, so --sort newest reads every leg whole.
func TestReadNeverPushesTheLimitUnderNewest(t *testing.T) {
	store := newNarrowingStore(readyRow("c-1", 2, time.Now().UTC()))
	if _, err := Read([]Leg{testLeg("city", store)}, Query{Limit: 1, Sort: SortNewest}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if calls := store.orderedCalls(); len(calls) != 0 {
		t.Fatalf("--sort newest pushed a bounded read: %+v", calls)
	}
	if got := store.readyQueries(); len(got) != 1 || got[0].Limit != 0 {
		t.Fatalf("--sort newest asked %+v, want one unbounded read", got)
	}
}

// TestReadBoundedLegIsAskedAgainWhenGoOnlyPredicatesThinIt pins the refill: a
// predicate that stays Go-side can drop most of a leg's bounded rows, and the
// leg is then asked for more rather than leaving the answer short.
func TestReadBoundedLegIsAskedAgainWhenGoOnlyPredicatesThinIt(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var rows []beads.Bead
	for i := range 200 {
		held := i < 150
		rows = append(rows, readyRow(fmt.Sprintf("c-%03d", i), 1, base.Add(time.Duration(i)*time.Second), func(b *beads.Bead) {
			if held {
				b.Labels = []string{"hold:mayor"}
			}
		}))
	}
	store := newNarrowingStore(rows...)
	q := Query{Limit: 5, ExcludeLabels: []string{"hold:mayor"}}
	got := readIDs(t, []Leg{testLeg("city", store)}, q)
	want := readIDs(t, []Leg{testLeg("city", beads.NewMemStoreFrom(0, rows, nil))}, q)
	if !slices.Equal(got, want) || len(got) != 5 {
		t.Fatalf("bounded answer = %v, want the unbounded %v", got, want)
	}
	calls := store.orderedCalls()
	if len(calls) < 2 {
		t.Fatalf("leg asked %d bounded reads, want a refill after the Go-only predicate thinned the first", len(calls))
	}
	for i := 1; i < len(calls); i++ {
		if calls[i].query.Limit <= calls[i-1].query.Limit {
			t.Fatalf("refill limits %v do not grow", calls)
		}
	}
}

// TestReadBoundedAnswerFallsBackWhenTheBindingServesAnIDAnEarlierCutLegHolds
// pins the exactness guard. A migrated bead lives in the work store it was
// minted in AND in the class binding; an unbounded merge keeps the work store's
// copy. When the work leg was cut before that copy and the binding's copy lands
// in the answer, the bounded merge would serve the wrong copy, so the answer is
// read again unbounded.
func TestReadBoundedAnswerFallsBackWhenTheBindingServesAnIDAnEarlierCutLegHolds(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var workRows []beads.Bead
	for i := range 40 {
		workRows = append(workRows, readyRow(fmt.Sprintf("w-%02d", i), 1, base.Add(time.Duration(i)*time.Minute)))
	}
	// The work store's stale copy of the migrated bead ranks LAST there.
	workRows = append(workRows, readyRow("w-migrated", 3, base.Add(time.Hour), func(b *beads.Bead) { b.Title = "work copy" }))
	// The binding's live copy ranks FIRST in the city.
	graphRows := []beads.Bead{readyRow("w-migrated", 0, base, func(b *beads.Bead) { b.Title = "binding copy" })}

	work, graph := newNarrowingStore(workRows...), newNarrowingStore(graphRows...)
	legs := []Leg{testLeg("city", work), {Label: "graph", Store: graph, ClassBinding: true}}
	plain := []Leg{testLeg("city", beads.NewMemStoreFrom(0, workRows, nil)), {Label: "graph", Store: beads.NewMemStoreFrom(0, graphRows, nil), ClassBinding: true}}

	q := Query{Limit: 3}
	got, err := Read(legs, q)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want, err := Read(plain, q)
	if err != nil {
		t.Fatalf("Read (unbounded): %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bounded answer = %+v\nwant the unbounded answer %+v", got, want)
	}
	if len(work.readyQueries()) == 0 {
		t.Fatal("the guard never fell back to an unbounded read of the cut work leg")
	}
}

// TestReadBoundedAnswerTrustsTheBindingWhenNoCutLegHoldsItsRow is the control:
// a binding row no earlier leg holds keeps the bounded answer.
func TestReadBoundedAnswerTrustsTheBindingWhenNoCutLegHoldsItsRow(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var workRows []beads.Bead
	for i := range 40 {
		workRows = append(workRows, readyRow(fmt.Sprintf("w-%02d", i), 1, base.Add(time.Duration(i)*time.Minute)))
	}
	graphRows := []beads.Bead{readyRow("g-1", 0, base)}
	work, graph := newNarrowingStore(workRows...), newNarrowingStore(graphRows...)
	legs := []Leg{testLeg("city", work), {Label: "graph", Store: graph, ClassBinding: true}}
	if got := readIDs(t, legs, Query{Limit: 3}); !slices.Equal(got, []string{"g-1", "w-00", "w-01"}) {
		t.Fatalf("answer = %v", got)
	}
	if got := work.readyQueries(); len(got) != 0 {
		t.Fatalf("the work leg was read unbounded %d times; nothing it holds conflicts", len(got))
	}
}

// TestReadPushdownNeverChangesTheAnswer is the property the pushdown rests on:
// over random cities and random queries, every arm, legs that apply every pushed
// predicate and cut bounded reads in order give exactly the answer of legs that
// apply none of them, row for row and field for field, including on a city
// whose class binding carries relics of earlier legs' beads.
func TestReadPushdownNeverChangesTheAnswer(t *testing.T) {
	rng := rand.New(rand.NewSource(20261017))
	assignees := []string{"", "", "", "w1", "w2", " w1", "  "}
	routes := []string{"", "pool/a", "pool/b", "true", "42"}
	labels := []string{"", "hold:mayor", "human", "x"}
	types := []string{"task", "task", "bug", "epic"}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for trial := range 300 {
		legCount := 1 + rng.Intn(4)
		bindingHasRelics := rng.Intn(2) == 0
		var narrowLegs, plainLegs []Leg
		var earlierIDs []string
		for l := range legCount {
			isBinding := l == legCount-1 && legCount > 1
			var rows []beads.Bead
			n := rng.Intn(60)
			for i := range n {
				id := fmt.Sprintf("l%d-%03d", l, i)
				if isBinding && bindingHasRelics && len(earlierIDs) > 0 && rng.Intn(3) == 0 {
					id = earlierIDs[rng.Intn(len(earlierIDs))]
				}
				b := readyRow(id, rng.Intn(4), base.Add(time.Duration(rng.Intn(500))*time.Second), func(b *beads.Bead) {
					b.Status = []string{"open", "open", "open", "in_progress", "blocked", "closed"}[rng.Intn(6)]
					b.Assignee = assignees[rng.Intn(len(assignees))]
					b.Type = types[rng.Intn(len(types))]
					if r := routes[rng.Intn(len(routes))]; r != "" {
						b.Metadata = beads.StringMap{"gc.routed_to": r}
					}
					if lb := labels[rng.Intn(len(labels))]; lb != "" {
						b.Labels = []string{lb}
					}
				})
				if slices.ContainsFunc(rows, func(r beads.Bead) bool { return r.ID == b.ID }) {
					continue
				}
				rows = append(rows, b)
				earlierIDs = append(earlierIDs, b.ID)
			}
			label := fmt.Sprintf("leg %d", l)
			var reserved []string
			if isBinding {
				// The binding's own ids are in its reserved namespace; only the
				// relics it carries are work-shaped.
				reserved = []string{fmt.Sprintf("l%d", l)}
			}
			narrowLegs = append(narrowLegs, Leg{Label: label, Store: newNarrowingStore(rows...), ClassBinding: isBinding, ReservedPrefixes: reserved})
			plainLegs = append(plainLegs, Leg{Label: label, Store: ignoringStore{beads.NewMemStoreFrom(0, rows, nil)}, ClassBinding: isBinding, ReservedPrefixes: reserved})
		}
		q := Query{
			Assignee:   []string{"", "", "w1", "w2"}[rng.Intn(4)],
			Unassigned: rng.Intn(3) == 0,
			Limit:      []int{0, 1, 3, 20}[rng.Intn(4)],
			Sort:       []string{"", "", SortOldest, SortNewest}[rng.Intn(4)],
			Status:     []string{"", "", "", StatusInProgress, StatusOpen, StatusBlocked}[rng.Intn(6)],
		}
		if rng.Intn(2) == 0 {
			q.MetadataFields = []string{"gc.routed_to=" + routes[1+rng.Intn(len(routes)-1)]}
		}
		if rng.Intn(3) == 0 {
			q.ExcludeTypes = []string{"epic"}
		}
		if rng.Intn(3) == 0 {
			q.ExcludeLabels = []string{"hold:mayor"}
		}
		got, err := Read(narrowLegs, q)
		if err != nil {
			t.Fatalf("trial %d: Read: %v", trial, err)
		}
		want, err := Read(plainLegs, q)
		if err != nil {
			t.Fatalf("trial %d: Read (no pushdown): %v", trial, err)
		}
		if !reflect.DeepEqual(got, want) {
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			t.Fatalf("trial %d, query %+v:\npushed   %s\nunpushed %s", trial, q, gotJSON, wantJSON)
		}
	}
}
