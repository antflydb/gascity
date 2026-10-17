package readywork

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fanout"
	"github.com/gastownhall/gascity/internal/storeref"
)

// Pushdown: what each leg is asked, so that it reads only the rows the answer
// can contain.
//
// Without it, every leg returns its WHOLE ready set and the predicates are
// applied here: on a city with eleven thousand ready beads, a claim tier asking
// for the one bead assigned to it read all eleven thousand from every store,
// then kept one. The pushed predicates make each leg's read selective.
//
// # Each pushed predicate is no stricter than the Go filter
//
// A store may narrow by a pushed predicate or ignore it (beads.ReadyQuery's
// advisory contract); either way the predicate is re-applied to the merged set
// by compiled.matches, which stays the authority. What pushing must never do is
// drop a row the Go filter keeps, so a predicate is pushed only where every
// backend's comparison is provably the Go comparison:
//
//   - --assignee and --unassigned compare the stored assignee exactly in Go, as
//     `bd ready` and every store's own Assignee filter do.
//   - --metadata-field key=value is pushed only when the value is not itself a
//     JSON text and the key is in bd's key grammar. The SQL backends compare
//     JSON_UNQUOTE(JSON_EXTRACT(metadata, '$."key"')) with the value, which is
//     the Go string comparison for a stored JSON string; for a value that is a
//     JSON literal (true, 42, null, an object) the two can disagree, and a key
//     outside the grammar is refused by the backend rather than matched.
//   - --exclude-type (bd expands type aliases), --exclude-label (bd normalizes
//     labels) and bare-key --metadata-field are never pushed.
//
// # The limit is pushed only where it is the same top N
//
// Under the canonical order and --sort oldest, a store that can cut its ready
// set in that order after every readiness veto (beads.OrderedReadyReader) is
// asked for its first Limit+boundedSlack rows instead of all of them. Every leg
// that returned fewer rows than it was asked for returned all of them, and a
// leg whose rows the Go-only predicates thin below the limit is asked again for
// more (boundedRead), so each leg's top N is in hand and the merged top N is
// the city's.
//
// --sort newest is never pushed: no store cuts in that order.
//
// # Co-resident ids
//
// Both kinds of narrowing rest on one more fact: no id is held by two legs.
// Work legs mint disjoint id namespaces and nothing copies a work bead from one
// to another, and an id in a class binding's reserved namespace belongs to the
// binding alone. The exception is the class binding's work-shaped ids: `gc
// storage migrate` copies beads into the binding under their original ids and
// never deletes the work store's copy. An unbounded read keeps the work store's
// copy (first leg wins) and the predicates judge THAT copy; a narrowed read can
// leave it behind and serve the binding's. So when the binding serves a row of
// the answer under a work-shaped id, the earlier legs are asked whether they
// hold it, and if one does, or cannot say, the answer is read again without any
// narrowing (readNarrowed). On a migrated city that is the relics alone; the
// binding's own rows, every routed graph step included, need no check.

// boundedSlack is how many rows past the limit a bounded leg read asks for.
// It absorbs the rows a Go-only predicate drops without a second read, and any
// difference between a store's order and the Go order at the cut, which can only
// be a tie broken differently.
const boundedSlack = 20

// metadataKeyGrammar is bd's metadata key grammar (storage.ValidateMetadataKey).
// A key outside it is refused by the backend instead of matched, so it is not
// pushed.
var metadataKeyGrammar = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_./]*$`)

// pushableMetadata reports whether f can be pushed into a store's ready read
// with the Go comparison's semantics (see the package comment above).
func pushableMetadata(f metadataFieldFilter) bool {
	return !f.present && metadataKeyGrammar.MatchString(f.key) && !json.Valid([]byte(f.value))
}

// readyReadQuery is the ready query every leg is asked: the federated tier and
// the pushable predicates.
func readyReadQuery(c compiled) beads.ReadyQuery {
	q := beads.ReadyQuery{
		Assignee:   c.assignee,
		TierMode:   beads.FederatedReadTier,
		Unassigned: c.query.Unassigned,
	}
	for _, f := range c.metadata {
		if !pushableMetadata(f) {
			continue
		}
		if q.MetadataEquals == nil {
			q.MetadataEquals = make(map[string]string, len(c.metadata))
		}
		// Two values for one key match nothing in Go; pushing either one still
		// returns a superset of nothing.
		q.MetadataEquals[f.key] = f.value
	}
	return q
}

// boundedOrder reports the order a bounded leg read cuts in, or false when the
// limit is not pushed at all.
func boundedOrder(c compiled) (beads.ReadyOrder, bool) {
	if c.query.Limit <= 0 {
		return 0, false
	}
	switch c.sort {
	case "":
		return beads.ReadyOrderCanonical, true
	case SortOldest:
		return beads.ReadyOrderOldest, true
	default:
		return 0, false
	}
}

// readyRead reads one leg's whole ready set for q, live.
func readyRead(q beads.ReadyQuery) legRead {
	return func(leg Leg) legResult {
		rows, err := beads.HandlesFor(leg.Store).Live.Ready(q)
		return legResult{rows: rows, err: err}
	}
}

// readReadyCandidates reads the ready arm: each leg asked with the pushed
// predicates and, where it is exact, a pushed limit.
func readReadyCandidates(legs []Leg, c compiled) ([]beads.Bead, map[string]int, error) {
	plain := readyRead(beads.ReadyQuery{TierMode: beads.FederatedReadTier})
	q := readyReadQuery(c)
	order, bounded := boundedOrder(c)
	switch {
	case bounded:
		return readNarrowed(legs, c, boundedRead(c, q, order), plain)
	case predicatesPushed(q):
		return readNarrowed(legs, c, readyRead(q), plain)
	default:
		return federate(legs, plain)
	}
}

// predicatesPushed reports whether q asks a leg for less than its whole ready
// set.
func predicatesPushed(q beads.ReadyQuery) bool {
	return q.Assignee != "" || q.Unassigned || len(q.MetadataEquals) > 0
}

// readNarrowed reads every leg with narrowed, a read that asks each leg for
// less than plain does, and answers from it when that answer is provably the
// one plain gives; otherwise it reads every leg with plain.
//
// The only way a narrowed read can answer differently is through first-leg-wins
// dedupe over an id two legs hold: a migrated bead's copy in the work store it
// was minted in, and its copy in the class binding it migrated to. An unbounded
// plain read keeps the work store's copy, and the predicates then judge THAT
// copy. A narrowed read can leave the work store's copy behind (a predicate it
// fails, or a bound it falls past) and serve the binding's instead. So when the
// binding serves a row of the answer under a work-shaped id, every earlier leg
// that was narrowed is asked whether it holds that id; if one does, or cannot
// say, the answer is read again with plain.
func readNarrowed(legs []Leg, c compiled, narrowed, plain legRead) ([]beads.Bead, map[string]int, error) {
	results := readLegs(legs, narrowed)
	if err := firstLegError(legs, results); err != nil {
		return nil, nil, err
	}
	items, owners := merge(results)
	if narrowedAnswerIsExact(legs, c, items, owners) {
		return items, owners, nil
	}
	return federate(legs, plain)
}

// boundedRead asks one leg for its first Limit+boundedSlack ready rows in order,
// and asks again for more while the rows the Go-only predicates keep fall short
// of the limit. A store that cannot cut in order returns its whole set.
func boundedRead(c compiled, q beads.ReadyQuery, order beads.ReadyOrder) legRead {
	limit := c.query.Limit
	return func(leg Leg) legResult {
		want := limit + boundedSlack
		for {
			bq := q
			bq.Limit = want
			rows, ordered, err := beads.ReadyOrdered(leg.Store, bq, order)
			if err != nil {
				return legResult{err: err}
			}
			if !ordered || len(rows) < want {
				return legResult{rows: rows}
			}
			kept := 0
			for _, b := range rows {
				if c.matches(b) {
					kept++
				}
			}
			if kept >= limit {
				return legResult{rows: rows}
			}
			want *= 4
		}
	}
}

// narrowedAnswerIsExact reports whether a narrowed read's answer is the plain
// read's: true unless a row the answer takes from a class binding, under an id
// outside the binding's reserved namespaces, is also held by an earlier leg. A
// leg that cannot say whether it holds the id counts as holding it.
func narrowedAnswerIsExact(legs []Leg, c compiled, items []beads.Bead, owners map[string]int) bool {
	answer := c.filter(items)
	c.order(answer)
	if c.query.Limit > 0 && len(answer) > c.query.Limit {
		answer = answer[:c.query.Limit]
	}
	type probe struct {
		leg int
		id  string
	}
	var probes []probe
	for _, row := range answer {
		src := owners[row.ID]
		if !legs[src].ClassBinding || inAnyNamespace(row.ID, legs[src].ReservedPrefixes) {
			continue
		}
		for i := range src {
			probes = append(probes, probe{leg: i, id: row.ID})
		}
	}
	held := make([]bool, len(probes))
	fanout.Bounded(len(probes), LegFanOut, func(i int) {
		_, err := beads.HandlesFor(legs[probes[i].leg].Store).Live.Get(probes[i].id)
		held[i] = !errors.Is(err, beads.ErrNotFound)
	})
	return !slices.Contains(held, true)
}

// inAnyNamespace reports whether id is in one of the prefixes' namespaces: the
// prefix itself, or the prefix followed by "-" (storeref.IDInNamespace).
func inAnyNamespace(id string, prefixes []string) bool {
	for _, p := range prefixes {
		if storeref.IDInNamespace(id, p) {
			return true
		}
	}
	return false
}
