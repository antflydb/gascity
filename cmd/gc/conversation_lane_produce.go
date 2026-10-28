package main

// The CLI's conversation-lane producers (DES-O1 §7.5): gc session submit's
// local fallback, mail --notify, sling --nudge and the hook-claim
// continuation propose lane message ops when [ops] authoritative lists
// message and a lane host can type into the target session
// (laneServesSession), as gc session nudge does. A target with no session
// row, or one no lane serves (a protocol-mode session without a host
// stamp), takes the producer's legacy path; gc session submit, whose key
// went to the API first, takes it only when the key names no lane op
// (answerKeyedLaneOp), and under the same key. A proposal that cannot be
// written is refused (cliLaneRefusal), as gc session nudge's is: under lane
// authority there is no legacy path to fall back to (DES-O1 §7.5), so the
// producer reports the refusal and neither delivers nor queues anything.
// gc session submit fails as gc session nudge does (under --json with the
// failure envelope), gc sling and gc mail send warn, as they do when their
// legacy delivery fails, and gc hook --claim warns on stderr.
//
// A key that names an op already closed answers by what the key stands for.
// A re-sling or a later claim of a root is a new event, so gc sling and the
// hook-claim continuation renew the notice (proposeRenewal). A mail notice
// announces one message: a delivered or withdrawn notice replays, and one
// that failed or was skipped is refused (mailLaneNoticeResult). gc session
// submit replays only a success. Its key can also name the op its own legacy
// path recorded, which the lane never takes: the submit answers from that op
// (replayLegacy).
//
// After the Propose the producer rings the controller's lane host
// (lane:<conversation>:<op id>) once, best effort. Only a prompt the caller
// asks it for runs the CLI's content-only inline pass (cliLaneInlinePass);
// every notice waits for the lane host.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// errCLILaneUnavailable classifies a CLI lane proposal the op journal could
// not record: nothing was written, nothing delivered.
var errCLILaneUnavailable = errors.New("op_journal_unavailable")

// cliLaneRefusal is the refusal of a CLI lane proposal the op journal could
// not record for cause: a *cliLaneRefusalError, which wraps
// errCLILaneUnavailable.
func cliLaneRefusal(cause any) error {
	return &cliLaneRefusalError{cause: cause}
}

// cliLaneRefusalError is a CLI lane proposal the op journal could not record
// for cause, reported after errCLILaneUnavailable's code.
type cliLaneRefusalError struct{ cause any }

func (e *cliLaneRefusalError) Error() string {
	return errCLILaneUnavailable.Error() + ": " + cliLaneUnavailable(e.cause)
}

func (e *cliLaneRefusalError) Unwrap() error { return errCLILaneUnavailable }

// cliLaneProduction is one CLI producer's lane op.
type cliLaneProduction struct {
	target nudgeTarget
	// store is the producer's bead store; the op store opens over its
	// sessions class.
	store beads.Store
	sp    runtime.Provider
	// req is the op without its conversation; an empty ClientKey is minted.
	req opjournal.Request
	// sessionScoped scopes the op to the session's current generation.
	sessionScoped bool
	// renew makes req's key coalesce a notice only while its op is open:
	// one the key names already closed, or an earlier request the key no
	// longer matches (another generation's, KeyReuseError), is renewed
	// (proposeRenewal), so a repeated event is delivered again and one that
	// repeats while the renewal is open coalesces into it.
	renew bool
	// inline runs the content-only inline pass after the Propose.
	inline bool
	// warn takes the inline pass's warnings; nil discards them.
	warn io.Writer
}

// cliLaneProduced is what cliLaneProduce did: handled false when the
// producer takes its legacy path, nothing written; else the op as read
// after the Propose (and the inline pass), whether the Propose created it,
// whether it was closed before this call (a replay of a closed op, which no
// pass runs), and the pass's result. legacy is set when the key names an op
// the lane does not execute (one gc session submit's legacy path recorded
// under it): op is that op, untouched, and neither the pass nor the ring ran.
type cliLaneProduced struct {
	handled      bool
	op           opjournal.Op
	created      bool
	closedBefore bool
	legacy       bool
	res          opjournal.PassResult
}

// cliLaneKeyReuseError refuses a CLI lane proposal whose key names a
// different request; it unwraps to the journal's *opjournal.KeyReuseError.
type cliLaneKeyReuseError struct {
	key   string
	reuse *opjournal.KeyReuseError
}

func (e *cliLaneKeyReuseError) Error() string {
	return fmt.Sprintf("the key %q names a different request (op %s)", e.key, e.reuse.Existing.ID)
}

func (e *cliLaneKeyReuseError) Unwrap() error { return e.reuse }

// cliLaneProduce proposes p's op on the conversation lane when [ops]
// authoritative lists message, p's target names a session row and a lane
// host can type into the session. It returns handled false, having written
// nothing, otherwise. A proposal it cannot write is an error wrapping
// errCLILaneUnavailable, and one whose key names a different request a
// *cliLaneKeyReuseError, handled true. A key that names a legacy op
// (keyedLegacyOp) returns that op, legacy set.
func cliLaneProduce(ctx context.Context, p cliLaneProduction) (cliLaneProduced, error) {
	if !cliLaneExecutesMessage(p.target.cfg) || p.target.sessionID == "" || isNilBeadStore(p.store) {
		return cliLaneProduced{}, nil
	}
	sessions := cliSessionStore(p.store, p.target.cfg, p.target.cityPath)
	unavailable := func(err error) (cliLaneProduced, error) {
		return cliLaneProduced{handled: true}, cliLaneRefusal(err)
	}
	st, err := opjournal.NewLaneStore(beads.SessionStore{Store: sessions}, cliOpClock)
	if err != nil {
		return unavailable(err)
	}
	view, absent, err := readConversationLaneViewOnly(sessionFrontDoor(sessions), p.target.sessionID)
	switch {
	case err != nil:
		return unavailable(fmt.Errorf("reading session %s: %w", p.target.sessionID, err))
	case absent:
		return cliLaneProduced{}, nil
	}
	serves, err := laneServesSession(p.sp, st, p.target.sessionName, view)
	if err != nil {
		return unavailable(err)
	}
	if !serves {
		return cliLaneProduced{}, nil
	}
	req := p.req
	req.Conversation = p.target.sessionID
	req.Executor = opjournal.ExecutorLane
	req.Kind = opjournal.KindMessage
	if req.ClientKey == "" {
		key, err := legacyop.MintKey()
		if err != nil {
			return unavailable(err)
		}
		req.ClientKey, req.MintedKey = key, true
	}
	if p.sessionScoped {
		req.Scope = opjournal.Scope{Kind: opjournal.ScopeSession, Generation: view.Generation}
	}
	op, created, err := st.Propose(req)
	if p.renew && needsRenewal(req, op, created, err) {
		op, created, err = proposeRenewal(st, req)
	}
	if answer, done, answerErr := proposalAnswer(req, op, created, err); done {
		return answer, answerErr
	}
	out := cliLaneProduced{handled: true, op: op, created: created, closedBefore: op.Status == opjournal.StatusClosed}
	if p.inline && !out.closedBefore {
		out.op, out.res = cliLaneProduceInline(ctx, p, sessions, st, op)
	}
	if out.op.Status != opjournal.StatusClosed {
		// Best effort: the controller's anti-entropy scan finds the op too.
		_, _ = cliLaneRing(p.target.cityPath, p.target.sessionID, op.ID)
	}
	return out, nil
}

// cliLaneProduceInline runs the CLI's content-only inline pass after p
// proposed op, and returns op as read after it, with the pass's result. A
// pass that did not run, and a read of op after it that failed, are warned
// of on p.warn (nil discards them); op then stands as proposed, waiting for
// the controller.
func cliLaneProduceInline(ctx context.Context, p cliLaneProduction, sessions beads.Store, st *opjournal.Store, op opjournal.Op) (opjournal.Op, opjournal.PassResult) {
	warn := p.warn
	if warn == nil {
		warn = io.Discard
	}
	res, passErr := cliLaneInlinePass(ctx, p.target, sessions, p.sp, st, func(format string, args ...any) {
		fmt.Fprintf(warn, "warning: "+format+"\n", args...) //nolint:errcheck // best-effort warning
	})
	if passErr != nil {
		fmt.Fprintf(warn, "warning: the inline delivery did not run (%v); op %s waits for the controller\n", passErr, op.ID) //nolint:errcheck // best-effort warning
	}
	cur, err := st.Get(op.ID)
	if err != nil {
		fmt.Fprintf(warn, "warning: reading op %s after the inline delivery failed (%v); it waits for the controller\n", op.ID, err) //nolint:errcheck // best-effort warning
		return op, res
	}
	return cur, res
}

// proposalAnswer is what cliLaneProduce answers, done, for a Propose of req
// that returned op, created and err when it does not go on to the pass: the
// legacy op the key names (keyedLegacyOp), a key that names a different
// request, or a write the journal refused.
func proposalAnswer(req opjournal.Request, op opjournal.Op, created bool, err error) (cliLaneProduced, bool, error) {
	if legacy, ok := keyedLegacyOp(op, created, err); ok {
		return cliLaneProduced{handled: true, op: legacy, legacy: true}, true, nil
	}
	var reuse *opjournal.KeyReuseError
	switch {
	case err == nil:
		return cliLaneProduced{}, false, nil
	case errors.As(err, &reuse):
		return cliLaneProduced{handled: true}, true, &cliLaneKeyReuseError{key: req.ClientKey, reuse: reuse}
	}
	return cliLaneProduced{handled: true}, true, cliLaneRefusal(err)
}

// keyedLegacyOp is the message op a lane Propose that answered op, created
// and err found under its key when the lane does not execute that op: the
// stored op of a replay, or the one a KeyReuseError names (Propose returns
// the stored op whatever executor it names, and never writes over it). An op
// of another kind under the key stays the key reuse it is. Only gc session
// submit's key can name a legacy op: its legacy path records ops under the
// same operator principal, conversation and key.
func keyedLegacyOp(op opjournal.Op, created bool, err error) (opjournal.Op, bool) {
	var reuse *opjournal.KeyReuseError
	switch {
	case errors.As(err, &reuse):
		op = reuse.Existing
	case err != nil || created:
		return opjournal.Op{}, false
	}
	return op, op.Executor != opjournal.ExecutorLane && op.Kind == opjournal.KindMessage
}

// needsRenewal reports whether a renewing producer's Propose of req found a
// notice req's key no longer coalesces into: the op the key names closed, or
// an earlier request under the key (KeyReuseError). A minted key names no
// earlier op.
func needsRenewal(req opjournal.Request, op opjournal.Op, created bool, err error) bool {
	if req.MintedKey {
		return false
	}
	var reuse *opjournal.KeyReuseError
	if errors.As(err, &reuse) {
		return true
	}
	return err == nil && !created && op.Status == opjournal.StatusClosed
}

// proposeRenewal renews req, whose key no longer coalesces (needsRenewal):
// an open renewal of the same request (openRenewal) takes it, so an event
// that repeats while that notice is open coalesces into it; otherwise req is
// proposed under a minted key. Two renewals that race may both mint, and
// each is delivered.
func proposeRenewal(st *opjournal.Store, req opjournal.Request) (opjournal.Op, bool, error) {
	open, found, err := openRenewal(st, req)
	if err != nil || found {
		return open, false, err
	}
	key, err := legacyop.MintKey()
	if err != nil {
		return opjournal.Op{}, false, err
	}
	req.ClientKey, req.MintedKey = key, true
	return st.Propose(req)
}

// openRenewal finds the open op an earlier renewal of req proposed: a lane
// op of req's conversation, principal and causation (one keyed read,
// opjournal.Store.OpenByCausation) whose request digest is req's, so of the
// same scope, payload and policy. A request with no causation has none.
func openRenewal(st *opjournal.Store, req opjournal.Request) (opjournal.Op, bool, error) {
	if req.Causation == "" {
		return opjournal.Op{}, false, nil
	}
	want, err := st.Preview(req)
	if err != nil {
		return opjournal.Op{}, false, err
	}
	open, err := st.OpenByCausation(req.Causation)
	if err != nil {
		return opjournal.Op{}, false, err
	}
	for _, op := range open {
		if op.Conversation == want.Conversation && op.Producer.Principal == want.Producer.Principal && op.PayloadDigest == want.PayloadDigest {
			return op, true, nil
		}
	}
	return opjournal.Op{}, false, nil
}

// cliLaneNotice is the lane request of a CLI notice from principal under key
// (minted when empty), caused by causation, delivered at the boundary with
// wake.
func cliLaneNotice(principal, key, causation, text string, wake opjournal.Wake) opjournal.Request {
	return opjournal.Request{
		Kind:      opjournal.KindMessage,
		Executor:  opjournal.ExecutorLane,
		ClientKey: key,
		Producer:  opjournal.Producer{Principal: principal, Actor: opjournal.ActorBackground},
		Payload:   text,
		Causation: causation,
		Policy:    opjournal.Policy{Message: opjournal.MessagePolicy{Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: wake}},
	}
}

// sessionSubmitLane is one gc session submit's local fallback under lane
// authority.
type sessionSubmitLane struct {
	cityPath string
	cfg      *config.City
	store    beads.Store
	// sessionID is the resolved target, set once it resolved: the preflight
	// runs before.
	sessionID string
	target    string
	message   string
	intent    session.SubmitIntent
	// key is the op's client key, --key or minted (minted: minted in this
	// run and never sent to the API).
	key    string
	minted bool
	asJSON bool
	stdout io.Writer
	stderr io.Writer
	// sp is the session provider; nil opens the city's (newSessionProvider).
	sp runtime.Provider
}

// submitPreflight runs, before gc session submit resolves its target, the
// preflight of the path its local fallback takes first: the lane's when
// [ops] authoritative lists message (laneOn), else the content gate's. It
// reports false after reporting the refusal.
func submitPreflight(laneOn bool, lane sessionSubmitLane, op *cliOp) bool {
	if laneOn {
		return lane.preflight()
	}
	return op.preflight()
}

// preflight refuses, before the target resolves, a submit whose lane op
// cannot be written, as gc session nudge's lane preflight does
// (cliLaneNudgePreflight): a sessions store that cannot open as a lane
// store, an invalid key or a payload the journal refuses. The resolution
// may materialize a configured named session's row, so the refusal comes
// before any row is written; it is the refusal the proposal would answer
// (refuseProduce). It reports false after reporting the refusal.
func (s sessionSubmitLane) preflight() bool {
	_, err := opjournal.NewLaneStore(beads.SessionStore{Store: cliSessionStore(s.store, s.cfg, s.cityPath)}, cliOpClock)
	if err == nil {
		err = opjournal.ValidateClientKey(s.key)
	}
	if err == nil {
		err = opjournal.ValidatePayload(opjournal.KindMessage, s.message)
	}
	if err == nil {
		return true
	}
	s.refuseProduce(cliLaneRefusal(err))
	return false
}

// request is s's lane op without its conversation: the operator's message
// under s's key with api.LaneMessageOpPolicy, the op the API proposes.
func (s sessionSubmitLane) request() opjournal.Request {
	return opjournal.Request{
		ClientKey: s.key,
		MintedKey: s.minted,
		Producer:  opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:   s.message,
		Policy:    api.LaneMessageOpPolicy(s.intent),
	}
}

// submitSessionLane proposes the lane op the API would for s: the operator's
// message under s's key with api.LaneMessageOpPolicy, so it finds an op the
// API proposed before the fallback. A boundary prompt runs the CLI's
// content-only inline pass; a deliver-now (interrupt_now) unit is control,
// which waits for the controller's lane host. It prints delivered or queued
// as gc session nudge does. When no lane serves the session it answers from
// the lane op s's key names (answerKeyedLaneOp), and reports handled false,
// having written nothing, when the key names none. A key that names a legacy
// op is answered from that op (replayLegacy). A refusal is reported as gc
// session nudge's lane path reports one: on stderr, and under --json as the
// failure envelope.
func submitSessionLane(s sessionSubmitLane) (code int, handled bool) {
	sp := s.sp
	if sp == nil {
		opened, err := newSessionProvider()
		if err != nil {
			return s.refuseUnavailable(fmt.Errorf("opening the session provider: %w", err)), true
		}
		sp = opened
	}
	info, err := sessionFrontDoor(cliSessionStore(s.store, s.cfg, s.cityPath)).Get(s.sessionID)
	if err != nil {
		return s.refuseUnavailable(fmt.Errorf("reading session %s: %w", s.sessionID, err)), true
	}
	prod, err := cliLaneProduce(context.Background(), cliLaneProduction{
		target: resolveNudgeTargetFromSessionInfo(s.cityPath, s.cfg, info),
		store:  s.store,
		sp:     sp,
		req:    s.request(),
		inline: s.intent != session.SubmitIntentInterruptNow,
		warn:   prefixWriter{prefix: "gc session submit: ", w: s.stderr},
	})
	if !prod.handled {
		return s.answerKeyedLaneOp()
	}
	switch {
	case err != nil:
		return s.refuseProduce(err), true
	case prod.legacy:
		return s.replayLegacy(prod.op), true
	}
	if !s.resumeHeld(prod.op) {
		return 1, true
	}
	return s.report(prod), true
}

// resumeHeld is the resume: true the API applies once it has proposed a
// submit's lane op (api.acceptMessageLane), run for op, the lane op this
// fallback proposed or found open: a request that died between the API's
// proposal and its resume left the op waiting on the session's operator
// hold, so the fallback consumes the hold, as the legacy fallback does
// (resumeForAcceptedLaneOp), consuming only a hold stamped by the op's
// proposal. A closed op is left alone. It reports false, having printed the
// error, when the session refuses the resume (a drain is stopping it).
func (s sessionSubmitLane) resumeHeld(op opjournal.Op) bool {
	if op.Status == opjournal.StatusClosed {
		return true
	}
	if err := resumeForAcceptedLaneOp(cliSessionStore(s.store, s.cfg, s.cityPath), op); err != nil {
		fmt.Fprintf(s.stderr, "gc session submit: resuming %s for op %s: %v (key %s)\n", s.target, op.ID, err, s.key) //nolint:errcheck // best-effort stderr
		return false
	}
	return true
}

// answerKeyedLaneOp answers a submit to a session no lane serves (a
// protocol-mode session without a host stamp) from the lane op s's key
// names, which the legacy path would deliver again: the op the API proposed
// under the key before an answer the CLI never got, which the controller's
// lane host delivers, or one an earlier run proposed under --key. An open
// op is the API's acceptance (emitSessionSubmitAccepted) once its hold is
// consumed (resumeHeld), a closed one is reported as report reports a
// replay, and a lane op of another request is refused as a key that names
// a different request. It reports handled
// false, having written nothing, when the key names no lane op, which a key
// minted in this run and never sent to the API cannot. A read that fails is
// refused: the API may have accepted the message.
func (s sessionSubmitLane) answerKeyedLaneOp() (code int, handled bool) {
	if s.minted {
		return 0, false
	}
	st, err := opjournal.NewLaneStore(beads.SessionStore{Store: cliSessionStore(s.store, s.cfg, s.cityPath)}, cliOpClock)
	if err != nil {
		return s.refuseUnavailable(err), true
	}
	req := s.request()
	req.Conversation, req.Kind, req.Executor = s.sessionID, opjournal.KindMessage, opjournal.ExecutorLane
	want, err := st.Preview(req)
	if err != nil {
		return s.refuseProduce(cliLaneRefusal(err)), true
	}
	op, err := st.Get(want.ID)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return 0, false
	case err != nil:
		return s.refuseUnavailable(err), true
	case op.Executor != opjournal.ExecutorLane:
		// The legacy path answers its own op under the key (cliOp.start).
		return 0, false
	case op.PayloadDigest != want.PayloadDigest:
		return s.refuseProduce(&opjournal.KeyReuseError{Existing: op}), true
	case op.Status != opjournal.StatusClosed:
		if !s.resumeHeld(op) {
			return 1, true
		}
		return emitSessionSubmitAccepted(s.stdout, s.stderr, s.target, s.intent, op.ID, s.key, s.asJSON), true
	}
	return s.report(cliLaneProduced{handled: true, op: op, closedBefore: true}), true
}

// report reports the lane op prod proposed or replayed, as gc session nudge
// reports its own: an op that closed without success before this run is
// refused as already failed, one that closed without success since is
// refused as failed, and any other is delivered or queued.
func (s sessionSubmitLane) report(prod cliLaneProduced) int {
	op := prod.op
	arm, _ := op.Outcome.Effective()
	failed := op.Status == opjournal.StatusClosed && arm != opjournal.ArmSucceeded
	if failed && prod.closedBefore {
		return s.refuse(cliOpCodeAlreadyFailed, fmt.Sprintf("op %s closed %s; rerun without --key to act again", op.ID, cliOpOutcomeText(op.Outcome)), op.ID)
	}
	if !prod.created {
		fmt.Fprintf(s.stderr, "gc session submit: replayed op %s\n", op.ID) //nolint:errcheck // best-effort stderr
	}
	if failed {
		return s.refuse(cliOpCodeFailed, fmt.Sprintf("op %s closed %s", op.ID, cliOpOutcomeText(op.Outcome)), op.ID)
	}
	return emitSessionSubmitResult(s.stdout, s.stderr, s.target, s.intent, op.Status != opjournal.StatusClosed, s.asJSON,
		sessionSubmitOp{id: op.ID, key: s.key, replayed: !prod.created})
}

// replayLegacy answers a key that names a legacy op, which the lane never
// takes, closes or rewrites, as the API's lane path and gc session nudge's
// answer one: an open op is in progress under its legacy path, one that
// closed without success is refused, and any other replays its outcome,
// queued when its legacy path only queued the message.
func (s sessionSubmitLane) replayLegacy(op opjournal.Op) int {
	switch {
	case op.Status != opjournal.StatusClosed:
		return s.refuse(cliOpCodeInProgress, fmt.Sprintf("op %s is in progress under its legacy path; rerun later or without --key", op.ID), op.ID)
	case !cliOpReplaysAsSuccess(op.Kind, op.Outcome):
		return s.refuse(cliOpCodeAlreadyFailed, fmt.Sprintf("op %s closed %s; rerun without --key to act again", op.ID, cliOpOutcomeText(op.Outcome)), op.ID)
	}
	fmt.Fprintf(s.stderr, "gc session submit: replayed op %s\n", op.ID) //nolint:errcheck // best-effort stderr
	return emitSessionSubmitResult(s.stdout, s.stderr, s.target, s.intent, op.Outcome.Result.Via == legacyop.ViaLegacyQueued, s.asJSON,
		sessionSubmitOp{id: op.ID, key: s.key, replayed: true})
}

// refuseProduce reports the refusal cliLaneProduce answered: a key that names
// a different request is a mismatch, an invalid --key is reported as one,
// and any other refusal is a request the op journal cannot record.
func (s sessionSubmitLane) refuseProduce(err error) int {
	var reuse *opjournal.KeyReuseError
	if errors.As(err, &reuse) {
		return s.refuse(cliOpCodeKeyMismatch, fmt.Sprintf("the key names a different request (op %s); rerun with another --key", reuse.Existing.ID), reuse.Existing.ID)
	}
	if kerr := opjournal.ValidateClientKey(s.key); kerr != nil {
		return s.refuse(cliOpCodeInvalidKey, fmt.Sprintf("invalid --key (%v)", kerr), "")
	}
	var refused *cliLaneRefusalError
	if errors.As(err, &refused) {
		return s.refuseUnavailable(refused.cause)
	}
	return s.refuseUnavailable(err)
}

// refusal is the CLI op refusal s reports through, so its text and --json
// failure envelope are gc session submit's legacy path's (cliOp.refuse).
func (s sessionSubmitLane) refusal() *cliOp {
	return &cliOp{req: cliOpRequest{verb: "gc session submit", kind: opjournal.KindMessage, asJSON: s.asJSON, stdout: s.stdout, stderr: s.stderr}}
}

// refuse reports a refusal of s: on stderr, naming the op and the key, and
// under --json as the failure envelope. It returns the exit code.
func (s sessionSubmitLane) refuse(code, message, opID string) int {
	s.refusal().refuse(code, message, opID, s.key)
	return 1
}

// refuseUnavailable refuses s as a request the op journal cannot record for
// cause.
func (s sessionSubmitLane) refuseUnavailable(cause any) int {
	return s.refuse(cliOpCodeUnavailable, s.refusal().unavailableMessage(cause), "")
}

// prefixWriter writes each write to w behind prefix.
type prefixWriter struct {
	prefix string
	w      io.Writer
}

func (p prefixWriter) Write(b []byte) (int, error) {
	if _, err := io.WriteString(p.w, p.prefix); err != nil {
		return 0, err
	}
	return p.w.Write(b)
}
