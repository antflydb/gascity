package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// requireReadySessions skips where the platform has no abstract unix sockets:
// there `gc hook` runs every work query without a session, which the
// unsupported-platform test pins instead.
func requireReadySessions(t *testing.T) {
	t.Helper()
	if !readySessionsSupported() {
		t.Skipf("ready sessions need abstract unix sockets, which %s does not have", runtime.GOOS)
	}
}

// startTestReadySession starts a session for the ambient city and points this
// process's `gc ready` at it, the way the wrapper points a work query's.
func startTestReadySession(t *testing.T) *readySession {
	t.Helper()
	requireReadySessions(t)
	s, err := startReadySession(io.Discard)
	if err != nil {
		t.Fatalf("startReadySession: %v", err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Errorf("closing the ready session: %v", err)
		}
	})
	for _, kv := range s.env() {
		key, value, _ := strings.Cut(kv, "=")
		t.Setenv(key, value)
	}
	return s
}

// countReadyRigOpens swaps the rig-leg opener for one that counts opens per
// rig, optionally wrapping each opened store.
func countReadyRigOpens(t *testing.T, wrap func(rig string, store beads.Store) beads.Store) func(rig string) int {
	t.Helper()
	var mu sync.Mutex
	opens := map[string]int{}
	prev := readyRigStoreOpener
	readyRigStoreOpener = func(cfg *config.City) rigStoreOpener {
		inner := prev(cfg)
		return func(rigPath, cityPath string) (beads.Store, error) {
			rig := rigPathBase(rigPath)
			mu.Lock()
			opens[rig]++
			mu.Unlock()
			store, err := inner(rigPath, cityPath)
			if err != nil || wrap == nil {
				return store, err
			}
			return wrap(rig, store), nil
		}
	}
	t.Cleanup(func() { readyRigStoreOpener = prev })
	return func(rig string) int {
		mu.Lock()
		defer mu.Unlock()
		return opens[rig]
	}
}

func rigPathBase(rigPath string) string {
	rigPath = strings.TrimRight(rigPath, "/")
	if i := strings.LastIndex(rigPath, "/"); i >= 0 {
		return rigPath[i+1:]
	}
	return rigPath
}

// readyAnswer is what a work query reads off `gc ready`: the exit code, the
// array on stdout, and the "gc ready: ..." line a failure prints. Other
// stderr — config-load warnings — is not part of the answer: in-process each
// call prints its own, and under a session the session prints them once.
type readyAnswer struct {
	code   int
	stdout string
	errors string
	stderr string
}

func askReady(opts readyOpts) readyAnswer {
	var stdout, stderr bytes.Buffer
	code := cmdReady(opts, &stdout, &stderr)
	var errLines []string
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.HasPrefix(line, "gc ready: ") {
			errLines = append(errLines, line)
		}
	}
	return readyAnswer{code: code, stdout: stdout.String(), errors: strings.Join(errLines, "\n"), stderr: stderr.String()}
}

// claimQuestions are the questions a fresh seat's generated work query asks
// `gc ready`, in order, for a seat with two identities and routed work waiting:
// in-progress work for each identity, assigned work for each identity, then the
// routed pool tier.
func claimQuestions() []readyOpts {
	poolTier := readyOpts{
		metadataFields: []string{beadmeta.RoutedToMetadataKey + "=worker"},
		unassigned:     true,
		excludeTypes:   []string{"epic"},
		excludeLabels:  []string{"hold:mayor", "hold:external", "human"},
		limit:          20,
	}
	return []readyOpts{
		{status: readyStatusInProgress, assignee: "gc-session-1", limit: 1},
		{status: readyStatusInProgress, assignee: "worker-1", limit: 1},
		{assignee: "gc-session-1", limit: 1},
		{assignee: "worker-1", limit: 1},
		poolTier,
	}
}

// seedReadySessionCity puts routed, assigned and blocked work in the city
// store and in every rig of the fixture city, so every arm has rows to read.
func seedReadySessionCity(t *testing.T, cityDir string) {
	t.Helper()
	scopes := []string{cityDir}
	for _, name := range readyCountedRigNames {
		scopes = append(scopes, cityDir+"/rigs/"+name)
	}
	for _, scope := range scopes {
		store, err := openStoreAtForCity(scope, cityDir)
		if err != nil {
			t.Fatalf("open store at %s: %v", scope, err)
		}
		mustCreateReadyBead(t, store, beads.Bead{Title: "routed", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
		blocker := mustCreateReadyBead(t, store, beads.Bead{Title: "blocker", Type: "task"})
		claimed := mustCreateReadyBead(t, store, beads.Bead{Title: "claimed", Type: "task"})
		inProgress, owner := readyStatusInProgress, "worker-1"
		if err := store.Update(claimed.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &owner}); err != nil {
			t.Fatalf("claim %s: %v", claimed.ID, err)
		}
		if err := store.DepAdd(claimed.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("block %s on %s: %v", claimed.ID, blocker.ID, err)
		}
	}
}

// TestReadySessionAnswersExactlyAsTheInProcessReader is the equivalence the
// whole mechanism rests on: for every arm a work query asks — the claim's
// tiers, the --status arms the workflows pack inventories with, bounds and
// orders, and a malformed question — the session's answer is the in-process
// reader's, byte for byte, with the same exit code and the same error line.
func TestReadySessionAnswersExactlyAsTheInProcessReader(t *testing.T) {
	requireReadySessions(t)
	cityDir := newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	seedReadySessionCity(t, cityDir)

	questions := append(claimQuestions(),
		readyOpts{},
		readyOpts{status: readyStatusInProgress},
		readyOpts{status: readyStatusOpen},
		readyOpts{status: "blocked"},
		readyOpts{sortOrder: readySortNewest, limit: 2},
		readyOpts{status: "not-a-status"},
	)
	want := make([]readyAnswer, len(questions))
	for i, q := range questions {
		want[i] = askReady(q)
	}
	if want[0].code != 0 || want[len(want)-1].code == 0 {
		t.Fatalf("fixture: in-process answers %+v; want the first to succeed and the malformed one to fail", want)
	}

	startTestReadySession(t)
	for i, q := range questions {
		got := askReady(q)
		if got.code != want[i].code || got.stdout != want[i].stdout || got.errors != want[i].errors {
			t.Fatalf("question %d (%+v) through the session = %+v, want the in-process answer %+v", i, q, got, want[i])
		}
	}
}

// TestReadySessionOpensTheStoresOnceForAClaim is the claim path's call count.
// A fresh seat's work query asks five questions; each used to be a separate
// process that opened every serving rig store again. Through one session each
// rig store is opened once for the whole claim.
func TestReadySessionOpensTheStoresOnceForAClaim(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	opens := countReadyRigOpens(t, nil)
	questions := claimQuestions()

	for _, q := range questions {
		if a := askReady(q); a.code != 0 {
			t.Fatalf("in-process %+v = %+v", q, a)
		}
	}
	for _, rig := range readyCountedRigNames {
		if got := opens(rig); got != len(questions) {
			t.Fatalf("without a session, rig %q opened %d times over %d questions, want once per question", rig, got, len(questions))
		}
	}

	startTestReadySession(t)
	for _, q := range questions {
		if a := askReady(q); a.code != 0 {
			t.Fatalf("through the session %+v = %+v", q, a)
		}
	}
	for _, rig := range readyCountedRigNames {
		if got := opens(rig) - len(questions); got != 1 {
			t.Fatalf("through one session, rig %q opened %d times over a claim's %d questions, want exactly once", rig, got, len(questions))
		}
	}
}

// readyFailOnceStore fails its next Ready read while fail is set, the way a
// leg hits a transient lock or a dropped connection.
type readyFailOnceStore struct {
	beads.Store
	fail *atomic.Bool
}

func (s readyFailOnceStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	if s.fail.CompareAndSwap(true, false) {
		return nil, errors.New("database is locked")
	}
	return s.Store.Ready(query...)
}

// TestReadySessionReopensAfterAFailedRead: a failed read is answered exactly
// like an in-process failure, and the session then drops its legs, so the next
// question opens fresh stores — what the next `gc ready` process would have
// done — instead of reading a leg that just failed.
func TestReadySessionReopensAfterAFailedRead(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	var fail atomic.Bool
	opens := countReadyRigOpens(t, func(rig string, store beads.Store) beads.Store {
		if rig == readyCountedRigNames[1] {
			return readyFailOnceStore{Store: store, fail: &fail}
		}
		return store
	})
	startTestReadySession(t)

	fail.Store(true)
	failed := askReady(readyOpts{})
	if failed.code == 0 || failed.stdout != "" || !strings.Contains(failed.stderr, "gc ready: rig "+readyCountedRigNames[1]+" store: database is locked") {
		t.Fatalf("a failed leg read through the session = %+v, want the in-process failure: exit 1, no stdout, the leg named on stderr", failed)
	}
	if ok := askReady(readyOpts{}); ok.code != 0 {
		t.Fatalf("the question after the failure = %+v, want a fresh successful read", ok)
	}
	if got := opens(readyCountedRigNames[1]); got != 2 {
		t.Fatalf("the failed rig was opened %d times, want twice: once for the failed read and once fresh after it", got)
	}
}

// TestReadySessionServesOnlyItsOwnCity: a `gc ready` that resolved another
// city (a --city flag, a cd into another city) is declined and reads
// in-process, so a session can never answer for stores it does not hold.
func TestReadySessionServesOnlyItsOwnCity(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	s := startTestReadySession(t)

	resp := s.answer(readySessionRequest{Token: s.token, CityPath: s.cityPath + "-elsewhere"})
	if resp.Served {
		t.Fatalf("the session served a question for another city: %+v", resp)
	}
	resp = s.answer(readySessionRequest{Token: "not-the-token", CityPath: s.cityPath})
	if resp.Served {
		t.Fatalf("the session served a question carrying the wrong token: %+v", resp)
	}
	if resp = s.answer(readySessionRequest{Token: s.token, CityPath: s.cityPath}); !resp.Served {
		t.Fatalf("the session declined a question for its own city with its own token: %+v", resp)
	}
}

// TestReadyReadsInProcessWhenTheSessionIsUnreachable: a session that is gone
// is never an outage. The reader says so on stderr and answers in-process.
func TestReadyReadsInProcessWhenTheSessionIsUnreachable(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	want := askReady(readyOpts{})

	t.Setenv(readySessionAddrEnv, "@gc-ready-session-gone")
	t.Setenv(readySessionTokenEnv, "token")
	got := askReady(readyOpts{})
	if got.code != 0 || got.stdout != want.stdout {
		t.Fatalf("with the session gone, gc ready = %+v, want the in-process answer %q", got, want.stdout)
	}
	if !strings.Contains(got.stderr, "ready session") {
		t.Fatalf("stderr = %q, want it to say the session was unreachable and the read went in-process", got.stderr)
	}
}

// TestRunReadySessionRunsTheQueryInsideOneSession pins the wrapper: the query
// runs with the session's address in its environment, its stdout and stderr
// pass through untouched, and its exit status is the wrapper's.
func TestRunReadySessionRunsTheQueryInsideOneSession(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	var stdout, stderr bytes.Buffer
	code := runReadySession([]string{"sh", "-c", `printf '%s' "$` + readySessionAddrEnv + `"; printf 'query stderr' >&2; exit 3`}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("wrapper exit = %d, want the query's own exit status 3", code)
	}
	if !strings.HasPrefix(stdout.String(), "@gc-ready-session-") {
		t.Fatalf("the query saw %s=%q, want the session's abstract address", readySessionAddrEnv, stdout.String())
	}
	if stderr.String() != "query stderr" {
		t.Fatalf("stderr = %q, want the query's own stderr and nothing else", stderr.String())
	}
}

// TestRunReadySessionRunsTheQueryWhenNoSessionCanStart: the session is an
// optimization, so failing to start one never fails the query. It runs as it
// would have without the wrapper — no session in its environment, its own exit
// status — and the wrapper says on stderr why there was no session.
func TestRunReadySessionRunsTheQueryWhenNoSessionCanStart(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	prev := readySessionListen
	readySessionListen = func() (net.Listener, string, error) { return nil, "", errors.New("no socket for you") }
	t.Cleanup(func() { readySessionListen = prev })

	var stdout, stderr bytes.Buffer
	code := runReadySession([]string{"sh", "-c", `printf '[%s]' "$` + readySessionAddrEnv + `"; exit 4`}, &stdout, &stderr)
	if code != 4 || stdout.String() != "[]" {
		t.Fatalf("wrapper = %d with stdout %q, want the query run without a session (empty %s) and its exit status 4", code, stdout.String(), readySessionAddrEnv)
	}
	if !strings.Contains(stderr.String(), "no socket for you") {
		t.Fatalf("stderr = %q, want the reason there was no session", stderr.String())
	}
}

// TestHookRunsTheGeneratedFederatedQueryInsideAReadySession pins where the
// session is used: only for the generated work query of a split city, the one
// that asks `gc ready` several questions. A single-store city's query and an
// operator's own work_query run exactly as before.
func TestHookRunsTheGeneratedFederatedQueryInsideAReadySession(t *testing.T) {
	federated := config.QueryTopology{FederatedReady: true}
	if got := hookUsesReadySession(&config.Agent{Name: "worker"}, federated); got != readySessionsSupported() {
		t.Fatalf("generated query on a split city uses a ready session = %v, want %v (whether this platform can run one)", got, readySessionsSupported())
	}
	if hookUsesReadySession(&config.Agent{Name: "worker"}, config.QueryTopology{}) {
		t.Fatal("a single-store city's generated query asks bd, not gc ready, and must run without a session")
	}
	if hookUsesReadySession(&config.Agent{Name: "worker", WorkQuery: "bd ready --json"}, federated) {
		t.Fatal("an operator's own work_query must run exactly as written")
	}
}

// TestShellWorkQueryRunsInsideAReadySessionWhenAsked pins the runner half
// end to end: with the request in the query's environment the query runs
// under `gc internal ready-session` — this binary, re-executed as gc — and sees
// a live session; without it, it is the bare sh it always was.
func TestShellWorkQueryRunsInsideAReadySessionWhenAsked(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	query := `printf '[%s]' "$` + readySessionAddrEnv + `"`

	plain, err := shellWorkQueryWithEnv(query, "", os.Environ())
	if err != nil || plain != "[]" {
		t.Fatalf("without the request the query printed %q (err %v), want no session: []", plain, err)
	}
	asked, err := shellWorkQueryWithEnv(query, "", append(os.Environ(), readySessionRequestEnv+"=1"))
	if err != nil {
		t.Fatalf("shellWorkQueryWithEnv with the request: %v", err)
	}
	if !strings.HasPrefix(asked, "[@gc-ready-session-") {
		t.Fatalf("with the request the query printed %q, want the address of the ready session it runs in", asked)
	}
}

// TestReadySessionRequestRidesEveryStoresOwnEnv pins where the request lives:
// in each store's env, the env its query runs with. A rig-scoped agent's
// primary leg runs with the rig's env, rebuilt from the rig's own query env
// plus the identity keys, so a request set only on the hook's env never
// reaches the query — which is the exact miss this guards.
func TestReadySessionRequestRidesEveryStoresOwnEnv(t *testing.T) {
	rigEnv := []string{"GC_RIG=alpha", "PATH=/bin"}
	cityEnv := []string{"GC_RIG=", "PATH=/bin", readySessionRequestEnv + "=0"}
	stores := []hookStore{{dir: "/rigs/alpha", env: rigEnv}, {dir: "/city", env: cityEnv}}

	got := withReadySessionRequest(stores)
	for i, st := range got {
		if hookClaimEnvValue(st.env, readySessionRequestEnv) != "1" {
			t.Fatalf("store %d (%s) env = %v, want the ready-session request in it", i, st.dir, st.env)
		}
		if n := strings.Count(strings.Join(st.env, "\n"), readySessionRequestEnv+"="); n != 1 {
			t.Fatalf("store %d env carries the request %d times, want once: %v", i, n, st.env)
		}
	}
	if len(rigEnv) != 2 || len(cityEnv) != 3 || stores[0].env[0] != "GC_RIG=alpha" {
		t.Fatalf("withReadySessionRequest wrote through to the caller's store envs: %v / %v", rigEnv, cityEnv)
	}
}

// TestReadySessionReadsFreshForEveryQuestion: the session keeps the opened
// stores, never their answers. Work created between two questions — another
// agent routing a bead mid-claim — is in the second answer, exactly as a
// second `gc ready` process would have seen it.
func TestReadySessionReadsFreshForEveryQuestion(t *testing.T) {
	requireReadySessions(t)
	cityDir := newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	opens := countReadyRigOpens(t, nil)
	startTestReadySession(t)

	if first := askReady(readyOpts{}); first.code != 0 || strings.Contains(first.stdout, "created mid-claim") {
		t.Fatalf("first question = %+v, want an answer without the bead not yet created", first)
	}
	rigStore, err := openStoreAtForCity(cityDir+"/rigs/"+readyCountedRigNames[1], cityDir)
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	created := mustCreateReadyBead(t, rigStore, beads.Bead{Title: "created mid-claim", Type: "task"})

	second := askReady(readyOpts{})
	if second.code != 0 || !strings.Contains(second.stdout, created.ID) {
		t.Fatalf("second question = %+v, want the bead %s created after the first answer", second, created.ID)
	}
	if got := opens(readyCountedRigNames[1]); got != 1 {
		t.Fatalf("rig opened %d times across both questions, want once: fresh reads, reused stores", got)
	}
}

// TestReadySessionWireCarriesEveryReadyFlag: a flag the session's wire did
// not carry would be dropped in transit, and the session would answer a
// different question than the one asked — silently, with exit 0. Every
// readyOpts field must survive the trip through the JSON request, so a field
// added to readyOpts later fails here until the wire carries it.
func TestReadySessionWireCarriesEveryReadyFlag(t *testing.T) {
	opts := readyOpts{
		assignee:       "worker-1",
		unassigned:     true,
		metadataFields: []string{beadmeta.RoutedToMetadataKey + "=worker", "gc.kind"},
		excludeTypes:   []string{"epic"},
		excludeLabels:  []string{"human", "hold:mayor"},
		sortOrder:      readySortOldest,
		limit:          7,
		status:         readyStatusOpen,
	}
	fields := reflect.ValueOf(opts)
	for i := range fields.NumField() {
		if fields.Field(i).IsZero() {
			t.Fatalf("readyOpts.%s is zero in this fixture; set it so the round trip below proves the session wire carries it", fields.Type().Field(i).Name)
		}
	}

	encoded, err := json.Marshal(readySessionRequestFor("token", "/city", opts))
	if err != nil {
		t.Fatalf("encoding the request: %v", err)
	}
	var decoded readySessionRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decoding the request: %v", err)
	}
	if got := decoded.readyOpts(); !reflect.DeepEqual(got, opts) {
		t.Fatalf("flags after the session wire = %+v, want %+v", got, opts)
	}
}
