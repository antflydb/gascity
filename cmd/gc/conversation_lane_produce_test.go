package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The CLI's lane producers (design slice 7): gc session submit's local
// fallback, mail --notify, sling --nudge and the hook-claim continuation.

// laneOpByKey reads the op principal's key names on the fixture's session.
func (c *cliLaneCity) laneOpByKey(t *testing.T, principal, key string) opjournal.Op {
	t.Helper()
	st := c.laneStore(t)
	id, err := opjournal.DeriveID(st.Prefix(), c.id, principal, key)
	if err != nil {
		t.Fatal(err)
	}
	return c.get(t, st, id)
}

// assertLaneNotice checks op is a lane notice from principal, of policy
// wake, whose delivered text is golden and names no identifier.
func assertLaneNotice(t *testing.T, op opjournal.Op, principal string, wake opjournal.Wake, golden string) {
	t.Helper()
	m := op.Policy.Message
	if op.Executor != opjournal.ExecutorLane || op.Producer.Principal != principal || m.Role != opjournal.RoleNotice ||
		m.When != opjournal.WhenBoundary || m.Wake != wake {
		t.Fatalf("op = %+v, want a lane notice from %s waking %s", op, principal, wake)
	}
	text, err := opjournal.RenderDelivery([]opjournal.Op{op}, opjournal.TextReminder, false)
	if err != nil {
		t.Fatal(err)
	}
	if text != golden {
		t.Fatalf("delivered text = %q, want %q", text, golden)
	}
	for _, id := range []string{op.ID, op.ClientKey, op.Conversation, op.Causation} {
		if id != "" && strings.Contains(text, id) {
			t.Fatalf("delivered text %q carries identifier %q", text, id)
		}
	}
}

// reminderGolden is the delivered text of one notice labeled label.
func reminderGolden(label, text string) string {
	return "<system-reminder>\nYou have a deferred reminder that was queued until a safe boundary:\n\n- [" + label + "] " + text + "\n\nHandle them after this turn.\n</system-reminder>\n"
}

func (c *cliLaneCity) submitLane(t *testing.T, intent session.SubmitIntent, key string) (int, string, string) {
	t.Helper()
	return c.submitLaneOver(t, c.store, intent, key, cliLaneTestMessage, false)
}

// submitLaneOver runs gc session submit's lane fallback of message under key
// over store, asJSON as --json.
func (c *cliLaneCity) submitLaneOver(t *testing.T, store beads.Store, intent session.SubmitIntent, key, message string, asJSON bool) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, handled := submitSessionLane(sessionSubmitLane{
		cityPath: c.dir, cfg: c.cfg, store: store, sessionID: c.id, target: c.target.agentKey(),
		message: message, intent: intent, key: key, asJSON: asJSON,
		stdout: &stdout, stderr: &stderr, sp: c.sp,
	})
	if !handled {
		t.Fatal("submitSessionLane did not handle a session a lane serves")
	}
	return code, stdout.String(), stderr.String()
}

// proposeLegacySubmit records the op gc session submit's legacy path records
// for intent under key: legacy:cli, under the operator, with
// api.MessageOpPolicy. outcome closes it; nil leaves it open. It returns the
// op as stored.
func (c *cliLaneCity) proposeLegacySubmit(t *testing.T, intent session.SubmitIntent, key string, outcome *opjournal.Outcome) opjournal.Op {
	t.Helper()
	st := c.laneStore(t)
	op, _, err := st.Propose(opjournal.Request{
		Conversation: c.id, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLegacyCLI, ClientKey: key,
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:  cliLaneTestMessage, Policy: api.MessageOpPolicy(intent),
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if outcome != nil {
		if _, err := st.Close(op.ID, *outcome, string(opjournal.ExecutorLegacyCLI)); err != nil {
			t.Fatalf("closing: %v", err)
		}
	}
	return c.get(t, st, op.ID)
}

// The fallback proposes the op the API would: the operator's message under
// the request's key with api.LaneMessageOpPolicy, so its op id is the one the
// API derives, and a boundary prompt is delivered by the inline pass.
func TestSessionSubmitFallbackProposesTheAPIsLaneOp(t *testing.T) {
	c := newCLILaneCity(t)
	code, stdout, stderr := c.submitLane(t, session.SubmitIntentDefault, "submit-1")
	if code != 0 || stdout != "Submitted to "+c.target.agentKey()+"\n" {
		t.Fatalf("submit = %d, %q; stderr %s", code, stdout, stderr)
	}
	op := c.laneOpByKey(t, "operator", "submit-1")
	want := api.LaneMessageOpPolicy(session.SubmitIntentDefault)
	m := op.Policy.Message
	if op.Executor != opjournal.ExecutorLane || m.When != want.Message.When || m.Wake != want.Message.Wake || m.Role != want.Message.Role || op.Payload != cliLaneTestMessage ||
		op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v, want the API's lane op delivered", op)
	}
	if n := len(c.nudges()); n != 1 {
		t.Fatalf("deliveries = %d, want the inline pass's one", n)
	}
	// The same request again replays the op.
	code, stdout, stderr = c.submitLane(t, session.SubmitIntentDefault, "submit-1")
	if code != 0 || !strings.Contains(stderr, "replayed op "+op.ID) || len(c.nudges()) != 1 {
		t.Fatalf("replay = %d, %q, %q; deliveries %d", code, stdout, stderr, len(c.nudges()))
	}
}

// interrupt_now is a deliver-now unit, which is control: the CLI's pass
// leaves it to the controller's lane host, interrupting nothing.
func TestSessionSubmitFallbackInterruptNowWaitsForTheController(t *testing.T) {
	c := newCLILaneCity(t)
	c.sp.Calls = nil
	code, stdout, stderr := c.submitLane(t, session.SubmitIntentInterruptNow, "submit-now")
	if code != 0 || stdout != "Queued follow-up for "+c.target.agentKey()+"\n" {
		t.Fatalf("submit = %d, %q; stderr %s", code, stdout, stderr)
	}
	if calls := c.controlCalls(); len(calls) != 0 || len(c.nudges()) != 0 {
		t.Fatalf("the CLI interrupted or delivered: %+v / %+v", calls, c.nudges())
	}
	op := c.laneOpByKey(t, "operator", "submit-now")
	if op.Status == opjournal.StatusClosed || op.Policy.Message.When != opjournal.WhenNow {
		t.Fatalf("op = %+v, want an open deliver-now message", op)
	}
}

// gc session submit's legacy path records its op under the operator
// principal, the conversation and the key the fallback proposes with, so a
// retry that reaches the fallback finds it, and the legacy and lane policies
// differ for follow_up and interrupt_now. The lane never takes, closes or
// rewrites that op: an open one is in progress under its legacy path, a
// closed one replays its outcome (queued when the legacy path only queued
// the message), and one that closed without success is refused.
func TestSessionSubmitFallbackNeverTakesALegacyOp(t *testing.T) {
	delivered := map[session.SubmitIntent]string{
		session.SubmitIntentDefault:      "Submitted to ",
		session.SubmitIntentFollowUp:     "Submitted follow-up to ",
		session.SubmitIntentInterruptNow: "Interrupted and submitted to ",
	}
	for _, intent := range []session.SubmitIntent{session.SubmitIntentDefault, session.SubmitIntentFollowUp, session.SubmitIntentInterruptNow} {
		for _, tc := range []struct {
			name    string
			outcome *opjournal.Outcome
			code    int
			stdout  string
			stderr  string
		}{
			{name: "open", code: 1, stderr: " is in progress under its legacy path; rerun later or without --key (key k-legacy)"},
			{name: "closed queued", outcome: &opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{Via: legacyop.ViaLegacyQueued}}, stdout: "Queued follow-up for ", stderr: "replayed op "},
			{name: "closed delivered", outcome: &opjournal.Outcome{Arm: opjournal.ArmSucceeded}, stdout: delivered[intent], stderr: "replayed op "},
			{name: "closed failed", outcome: &opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonExpiredBeforeStart}, code: 1, stderr: " closed failed{expired_before_start}; rerun without --key to act again (key k-legacy)"},
		} {
			t.Run(string(intent)+"/"+tc.name, func(t *testing.T) {
				c := newCLILaneCity(t)
				c.sp.Calls = nil
				legacy := c.proposeLegacySubmit(t, intent, "k-legacy", tc.outcome)
				code, stdout, stderr := c.submitLane(t, intent, "k-legacy")
				if code != tc.code || (tc.stdout != "" && stdout != tc.stdout+c.target.agentKey()+"\n") || (tc.stdout == "" && stdout != "") ||
					!strings.Contains(stderr, "op "+legacy.ID) || !strings.Contains(stderr, tc.stderr) {
					t.Fatalf("submit = %d, %q, %q; want %d, %q, %q", code, stdout, stderr, tc.code, tc.stdout, tc.stderr)
				}
				if got := c.only(t); got.Executor != opjournal.ExecutorLegacyCLI || got.Status != legacy.Status || got.Revision != legacy.Revision {
					t.Fatalf("the legacy op was taken or rewritten: %+v, was %+v", got, legacy)
				}
				if len(c.nudges()) != 0 || len(c.controlCalls()) != 0 || len(c.rings) != 0 {
					t.Fatalf("the fallback delivered %+v, controlled %+v or rang %v for a legacy op", c.nudges(), c.controlCalls(), c.rings)
				}
			})
		}
	}
}

// Under --json every refusal of the fallback writes the failure envelope
// (schemas/failure.schema.json) with its code, as the legacy path's
// refusals do, and exits 1.
func TestSessionSubmitFallbackRefusalsWriteTheFailureEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name string
		// run sets the case up and submits under --json.
		run    func(t *testing.T, c *cliLaneCity) (int, string, string)
		want   string
		stderr string
	}{
		{
			name: "the op journal cannot record it", want: cliOpCodeUnavailable, stderr: "the message did not run (key k-json)",
			run: func(t *testing.T, c *cliLaneCity) (int, string, string) {
				return c.submitLaneOver(t, noBatchStore{c.store}, session.SubmitIntentDefault, "k-json", cliLaneTestMessage, true)
			},
		},
		{
			name: "the key names a different request", want: cliOpCodeKeyMismatch, stderr: "the key names a different request (op ",
			run: func(t *testing.T, c *cliLaneCity) (int, string, string) {
				if code, _, stderr := c.submitLane(t, session.SubmitIntentDefault, "k-json"); code != 0 {
					t.Fatalf("first submit = %d; %s", code, stderr)
				}
				return c.submitLaneOver(t, c.store, session.SubmitIntentDefault, "k-json", "something else", true)
			},
		},
		{
			name: "this run's op closed without success", want: cliOpCodeFailed, stderr: " closed skipped{conversation_closed} (key k-json)",
			run: func(t *testing.T, c *cliLaneCity) (int, string, string) {
				if err := c.store.Close(c.id); err != nil {
					t.Fatalf("closing the session row: %v", err)
				}
				return c.submitLaneOver(t, c.store, session.SubmitIntentDefault, "k-json", cliLaneTestMessage, true)
			},
		},
		{
			name: "the op closed without success before", want: cliOpCodeAlreadyFailed, stderr: "; rerun without --key to act again (key k-json)",
			run: func(t *testing.T, c *cliLaneCity) (int, string, string) {
				if code, _, stderr := c.submitLane(t, session.SubmitIntentInterruptNow, "k-json"); code != 0 {
					t.Fatalf("first submit = %d; %s", code, stderr)
				}
				op := c.laneOpByKey(t, "operator", "k-json")
				if _, err := c.laneStore(t).Close(op.ID, opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonExpiredBeforeStart}, "test"); err != nil {
					t.Fatalf("closing: %v", err)
				}
				return c.submitLaneOver(t, c.store, session.SubmitIntentInterruptNow, "k-json", cliLaneTestMessage, true)
			},
		},
		{
			name: "a legacy op is in progress", want: cliOpCodeInProgress, stderr: " is in progress under its legacy path",
			run: func(t *testing.T, c *cliLaneCity) (int, string, string) {
				c.proposeLegacySubmit(t, session.SubmitIntentDefault, "k-json", nil)
				return c.submitLaneOver(t, c.store, session.SubmitIntentDefault, "k-json", cliLaneTestMessage, true)
			},
		},
		{
			name: "the key is invalid", want: cliOpCodeInvalidKey, stderr: "invalid --key (",
			run: func(t *testing.T, c *cliLaneCity) (int, string, string) {
				return c.submitLaneOver(t, c.store, session.SubmitIntentDefault, " bad-key", cliLaneTestMessage, true)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			code, stdout, stderr := tc.run(t, c)
			if code != 1 || !strings.HasPrefix(stderr, "gc session submit: ") || !strings.Contains(stderr, tc.stderr) {
				t.Fatalf("submit = %d, stderr %q; want 1 and %q", code, stderr, tc.stderr)
			}
			if got := cliOpFailureCode(t, []byte(stdout)); got != tc.want {
				t.Fatalf("failure code %q, want %s; stdout %s", got, tc.want, stdout)
			}
		})
	}
}

// laneOpRereadFailStore fails every read of an op row it holds: the read of
// the op the fallback proposed, after its inline pass.
type laneOpRereadFailStore struct {
	laneTestStore
}

func (s *laneOpRereadFailStore) Get(id string) (beads.Bead, error) {
	b, err := s.laneTestStore.Get(id)
	if err == nil && slices.Contains(b.Labels, opjournal.LabelOp) {
		return beads.Bead{}, errors.New("store hiccup")
	}
	return b, err
}

// A read of the op after the inline pass that fails is warned of, and the
// op is reported as it was proposed: queued for the controller.
func TestSessionSubmitFallbackWarnsOfAFailedReread(t *testing.T) {
	c := newCLILaneCityOver(t, &laneOpRereadFailStore{laneTestStore: newLaneTestStore()})
	code, stdout, stderr := c.submitLane(t, session.SubmitIntentDefault, "k-reread")
	if code != 0 || stdout != "Queued follow-up for "+c.target.agentKey()+"\n" ||
		!strings.Contains(stderr, "gc session submit: warning: reading op ") || !strings.Contains(stderr, " after the inline delivery failed (") || !strings.Contains(stderr, "store hiccup); it waits for the controller") {
		t.Fatalf("submit = %d, %q, stderr %q; want queued and the failed read warned of", code, stdout, stderr)
	}
}

// cmdSubmitCity points gc session submit at the fixture: city.toml lists
// message (cityOnDisk), the command opens the fixture's store and fake
// runtime, and its API client talks to srv (nil: no API).
func (c *cliLaneCity) cmdSubmitCity(t *testing.T, srv *httptest.Server) {
	t.Helper()
	clearGCEnv(t)
	c.cityOnDisk(t)
	prevFactory, prevBuild := openStoreFactoryForCity, buildSessionProviderByName
	prevAlive, prevClient := apiRouteControllerAliveHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		openStoreFactoryForCity, buildSessionProviderByName = prevFactory, prevBuild
		apiRouteControllerAliveHook, apiRouteSupervisorClientHook = prevAlive, prevClient
	})
	openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: c.store}, nil
	}
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return c.sp, nil
	}
	apiRouteControllerAliveHook = func(string) int { return 0 }
	apiRouteSupervisorClientHook = func(string) *api.Client {
		if srv == nil {
			return nil
		}
		return api.NewCityScopedClient(srv.URL, filepath.Base(c.dir))
	}
}

// cmdSubmit runs gc session submit of the test message to the fixture's
// session under key, asJSON as --json.
func (c *cliLaneCity) cmdSubmit(intent session.SubmitIntent, key string, asJSON bool) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cmdSessionSubmit([]string{c.id, cliLaneTestMessage}, intent, key, asJSON, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// proposeLaneSubmit proposes payload under key as the API's acceptMessageLane
// proposes a submit of intent: a lane op from the operator with
// api.LaneMessageOpPolicy. outcome closes it; nil leaves it open. It returns
// the op as stored.
func (c *cliLaneCity) proposeLaneSubmit(t *testing.T, intent session.SubmitIntent, key, payload string, outcome *opjournal.Outcome) opjournal.Op {
	t.Helper()
	st := c.laneStore(t)
	op, _, err := st.Propose(opjournal.Request{
		Conversation: c.id, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane, ClientKey: key,
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:  payload, Policy: api.LaneMessageOpPolicy(intent),
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if outcome != nil {
		if _, err := st.Close(op.ID, *outcome, "test"); err != nil {
			t.Fatalf("closing: %v", err)
		}
	}
	return c.get(t, st, op.ID)
}

// assertNothingDelivered checks the submit neither delivered nor interrupted.
func (c *cliLaneCity) assertNothingDelivered(t *testing.T) {
	t.Helper()
	if n, calls := len(c.nudges()), c.controlCalls(); n != 0 || len(calls) != 0 {
		t.Fatalf("the submit delivered %d times and interrupted %+v, want neither", n, calls)
	}
}

// With message listed, a submit to a session no lane serves the CLI from (a
// protocol-mode session without a host stamp, which the controller's lane
// host serves), whose API answer was lost after the API proposed its lane
// op, answers from that op instead of delivering the message again on the
// legacy path (the fallback dedupe, DES-O0API §15 OJ-5): an open op is the
// API's acceptance, and a closed one replays its outcome or is refused as
// failed, for every intent.
func TestSessionSubmitLaneDeclinedFallbackAnswersTheAPIsOp(t *testing.T) {
	succeeded := opjournal.Outcome{Arm: opjournal.ArmSucceeded}
	failed := opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonExpiredBeforeStart}
	delivered := map[session.SubmitIntent]string{
		session.SubmitIntentDefault:      "Submitted to ",
		session.SubmitIntentFollowUp:     "Submitted follow-up to ",
		session.SubmitIntentInterruptNow: "Interrupted and submitted to ",
	}
	for _, intent := range []session.SubmitIntent{session.SubmitIntentDefault, session.SubmitIntentFollowUp, session.SubmitIntentInterruptNow} {
		for _, tc := range []struct {
			name    string
			outcome *opjournal.Outcome
			code    int
			// stdout is the output for the session, the API's op and the key.
			stdout func(target, opID, key string) string
			stderr string
		}{
			{name: "open", stdout: func(target, opID, key string) string {
				return fmt.Sprintf("Accepted by the controller for %s (op %s); gc session submit --key %s reports its outcome\n", target, opID, key)
			}},
			{name: "closed delivered", outcome: &succeeded, stdout: func(target, _, _ string) string {
				return delivered[intent] + target + "\n"
			}, stderr: "gc session submit: replayed op "},
			{
				name: "closed failed", outcome: &failed, code: 1, stdout: func(string, string, string) string { return "" },
				stderr: " closed failed{expired_before_start}; rerun without --key to act again (key ",
			},
		} {
			t.Run(string(intent)+"/"+tc.name, func(t *testing.T) {
				c := newCLILaneCity(t)
				c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
				var sent string
				var apiOp opjournal.Op
				c.cmdSubmitCity(t, lostAnswerServer(t, func(key string) {
					sent = key
					apiOp = c.proposeLaneSubmit(t, intent, key, cliLaneTestMessage, tc.outcome)
				}))
				c.sp.Calls = nil
				code, stdout, stderr := c.cmdSubmit(intent, "", false)
				if !strings.HasPrefix(sent, "k-") {
					t.Fatalf("Idempotency-Key = %q, want the minted key", sent)
				}
				if code != tc.code || stdout != tc.stdout(c.id, apiOp.ID, sent) || !strings.Contains(stderr, tc.stderr) {
					t.Fatalf("submit = %d, %q, stderr %q; want %d, %q and %q", code, stdout, stderr, tc.code, tc.stdout(c.id, apiOp.ID, sent), tc.stderr)
				}
				c.assertNothingDelivered(t)
				if got := c.only(t); got.ID != apiOp.ID || got.Status != apiOp.Status || got.Revision != apiOp.Revision {
					t.Fatalf("op rows = %+v, want only the API's op %+v, untouched", got, apiOp)
				}
			})
		}
	}
	t.Run("open, under --json", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		var sent string
		var apiOp opjournal.Op
		c.cmdSubmitCity(t, lostAnswerServer(t, func(key string) {
			sent = key
			apiOp = c.proposeLaneSubmit(t, session.SubmitIntentDefault, key, cliLaneTestMessage, nil)
		}))
		c.sp.Calls = nil
		code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "", true)
		if code != 0 {
			t.Fatalf("submit = %d; stderr %s", code, stderr)
		}
		contentOpValidate(t, "schemas/session/submit/result.schema.json", stdout)
		if got := cliOpJSON(t, []byte(stdout)); got["outcome"] != sessionSubmitAcceptedOutcome || got["op_id"] != apiOp.ID || got["key"] != sent {
			t.Fatalf("JSON = %v, want accepted with op %s and key %s", got, apiOp.ID, sent)
		}
		c.assertNothingDelivered(t)
	})
}

// The declined fallback's other answers from a --key with no API: a lane op
// of another request under it is refused as one, an op it names that cannot
// be read is refused (the API may have accepted the message), and a key that
// names no op takes the legacy path.
func TestSessionSubmitLaneDeclinedFallbackKeyedAnswers(t *testing.T) {
	t.Run("the key names another request", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		c.cmdSubmitCity(t, nil)
		earlier := c.proposeLaneSubmit(t, session.SubmitIntentDefault, "k-reused", "an earlier message", nil)
		c.sp.Calls = nil
		code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "k-reused", true)
		if code != 1 || !strings.Contains(stderr, "the key names a different request (op "+earlier.ID+")") {
			t.Fatalf("submit = %d, stderr %q; want the key reuse refused", code, stderr)
		}
		if got := cliOpFailureCode(t, []byte(stdout)); got != cliOpCodeKeyMismatch {
			t.Fatalf("failure code %q, want %s", got, cliOpCodeKeyMismatch)
		}
		c.assertNothingDelivered(t)
		if got := c.only(t); got.ID != earlier.ID || got.Revision != earlier.Revision {
			t.Fatalf("op rows = %+v, want only the earlier op, untouched", got)
		}
	})
	t.Run("the op the key names cannot be read", func(t *testing.T) {
		store := &laneGetFailStore{laneTestStore: newLaneTestStore()}
		c := newCLILaneCityOver(t, store)
		c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		c.cmdSubmitCity(t, nil)
		id, err := opjournal.DeriveID("gc", c.id, legacyop.PrincipalOperator, "k-unread")
		if err != nil {
			t.Fatal(err)
		}
		store.failID, store.fails = id, 1
		c.sp.Calls = nil
		code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "k-unread", true)
		if code != 1 || !strings.Contains(stderr, "store hiccup") || !strings.Contains(stderr, "the message did not run (key k-unread)") {
			t.Fatalf("submit = %d, stderr %q; want the failed read refused", code, stderr)
		}
		if got := cliOpFailureCode(t, []byte(stdout)); got != cliOpCodeUnavailable {
			t.Fatalf("failure code %q, want %s", got, cliOpCodeUnavailable)
		}
		if store.fails != 0 {
			t.Fatalf("the op %s was never read", id)
		}
		c.assertNothingDelivered(t)
		if ops := c.ops(t); len(ops) != 0 {
			t.Fatalf("op rows %+v, want none", ops)
		}
	})
	t.Run("the key names no op: the legacy path delivers", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		c.env["GC_OPS_CONTENT"] = "auto"
		c.cmdSubmitCity(t, nil)
		c.sp.Calls = nil
		code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "k-fresh", true)
		if code != 0 {
			t.Fatalf("submit = %d; stderr %s", code, stderr)
		}
		if n := len(c.nudges()); n != 1 {
			t.Fatalf("deliveries = %d, want the legacy path's one", n)
		}
		op := c.only(t)
		if op.Executor != opjournal.ExecutorLegacyCLI || op.ClientKey != "k-fresh" || op.Outcome.Arm != opjournal.ArmSucceeded {
			t.Fatalf("op = %+v, want the legacy path's delivered op under k-fresh", op)
		}
		if got := cliOpJSON(t, []byte(stdout)); got["op_id"] != op.ID || got["key"] != "k-fresh" {
			t.Fatalf("JSON = %v, want op %s under k-fresh", got, op.ID)
		}
	})
}

// The legacy path of a declined submit admits under the key the API was
// sent, so it finds an op the API recorded under it as a legacy op (an API
// whose config does not list message yet): open, that op is the API's
// acceptance, and the message is not delivered again.
func TestSessionSubmitLaneDeclinedLegacyPathSharesTheAPIsKey(t *testing.T) {
	c := newCLILaneCity(t)
	c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	c.env["GC_OPS_CONTENT"] = "auto"
	var sent string
	var apiOp opjournal.Op
	c.cmdSubmitCity(t, lostAnswerServer(t, func(key string) {
		sent = key
		op, created, err := c.laneStore(t).Propose(opjournal.Request{
			Conversation: c.id, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLegacyAPI, ClientKey: key,
			Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
			Payload:  cliLaneTestMessage, Policy: api.MessageOpPolicy(session.SubmitIntentDefault),
			StartBy: laneT0.Add(legacyop.ExecutionBound), FinishBy: laneT0.Add(legacyop.ExecutionBound),
		})
		if err != nil || !created {
			t.Errorf("proposing the API's legacy op: created=%t err=%v", created, err)
		}
		apiOp = op
	}))
	c.sp.Calls = nil
	code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "", false)
	want := fmt.Sprintf("Accepted by the controller for %s (op %s); gc session submit --key %s reports its outcome\n", c.id, apiOp.ID, sent)
	if code != 0 || stdout != want {
		t.Fatalf("submit = %d, %q, stderr %q; want %q", code, stdout, stderr, want)
	}
	c.assertNothingDelivered(t)
	if got := c.only(t); got.ID != apiOp.ID || got.Status == opjournal.StatusClosed {
		t.Fatalf("op rows = %+v, want only the API's open op", got)
	}
}

// laneUpsertStore overwrites a bead whose pinned id it already holds instead
// of refusing the create: a store without the duplicate-id contract, which
// opjournal.Store.Propose guards by reading an op before it creates one
// unless the request's key was minted in the same call.
type laneUpsertStore struct {
	laneTestStore
}

func (s laneUpsertStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.ID != "" {
		if _, err := s.Get(b.ID); err == nil {
			if err := s.Delete(b.ID); err != nil {
				return beads.Bead{}, err
			}
		}
	}
	return s.laneTestStore.Create(b)
}

// A key gc session submit minted is no longer new once it went to the API,
// so the fallback's Propose reads the op the API proposed under it before it
// creates one: on a store that overwrites a duplicate pinned id, a lost
// answer for an op the lane already delivered replays it, rather than
// resetting it and delivering the message again.
func TestSessionSubmitFallbackReadsTheAPIsOpFirst(t *testing.T) {
	c := newCLILaneCityOver(t, laneUpsertStore{newLaneTestStore()})
	var apiOp opjournal.Op
	c.cmdSubmitCity(t, lostAnswerServer(t, func(key string) {
		apiOp = c.proposeLaneSubmit(t, session.SubmitIntentDefault, key, cliLaneTestMessage, &opjournal.Outcome{Arm: opjournal.ArmSucceeded})
	}))
	c.sp.Calls = nil
	code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "", false)
	if code != 0 || stdout != "Submitted to "+c.id+"\n" || !strings.Contains(stderr, "replayed op "+apiOp.ID) {
		t.Fatalf("submit = %d, %q, stderr %q; want the API's op replayed", code, stdout, stderr)
	}
	c.assertNothingDelivered(t)
	if got := c.only(t); got.Status != opjournal.StatusClosed || got.Revision != apiOp.Revision {
		t.Fatalf("the API's op was rewritten: %+v, was %+v", got, apiOp)
	}
}

// gc session submit, with message listed, refuses a lane op it cannot write
// before the configured named session's row materializes, as gc session
// nudge does (TestCmdSessionNudgeLaneRefusesBeforeMaterializing): the lane
// preflight runs ahead of the resolution for a store that cannot open as a
// lane store, an invalid key and a payload the journal refuses.
func TestCmdSessionSubmitLaneRefusesBeforeMaterializing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// laneStore opens the city's store as a store that can host lanes,
		// instead of the file store a test city opens, which names no id
		// prefix.
		laneStore bool
		key       string
		message   string
		code      string
		stderr    string
	}{
		{
			name: "the store cannot host lanes", message: "hello", code: cliOpCodeUnavailable,
			stderr: "op journal cannot record this request (opening op store",
		},
		{
			name: "an invalid key", laneStore: true, key: " bad-key", message: "hello", code: cliOpCodeInvalidKey,
			stderr: "invalid --key (",
		},
		{
			name: "a payload the journal refuses", laneStore: true, message: "\xff", code: cliOpCodeUnavailable,
			stderr: "op journal cannot record this request (opjournal: invalid request: payload is not valid UTF-8); the message did not run",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newCLIOpHarness(t, nil)
			dir := newCLIOpNamedCityWithoutRow(t)
			cityTOML := filepath.Join(dir, "city.toml")
			data, err := os.ReadFile(cityTOML)
			if err != nil {
				t.Fatalf("reading city.toml: %v", err)
			}
			if err := os.WriteFile(cityTOML, append(data, []byte("\n[ops]\nauthoritative = [\"message\"]\n")...), 0o644); err != nil {
				t.Fatalf("writing city.toml: %v", err)
			}
			t.Setenv("GC_NO_API", "1")
			if tc.laneStore {
				store := newLaneTestStore()
				prev := openStoreFactoryForCity
				openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
					return beads.StoreOpenResult{Store: store}, nil
				}
				t.Cleanup(func() { openStoreFactoryForCity = prev })
			}
			var stdout, stderr bytes.Buffer
			code := cmdSessionSubmit([]string{killPokeSessionIdentity, tc.message}, session.SubmitIntentDefault, tc.key, true, &stdout, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("submit = %d, stderr %q; want the lane refusal %q", code, stderr.String(), tc.stderr)
			}
			if got := cliOpFailureCode(t, stdout.Bytes()); got != tc.code {
				t.Fatalf("failure code %q, want %s", got, tc.code)
			}
			if n := cliOpSessionRows(t, dir); n != 0 {
				t.Fatalf("session rows = %d, want none: the refusal comes before the resolution", n)
			}
		})
	}
}

func TestMailNotifyUnderLaneProposesANotice(t *testing.T) {
	c := newCLILaneCity(t)
	id, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", "msg-1")
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	op := c.laneOpByKey(t, mailNoticePrincipal, "mail:msg-1")
	if id != op.ID || op.Causation != "msg-1" || op.Status == opjournal.StatusClosed {
		t.Fatalf("op %s = %+v, want an open notice caused by msg-1", id, op)
	}
	assertLaneNotice(t, op, mailNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("mail", "You have mail from alice"))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
	if len(c.rings) != 1 || c.rings[0] != c.id+":"+op.ID {
		t.Fatalf("rings = %v, want one for the notice", c.rings)
	}
}

// gc mail send of a message whose lane notice already closed replays the
// notice only if it did its work: delivered (succeeded) or withdrawn by a
// read of the message (canceled). One that failed or was skipped was never
// delivered, so the send reports the nudge failed and not notified, as the
// legacy path refuses it. Either way nothing is proposed, delivered, queued
// or rung again: mail:<message id> stays one op per message and recipient.
func TestMailNotifyUnderLaneReplaysOnlyADeliveredOrWithdrawnNotice(t *testing.T) {
	for _, tc := range []struct {
		name string
		// outcome closes the first notice; withdrawn cancels it instead, as a
		// read of its message does.
		outcome   opjournal.Outcome
		withdrawn bool
		// refused is the closed outcome the send reports; "" for a replay.
		refused string
	}{
		{name: "delivered", outcome: opjournal.Outcome{Arm: opjournal.ArmSucceeded}},
		{name: "withdrawn", withdrawn: true},
		{name: "failed", outcome: opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonPoison}, refused: "failed{poison}"},
		{name: "skipped", outcome: opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonConversationClosed}, refused: "skipped{conversation_closed}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			// The fake mail provider's first message is fake-1, so the send
			// below notifies the message this notice announced.
			first, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", "fake-1")
			if err != nil {
				t.Fatalf("first notify: %v", err)
			}
			st := c.laneStore(t)
			if tc.withdrawn {
				_, _, err = st.Cancel(first, mailNoticePrincipal, mailNoticeWithdrawReason)
			} else {
				_, err = st.Close(first, tc.outcome, "lane:test")
			}
			if err != nil {
				t.Fatalf("ending the first notice: %v", err)
			}
			if op := c.get(t, st, first); op.Status != opjournal.StatusClosed {
				t.Fatalf("first notice is %s, want it closed", op.Status)
			}
			rings, nudges := len(c.rings), len(c.nudges())
			notify := func(_, messageID string) (string, error) {
				return sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", messageID)
			}
			var stdout, stderr bytes.Buffer
			if code := doMailSendJSON(mail.NewFake(), events.Discard, nil, "alice", []string{c.target.agentKey(), "hi"}, notify, "", true, &stdout, &stderr); code != 0 {
				t.Fatalf("gc mail send = %d; stderr %q", code, stderr.String())
			}
			var res mailActionResult
			if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
				t.Fatalf("decoding the send result %q: %v", stdout.String(), err)
			}
			if res.ID != "fake-1" {
				t.Fatalf("sent message %q, want fake-1, the message the first notice announced", res.ID)
			}
			if tc.refused != "" {
				want := "gc mail send: nudge failed: op " + first + " closed " + tc.refused + "\n"
				if res.Notified || len(res.NotifyOpIDs) != 0 || stderr.String() != want {
					t.Fatalf("notified %v, ops %v, stderr %q; want not notified and %q", res.Notified, res.NotifyOpIDs, stderr.String(), want)
				}
			} else if !res.Notified || !slices.Equal(res.NotifyOpIDs, []string{first}) || stderr.Len() != 0 {
				t.Fatalf("notified %v, ops %v, stderr %q; want the replay of %s", res.Notified, res.NotifyOpIDs, stderr.String(), first)
			}
			if n := len(c.ops(t)); n != 1 {
				t.Fatalf("op rows = %d, want only the message's first notice", n)
			}
			if len(c.rings) != rings || len(c.nudges()) != nudges || c.queuedItems(t) != 0 {
				t.Fatalf("the send rang %d, delivered %d, queued %d; want none", len(c.rings)-rings, len(c.nudges())-nudges, c.queuedItems(t))
			}
		})
	}
}

// Reading or archiving a message withdraws its open lane notices.
func TestMailReadCancelsItsNotice(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(p mail.Provider, id string) error
	}{
		{"read", func(p mail.Provider, id string) error { _, err := p.Read(id); return err }},
		{"mark read", func(p mail.Provider, id string) error { return p.MarkRead(id) }},
		{"archive", func(p mail.Provider, id string) error { return p.Archive(id) }},
		{"archive many", func(p mail.Provider, id string) error { _, err := p.ArchiveMany([]string{id}); return err }},
		{"delete", func(p mail.Provider, id string) error { return p.Delete(id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			inner := mail.NewFake()
			msg, err := inner.Send("alice", c.target.agentKey(), "hi", "body")
			if err != nil {
				t.Fatal(err)
			}
			opID, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", msg.ID)
			if err != nil {
				t.Fatal(err)
			}
			var warn bytes.Buffer
			p := withMailNoticeWithdrawal(inner, c.store, c.cfg, &warn)
			if err := tc.do(p, msg.ID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			op := c.get(t, c.laneStore(t), opID)
			if op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmCanceled {
				t.Fatalf("notice after %s = %s %+v, want it canceled; warnings %s", tc.name, op.Status, op.Outcome, warn.String())
			}
		})
	}
	// Without message listed the provider is not wrapped.
	c := newCLILaneCity(t)
	c.cfg.Ops.Authoritative = nil
	inner := mail.NewFake()
	if p := withMailNoticeWithdrawal(inner, c.store, c.cfg, nil); p != mail.Provider(inner) {
		t.Fatalf("provider wrapped with message unlisted: %T", p)
	}
}

// The city's bead mail provider keeps every capability its readers ask for
// (the session inbox, the handoff archive) under the withdrawal, and each of
// its reads and archives withdraws the message's open lane notices.
func TestBeadMailReadCancelsItsNotice(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(p mail.Provider, id string) error
	}{
		{"read", func(p mail.Provider, id string) error { _, err := p.Read(id); return err }},
		{"mark read", func(p mail.Provider, id string) error { return p.MarkRead(id) }},
		{"archive", func(p mail.Provider, id string) error { return p.Archive(id) }},
		{"archive many", func(p mail.Provider, id string) error { _, err := p.ArchiveMany([]string{id}); return err }},
		{"archive matching", func(p mail.Provider, _ string) error {
			_, _, err := p.(*beadmail.Provider).ArchiveMatching(beadmail.ArchiveFilter{From: "alice", IncludeRead: true})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			inner := beadmail.New(c.store)
			msg, err := inner.Send("alice", c.target.agentKey(), "hi", "body")
			if err != nil {
				t.Fatal(err)
			}
			opID, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", msg.ID)
			if err != nil {
				t.Fatal(err)
			}
			var warn bytes.Buffer
			p := withMailNoticeWithdrawal(inner, c.store, c.cfg, &warn)
			if _, reads := p.(beadmail.SessionInboxReader); !reads {
				t.Fatalf("the withdrawal hides beadmail.SessionInboxReader (%T)", p)
			}
			if _, archives := p.(inboxAutoHandoffArchiver); !archives {
				t.Fatalf("the withdrawal hides the handoff archive (%T)", p)
			}
			if err := tc.do(p, msg.ID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			op := c.get(t, c.laneStore(t), opID)
			if op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmCanceled {
				t.Fatalf("notice after %s = %s %+v, want it canceled; warnings %s", tc.name, op.Status, op.Outcome, warn.String())
			}
		})
	}
}

// With message listed, an injected auto-handoff is still archived after its
// injection, so the next prompt's mail check does not inject it again.
func TestMailNoticeWithdrawalKeepsTheHandoffArchive(t *testing.T) {
	for _, listed := range []bool{false, true} {
		store := beads.NewMemStore()
		cfg := &config.City{}
		if listed {
			cfg.Ops.Authoritative = []string{"message"}
		}
		mp := withMailNoticeWithdrawal(beadmail.New(store), store, cfg, io.Discard)
		auto, err := store.Create(beads.Bead{
			Title: "context cycle", Type: "message", Assignee: "mayor", From: "mayor",
			Labels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
		})
		if err != nil {
			t.Fatal(err)
		}
		var first, second bytes.Buffer
		if code := doMailCheck(mp, "mayor", true, &first, io.Discard); code != 0 {
			t.Fatalf("message listed %v: first mail check = %d", listed, code)
		}
		if !bytes.Contains(first.Bytes(), []byte(auto.ID)) {
			t.Fatalf("message listed %v: the handoff %s was not injected: %s", listed, auto.ID, first.String())
		}
		_ = doMailCheck(mp, "mayor", true, &second, io.Discard)
		if b, _ := store.Get(auto.ID); b.Status != "closed" || bytes.Contains(second.Bytes(), []byte(auto.ID)) {
			t.Fatalf("message listed %v: handoff %s is %s after its injection and injected again: %v", listed, auto.ID, b.Status, bytes.Contains(second.Bytes(), []byte(auto.ID)))
		}
	}
}

// cityOnDisk writes the fixture's city.toml, with message listed in [ops]
// authoritative, for the paths that load the city's config from disk, and
// points GC_CITY at it.
func (c *cliLaneCity) cityOnDisk(t *testing.T) {
	t.Helper()
	cityTOML := "[workspace]\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[ops]\nauthoritative = [\"message\"]\n"
	if err := os.WriteFile(filepath.Join(c.dir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", c.dir)
	resetCLIStorageRoutes(t)
}

// mailWithNotice sends the fixture's session a message whose lane notice is
// open, and returns the message and its notice's op id.
func (c *cliLaneCity) mailWithNotice(t *testing.T, labels ...string) (beads.Bead, string) {
	t.Helper()
	msg, err := c.store.Create(beads.Bead{Title: "hi", Type: "message", Assignee: c.id, From: "alice", Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	opID, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op := c.get(t, c.laneStore(t), opID); op.Status == opjournal.StatusClosed {
		t.Fatalf("notice %s closed before the message was read: %+v", opID, op.Outcome)
	}
	return msg, opID
}

// assertWithdrawn checks the notice opID was canceled.
func (c *cliLaneCity) assertWithdrawn(t *testing.T, opID, after string, warnings *bytes.Buffer) {
	t.Helper()
	if op := c.get(t, c.laneStore(t), opID); op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmCanceled {
		t.Fatalf("notice after %s = %s %+v, want it canceled; warnings %q", after, op.Status, op.Outcome, warnings.String())
	}
}

// Each production builder of the city's mail provider registers the
// withdrawal: a read through the CLI's (openCityMailProvider, behind gc mail
// read, mark-read, archive, delete and check) or the controller's
// (newCityMailProvider, behind the API's mail reads) cancels the message's
// open lane notice.
func TestCityMailProvidersWithdrawAReadMessagesNotice(t *testing.T) {
	t.Run("openCityMailProvider", func(t *testing.T) {
		clearGCEnv(t)
		c := newCLILaneCity(t)
		c.cityOnDisk(t)
		prevFactory := openStoreFactoryForCity
		openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
			return beads.StoreOpenResult{Store: c.store}, nil
		}
		t.Cleanup(func() { openStoreFactoryForCity = prevFactory })
		msg, opID := c.mailWithNotice(t)
		var stderr bytes.Buffer
		p, code := openCityMailProvider(&stderr, "gc mail read")
		if code != 0 {
			t.Fatalf("openCityMailProvider = %d; stderr %s", code, stderr.String())
		}
		if _, err := p.Read(msg.ID); err != nil {
			t.Fatalf("read: %v", err)
		}
		c.assertWithdrawn(t, opID, "a read", &stderr)
	})
	t.Run("newCityMailProvider", func(t *testing.T) {
		clearGCEnv(t)
		c := newCLILaneCity(t)
		msg, opID := c.mailWithNotice(t)
		p := newCityMailProvider(nil, c.store, c.cfg, c.dir, nil)
		if _, err := p.Read(msg.ID); err != nil {
			t.Fatalf("read: %v", err)
		}
		c.assertWithdrawn(t, opID, "a read", &bytes.Buffer{})
	})
}

// SessionStart (sessionStartMailInjection, as gc prime --hook runs it)
// archives the auto-handoff it injected through a bead mail provider of its
// own; with message listed, that archive withdraws the handoff's open lane
// notice, as gc mail check --inject's does.
func TestSessionStartHandoffArchiveWithdrawsItsNotice(t *testing.T) {
	clearGCEnv(t)
	c := newCLILaneCity(t)
	c.cityOnDisk(t)
	t.Setenv("GC_SESSION_ID", c.id)
	handoff, opID := c.mailWithNotice(t, mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel)
	live := c.rowInfo(t)
	var stderr bytes.Buffer
	startMail := sessionStartMailInjection(c.store, &live, c.dir, &stderr)
	if !startMail.autoHandoffIDs[handoff.ID] || startMail.autoHandoff.afterDelivery == nil {
		t.Fatalf("SessionStart did not inject handoff %s (ids %v); stderr %s", handoff.ID, startMail.autoHandoffIDs, stderr.String())
	}
	startMail.autoHandoff.afterDelivery()
	if b, err := c.store.Get(handoff.ID); err != nil || b.Status != "closed" {
		t.Fatalf("handoff after its injection = %+v, %v; want it archived; stderr %s", b, err, stderr.String())
	}
	c.assertWithdrawn(t, opID, "the SessionStart archive", &stderr)
}

func TestSlingNudgeUnderLaneProposesANotice(t *testing.T) {
	c := newCLILaneCity(t)
	var stdout, stderr bytes.Buffer
	deliverSlingNudge(c.target, c.sp, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	if !strings.Contains(stdout.String(), "in the conversation lane") || stderr.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	op := c.laneOpByKey(t, slingNoticePrincipal, "sling:gc-bead-1")
	assertLaneNotice(t, op, slingNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("sling", slingNudgeText))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
}

// A second gc sling --nudge of the same bead to the same session, after the
// first notice closed, proposes a new notice under a minted key rather than
// finding the closed one and reporting a nudge it never queued; while the
// first is still open the second coalesces into it. The renewal converges:
// a re-sling while the renewed notice is open coalesces into that one, and
// one after it closed renews again.
func TestSlingNudgeUnderLaneReslingAfterTheNoticeClosed(t *testing.T) {
	c := newCLILaneCity(t)
	var stdout, stderr bytes.Buffer
	deliverSlingNudge(c.target, c.sp, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	first := c.laneOpByKey(t, slingNoticePrincipal, "sling:gc-bead-1")
	stdout.Reset()
	deliverSlingNudge(c.target, c.sp, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	if n := len(c.ops(t)); n != 1 {
		t.Fatalf("re-sling with the notice open wrote %d ops, want the one it coalesces into", n)
	}
	if _, err := c.laneStore(t).Close(first.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:test"); err != nil {
		t.Fatalf("close: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	deliverSlingNudge(c.target, c.sp, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	if !strings.Contains(stdout.String(), "in the conversation lane") || stderr.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	var open []opjournal.Op
	for _, op := range c.ops(t) {
		if op.Status != opjournal.StatusClosed {
			open = append(open, op)
		}
	}
	if len(open) != 1 || open[0].ID == first.ID || open[0].ClientKey == first.ClientKey || open[0].Causation != "gc-bead-1" {
		t.Fatalf("open ops after the re-sling = %+v, want one new notice caused by the bead", open)
	}
	assertLaneNotice(t, open[0], slingNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("sling", slingNudgeText))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
	renewed := open[0]
	deliverSlingNudge(c.target, c.sp, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	if open := c.openOps(t); len(open) != 1 || open[0].ID != renewed.ID || len(c.ops(t)) != 2 {
		t.Fatalf("open ops after a re-sling with the renewal open = %+v of %d ops, want only the renewal %s", open, len(c.ops(t)), renewed.ID)
	}
	if _, err := c.laneStore(t).Close(renewed.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:test"); err != nil {
		t.Fatalf("close: %v", err)
	}
	deliverSlingNudge(c.target, c.sp, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	if open := c.openOps(t); len(open) != 1 || open[0].ID == first.ID || open[0].ID == renewed.ID || len(c.ops(t)) != 3 {
		t.Fatalf("open ops after a re-sling with the renewal closed = %+v of %d ops, want one new notice", open, len(c.ops(t)))
	}
	if stderr.Len() != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("stderr %q, %d queued", stderr.String(), c.queuedItems(t))
	}
}

// The hook-claim continuation of the same root again, in a later generation
// of the session (the generation is in the request's digest, not its key) or
// after the first notice closed, is a new lane notice under a minted key,
// never the legacy queue; a claim while that notice is open coalesces into
// it.
func TestHookClaimContinuationUnderLaneRenews(t *testing.T) {
	for _, nextGeneration := range []bool{false, true} {
		c := newCLILaneCity(t)
		if handled, err := continuationLaneNotice(c.target, c.store, c.sp, "gc-root-1"); !handled || err != nil {
			t.Fatalf("first continuation = %v, %v; want proposed", handled, err)
		}
		first := c.laneOpByKey(t, hookClaimContinuationPrincipal, "hook-claim:gc-root-1")
		if _, err := c.laneStore(t).Close(first.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:test"); err != nil {
			t.Fatalf("close: %v", err)
		}
		target := c.target
		if nextGeneration {
			if err := c.store.SetMetadata(c.id, "generation", "2"); err != nil {
				t.Fatalf("bump generation: %v", err)
			}
			target = resolveNudgeTargetFromSessionInfo(c.dir, c.cfg, c.rowInfo(t))
		}
		if handled, err := continuationLaneNotice(target, c.store, c.sp, "gc-root-1"); !handled || err != nil {
			t.Fatalf("next generation %v: the continuation = %v, %v; want proposed on the lane", nextGeneration, handled, err)
		}
		var open []opjournal.Op
		for _, op := range c.ops(t) {
			if op.Status != opjournal.StatusClosed {
				open = append(open, op)
			}
		}
		wantGen := int64(1)
		if nextGeneration {
			wantGen = 2
		}
		if len(open) != 1 || open[0].ID == first.ID || open[0].Causation != "gc-root-1" || open[0].Scope.Generation != wantGen {
			t.Fatalf("next generation %v: open ops = %+v, want one new notice for generation %d", nextGeneration, open, wantGen)
		}
		if c.queuedItems(t) != 0 {
			t.Fatalf("next generation %v: the continuation was queued on the legacy queue", nextGeneration)
		}
		if handled, err := continuationLaneNotice(target, c.store, c.sp, "gc-root-1"); !handled || err != nil {
			t.Fatalf("next generation %v: the third continuation = %v, %v", nextGeneration, handled, err)
		}
		if again := c.openOps(t); len(again) != 1 || again[0].ID != open[0].ID || len(c.ops(t)) != 2 {
			t.Fatalf("next generation %v: open ops after a claim with the renewal open = %+v of %d ops, want only the renewal %s", nextGeneration, again, len(c.ops(t)), open[0].ID)
		}
	}
}

func TestHookClaimContinuationUnderLaneProposesANotice(t *testing.T) {
	c := newCLILaneCity(t)
	if handled, err := continuationLaneNotice(c.target, c.store, c.sp, "gc-root-1"); !handled || err != nil {
		t.Fatalf("continuationLaneNotice = %v, %v; want proposed", handled, err)
	}
	op := c.laneOpByKey(t, hookClaimContinuationPrincipal, "hook-claim:gc-root-1")
	if op.Causation != "gc-root-1" || op.Scope.Kind != opjournal.ScopeSession || op.Scope.Generation != 1 {
		t.Fatalf("op = %+v, want caused by the root and scoped to the generation", op)
	}
	assertLaneNotice(t, op, hookClaimContinuationPrincipal, opjournal.WakeLiveOnly, reminderGolden("hook-claim-continuation", hookClaimContinuationText))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
}

// With message unlisted every CLI producer keeps its legacy path and writes
// no lane op.
func TestCLIProducersWithoutLaneKeepLegacy(t *testing.T) {
	c := newCLILaneCity(t)
	c.cfg.Ops.Authoritative = nil
	c.target.cfg = c.cfg
	if prod, err := cliLaneProduce(context.Background(), cliLaneProduction{
		target: c.target, store: c.store, sp: c.sp,
		req: cliLaneNotice(mailNoticePrincipal, "mail:m", "m", "x", opjournal.WakeIfNeeded),
	}); prod.handled || err != nil {
		t.Fatalf("cliLaneProduce with message unlisted = %+v, %v; want unhandled", prod, err)
	}
	var stdout, stderr bytes.Buffer
	deliverSlingNudge(c.target, c.sp, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	if strings.Contains(stdout.String(), "conversation lane") {
		t.Fatalf("sling took the lane: %q", stdout.String())
	}
	if handled, err := continuationLaneNotice(c.target, c.store, c.sp, "gc-root-1"); handled || err != nil {
		t.Fatalf("the continuation took the lane: %v, %v", handled, err)
	}
	for _, op := range c.ops(t) {
		if op.Executor == opjournal.ExecutorLane {
			t.Fatalf("lane op %+v written with message unlisted", op)
		}
	}
}

// laneProposeFailStore refuses to create any op row: a proposal that fails
// before its row exists.
type laneProposeFailStore struct{ laneTestStore }

func (s laneProposeFailStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.Type == opjournal.BeadType {
		return beads.Bead{}, errors.New("store hiccup")
	}
	return s.laneTestStore.Create(b)
}

// cliLaneRefusalCases are the ways a CLI lane notice's op cannot be written
// once a lane serves its session: the store the producer is handed, over the
// fixture's, and the cause its refusal names.
var cliLaneRefusalCases = []struct {
	name  string
	store func(c *cliLaneCity, base *laneGetFailStore) beads.Store
	cause string
}{
	{name: "the op journal cannot open", store: func(_ *cliLaneCity, base *laneGetFailStore) beads.Store {
		return noBatchStore{base}
	}, cause: "opening lane store"},
	{name: "the session row cannot be read", store: func(c *cliLaneCity, base *laneGetFailStore) beads.Store {
		base.failID, base.fails = c.id, 1
		return base
	}, cause: "reading session "},
	{name: "the proposal fails", store: func(_ *cliLaneCity, base *laneGetFailStore) beads.Store {
		return laneProposeFailStore{base.laneTestStore}
	}, cause: "store hiccup"},
}

// Under lane authority a notice whose op cannot be written is refused and
// reported, as gc session nudge's request is (TestSessionNudgeLaneRefusesInRun):
// no CLI producer falls back to its legacy delivery or queue, and nothing is
// written or rung.
func TestCLILaneNoticeRefusalsNeverFallBack(t *testing.T) {
	producers := []struct {
		name string
		// run runs the producer over store and returns its report.
		run func(t *testing.T, c *cliLaneCity, store beads.Store) string
	}{
		{name: "sling", run: func(t *testing.T, c *cliLaneCity, store beads.Store) string {
			var stdout, stderr bytes.Buffer
			deliverSlingNudge(c.target, c.sp, store, c.dir, "gc-bead-1", &stdout, &stderr)
			if stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "warning: bead routed but nudge failed: op_journal_unavailable: ") {
				t.Fatalf("stdout %q, stderr %q; want only the refusal's warning", stdout.String(), stderr.String())
			}
			return stderr.String()
		}},
		{name: "mail notify", run: func(t *testing.T, c *cliLaneCity, store beads.Store) string {
			id, err := sendMailNotifyWithOp(c.target, store, c.sp, "alice", "msg-1")
			if !errors.Is(err, errCLILaneUnavailable) || id != "" {
				t.Fatalf("notify = %q, %v; want the refusal", id, err)
			}
			return err.Error()
		}},
		{name: "hook-claim continuation", run: func(t *testing.T, c *cliLaneCity, store beads.Store) string {
			handled, err := continuationLaneNotice(c.target, store, c.sp, "gc-root-1")
			if !handled || !errors.Is(err, errCLILaneUnavailable) {
				t.Fatalf("continuation = %v, %v; want handled with the refusal", handled, err)
			}
			return err.Error()
		}},
	}
	for _, p := range producers {
		for _, tc := range cliLaneRefusalCases {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				base := &laneGetFailStore{laneTestStore: newLaneTestStore()}
				c := newCLILaneCityOver(t, base)
				calls := len(c.nudges())
				if got := p.run(t, c, tc.store(c, base)); !strings.Contains(got, tc.cause) {
					t.Fatalf("refusal %q, want its cause %q", got, tc.cause)
				}
				base.fails = 0
				if ops := c.ops(t); len(ops) != 0 {
					t.Fatalf("op rows %+v, want none", ops)
				}
				if n := c.queuedItems(t); n != 0 {
					t.Fatalf("%d queue items, want none", n)
				}
				if n := len(c.nudges()); n != calls {
					t.Fatalf("%d deliveries, want none", n-calls)
				}
				if len(c.rings) != 0 {
					t.Fatalf("rings %v after a refusal", c.rings)
				}
			})
		}
	}
}

// The claim's continuation nudge through its production path
// (hookContinuationNudgeEnqueue, as writeHookClaimWorkResultForBead calls
// it) with message listed: the session a lane serves gets the root's
// hook-claim:<root id> notice and no queue item. A notice the op journal
// cannot record, or whose nudge store or session provider does not open, is
// warned of on stderr and nothing is queued.
func TestHookContinuationNudgeEnqueueUnderLane(t *testing.T) {
	for _, tc := range []struct {
		name string
		// opened is the store the call opens, over the fixture's; unopened
		// opens none, as when the nudge store does not open.
		opened   func(laneTestStore) beads.Store
		unopened bool
		broken   bool
		warning  string
	}{
		{name: "proposes the root's notice", opened: func(s laneTestStore) beads.Store { return s }},
		{name: "the op journal cannot open", opened: func(s laneTestStore) beads.Store { return noBatchStore{s} }, warning: "opening lane store"},
		{name: "the session provider does not open", opened: func(s laneTestStore) beads.Store { return s }, broken: true, warning: "opening the session provider"},
		{name: "the nudge store does not open", unopened: true, warning: "the nudge store did not open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := t.TempDir()
			cityTOML := "[workspace]\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[ops]\nauthoritative = [\"message\"]\n"
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
				t.Fatalf("WriteFile(city.toml): %v", err)
			}
			t.Setenv("GC_CITY", cityDir)
			resetCLIStorageRoutes(t)
			if tc.broken {
				t.Setenv("GC_SESSION", "broken")
				prevBuild := buildSessionProviderByName
				buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
					return nil, errors.New("injected provider failure")
				}
				t.Cleanup(func() { buildSessionProviderByName = prevBuild })
			}
			store := newLaneTestStore()
			row, err := store.Create(beads.Bead{
				Title: "Session: worker", Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"session_name": "worker-session", "agent_name": "worker", "template": "worker",
					"state": string(session.StateActive), "generation": "1", "continuation_epoch": "1", "instance_token": "tok-1",
				},
			})
			if err != nil {
				t.Fatalf("creating the session row: %v", err)
			}
			prevOpen, prevPoller, prevRing := openOwnedNudgeBeadStore, startNudgePoller, cliLaneRing
			t.Cleanup(func() { openOwnedNudgeBeadStore, startNudgePoller, cliLaneRing = prevOpen, prevPoller, prevRing })
			openOwnedNudgeBeadStore = func(string) (beads.NudgesStore, beads.Store) {
				if tc.unopened {
					return beads.NudgesStore{}, nil
				}
				return beads.NudgesStore{Store: store}, tc.opened(store)
			}
			var pollers, rings []string
			startNudgePoller = func(_, _, sessionName string) error {
				pollers = append(pollers, sessionName)
				return nil
			}
			cliLaneRing = func(_, conv, opID string) (string, error) {
				rings = append(rings, conv+":"+opID)
				return "ok", nil
			}

			var stderr bytes.Buffer
			hookContinuationNudgeEnqueue("worker-session", "gc-root-1", &stderr)

			state, err := nudgequeue.LoadState(cityDir)
			if err != nil {
				t.Fatalf("LoadState: %v", err)
			}
			if len(state.Pending) != 0 || len(pollers) != 0 {
				t.Fatalf("queued %+v, pollers %v; want nothing queued", state.Pending, pollers)
			}
			ops := cliOpRows(t, store)
			if tc.warning != "" {
				got := stderr.String()
				if !strings.HasPrefix(got, "gc hook --claim: warning: the continuation nudge for gc-root-1 was not sent: op_journal_unavailable: ") || !strings.Contains(got, tc.warning) {
					t.Fatalf("stderr %q, want the refusal naming %q", got, tc.warning)
				}
				if len(ops) != 0 || len(rings) != 0 {
					t.Fatalf("op rows %+v, rings %v after a refusal", ops, rings)
				}
				return
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr %q", stderr.String())
			}
			id, err := opjournal.DeriveID("gc", row.ID, hookClaimContinuationPrincipal, "hook-claim:gc-root-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(ops) != 1 || ops[0].ID != id || ops[0].Executor != opjournal.ExecutorLane || ops[0].Causation != "gc-root-1" || ops[0].Status == opjournal.StatusClosed {
				t.Fatalf("op rows %+v, want the open lane notice %s", ops, id)
			}
			if len(rings) != 1 || rings[0] != row.ID+":"+id {
				t.Fatalf("rings %v, want one for the notice", rings)
			}
		})
	}
}

// gc sling --nudge through its production path (doSling → doSlingNudge →
// deliverSlingNudge) with message listed proposes the slung bead's notice,
// keyed sling:<bead id>; a notice the op journal cannot record is warned of,
// and nothing is delivered or queued.
func TestDoSlingNudgeUnderLaneProposesTheBeadsNotice(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "proposes", true: "refused"}[refused], func(t *testing.T) {
			c := newCLILaneCity(t)
			if _, err := c.store.Create(beads.Bead{ID: "gc-work-1", Title: "work", Type: "task", Status: "open"}); err != nil {
				t.Fatalf("creating the bead: %v", err)
			}
			c.cfg.Workspace.Name = "test-city"
			prevPoller := startNudgePoller
			startNudgePoller = func(_, _, _ string) error { return nil }
			t.Cleanup(func() { startNudgePoller = prevPoller })
			runner := newFakeRunner()
			deps, stdout, stderr := testDeps(c.cfg, c.sp, runner.run)
			deps.CityPath = c.dir
			deps.Store = c.store
			if refused {
				deps.Store = noBatchStore{c.store}
			}
			opts := testOpts(config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, "gc-work-1")
			opts.Nudge = true
			calls := len(c.nudges())
			if code := doSling(opts, deps, nil, stdout, stderr); code != 0 {
				t.Fatalf("doSling = %d; stderr %s", code, stderr.String())
			}
			if n := len(c.nudges()); n != calls || c.queuedItems(t) != 0 {
				t.Fatalf("legacy delivery ran: %d nudges, %d queued", n-calls, c.queuedItems(t))
			}
			if refused {
				if !strings.Contains(stderr.String(), "warning: bead routed but nudge failed: op_journal_unavailable: ") || len(c.ops(t)) != 0 {
					t.Fatalf("stderr %q, ops %+v; want the refusal and no op", stderr.String(), c.ops(t))
				}
				return
			}
			op := c.laneOpByKey(t, slingNoticePrincipal, "sling:gc-work-1")
			if op.Causation != "gc-work-1" || op.Status == opjournal.StatusClosed || !strings.Contains(stdout.String(), "in the conversation lane") {
				t.Fatalf("op %+v, stdout %q; want the bead's open notice", op, stdout.String())
			}
			assertLaneNotice(t, op, slingNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("sling", slingNudgeText))
		})
	}
}

// gc sling --nudge of a rig bead to a fixed agent: the rig's store holds the
// bead but no session row, since session rows live in the city store. With
// message listed the target resolves in the city store, as the pool branch's
// does (#2579), so the bead's notice lands in the sessions class the lane
// host reads, and nothing takes the legacy path into the session the lane
// serves. A city store that does not open is refused, as a notice the op
// journal cannot record is. With message unlisted the city store is never
// opened, and the nudge keeps its legacy path.
func TestDoSlingNudgeUnderLaneResolvesARigBeadsSessionInTheCityStore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		listed  bool
		openErr error
	}{
		{name: "proposes", listed: true},
		{name: "the city store does not open", listed: true, openErr: errors.New("city store down")},
		{name: "message unlisted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			if !tc.listed {
				c.cfg.Ops.Authoritative = nil
			}
			c.cfg.Workspace.Name = "test-city"
			writeSlingTestCity(t, c.dir, "[workspace]\nname = \"test-city\"\n")
			rig := &beads.MemStore{IDPrefix: "rg", HonorExplicitIDs: true}
			if _, err := rig.Create(beads.Bead{ID: "rg-work-1", Title: "work", Type: "task", Status: "open"}); err != nil {
				t.Fatalf("creating the bead: %v", err)
			}
			opened := 0
			prevOpen := slingOpenCityStore
			slingOpenCityStore = func(path string) (beads.Store, error) {
				opened++
				if path != c.dir {
					t.Fatalf("slingOpenCityStore(%q), want %q", path, c.dir)
				}
				if tc.openErr != nil {
					return nil, tc.openErr
				}
				return c.store, nil
			}
			t.Cleanup(func() { slingOpenCityStore = prevOpen })
			prevPoller := startNudgePoller
			startNudgePoller = func(_, _, _ string) error { return nil }
			t.Cleanup(func() { startNudgePoller = prevPoller })
			runner := newFakeRunner()
			deps, stdout, stderr := testDeps(c.cfg, c.sp, runner.run)
			deps.CityPath = c.dir
			deps.Store = rig
			opts := testOpts(config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, "rg-work-1")
			opts.Nudge = true
			calls := len(c.nudges())
			if code := doSling(opts, deps, nil, stdout, stderr); code != 0 {
				t.Fatalf("doSling = %d; stderr %s", code, stderr.String())
			}
			if ops := cliOpRows(t, rig); len(ops) != 0 {
				t.Fatalf("op rows in the rig store %+v, want none", ops)
			}
			if !tc.listed {
				if opened != 0 || c.queuedItems(t) != 1 || len(c.ops(t)) != 0 {
					t.Fatalf("city store opened %d times, %d queued, ops %+v; want the legacy queue item alone", opened, c.queuedItems(t), c.ops(t))
				}
				return
			}
			if n := len(c.nudges()); n != calls || c.queuedItems(t) != 0 {
				t.Fatalf("legacy delivery ran: %d nudges, %d queued; stdout %q stderr %q", n-calls, c.queuedItems(t), stdout.String(), stderr.String())
			}
			if tc.openErr != nil {
				if !strings.Contains(stderr.String(), "warning: bead routed but nudge failed: op_journal_unavailable: ") ||
					!strings.Contains(stderr.String(), tc.openErr.Error()) || len(c.ops(t)) != 0 {
					t.Fatalf("stderr %q, ops %+v; want the refusal and no op", stderr.String(), c.ops(t))
				}
				return
			}
			if !strings.Contains(stdout.String(), "in the conversation lane") || stderr.Len() != 0 {
				t.Fatalf("stdout %q, stderr %q; want the notice proposed", stdout.String(), stderr.String())
			}
			op := c.laneOpByKey(t, slingNoticePrincipal, "sling:rg-work-1")
			if op.Causation != "rg-work-1" || op.Status == opjournal.StatusClosed {
				t.Fatalf("op %+v; want the bead's open notice", op)
			}
			assertLaneNotice(t, op, slingNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("sling", slingNudgeText))
		})
	}
}

// laneCausationFailStore fails the next fails keyed reads of ops by
// causation (opjournal.Store.OpenByCausation): a withdrawal that cannot list
// a message's notices.
type laneCausationFailStore struct {
	laneTestStore
	fails int
}

func (s *laneCausationFailStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.fails > 0 && q.Metadata["gc.op.causation"] != "" {
		s.fails--
		return nil, errors.New("store hiccup")
	}
	return s.laneTestStore.List(q)
}

// A withdrawal that fails after its message is archived is retried by the
// next archive, which finds the message already archived, through the bead
// mail provider's consumed hook and through the wrapper alike.
func TestMailArchiveRetriesAFailedWithdrawal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inner func(store beads.Store) mail.Provider
	}{
		{"wrapped provider", func(beads.Store) mail.Provider { return mail.NewFake() }},
		{"bead mail", func(store beads.Store) mail.Provider { return beadmail.New(store) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &laneCausationFailStore{laneTestStore: newLaneTestStore()}
			c := newCLILaneCityOver(t, store)
			inner := tc.inner(c.store)
			msg, err := inner.Send("alice", c.target.agentKey(), "hi", "body")
			if err != nil {
				t.Fatal(err)
			}
			opID, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", msg.ID)
			if err != nil {
				t.Fatal(err)
			}
			var warn bytes.Buffer
			p := withMailNoticeWithdrawal(inner, c.store, c.cfg, &warn)
			store.fails = 1
			if err := p.Archive(msg.ID); err != nil {
				t.Fatalf("archive: %v", err)
			}
			if op := c.get(t, c.laneStore(t), opID); op.Status == opjournal.StatusClosed || !strings.Contains(warn.String(), "listing the notices of "+msg.ID) {
				t.Fatalf("notice after a failed withdrawal = %s, warnings %q; want it open and the failure warned of", op.Status, warn.String())
			}
			if err := p.Archive(msg.ID); !errors.Is(err, mail.ErrAlreadyArchived) {
				t.Fatalf("second archive = %v, want mail.ErrAlreadyArchived", err)
			}
			if op := c.get(t, c.laneStore(t), opID); op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmCanceled {
				t.Fatalf("notice after the second archive = %s %+v, want it canceled; warnings %q", op.Status, op.Outcome, warn.String())
			}
		})
	}
}
