package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	// startupHealthEscalationSender is the SDK identity escalation mail is
	// sent from; it is not a configured role.
	startupHealthEscalationSender = "controller"

	// startupHealthEscalationTailBytes bounds the start-stderr.log tail
	// attached to an escalation.
	startupHealthEscalationTailBytes = 4096

	// startupHealthEscalationRedactMargin is how much extra log is read ahead of
	// the tail so a secret straddling the tail's start is redacted whole before
	// the tail is cut.
	startupHealthEscalationRedactMargin = 4096

	startupStderrLogName = "start-stderr.log"
)

// startupHealthEscalator sends one escalation per startup-health episode.
//
// The episode's persisted alert disposition is the one-shot guard. A sweep
// claims a pending episode by durably moving it to sending BEFORE the mail is
// sent, then records sent or failed. A crash between the claim and the
// outcome therefore leaves the episode in sending: the alert may be lost, but
// it is never sent twice, and doctor shows the stuck state.
type startupHealthEscalator struct {
	store    *session.Store
	mail     mail.Provider
	to       string
	cityPath string
	now      func() time.Time
	stderr   io.Writer

	mu sync.Mutex
}

// newStartupHealthEscalator builds the escalator for one controller tick. The
// mail provider is the running controller's class-routed city provider; a nil
// controller state yields no provider, and an escalation then records failed.
func newStartupHealthEscalator(store *session.Store, cs *controllerState, cfg *config.City, cityPath string, stderr io.Writer) *startupHealthEscalator {
	e := &startupHealthEscalator{
		store:    store,
		cityPath: cityPath,
		now:      time.Now,
		stderr:   stderr,
	}
	if cs != nil {
		e.mail = cs.MailProvider("")
	}
	if cfg != nil {
		e.to = cfg.Session.EscalationRecipient()
	}
	return e
}

// sweep escalates every episode that has crossed the threshold and has not yet
// been claimed.
func (e *startupHealthEscalator) sweep() {
	if e == nil || e.store == nil {
		return
	}
	pending, err := e.store.ListStartupHealthEpisodesByAlert(session.AlertDispositionPending)
	if err != nil {
		e.logf("startup-health escalation: listing pending episodes: %v", err)
		return
	}
	for _, ep := range pending {
		e.escalate(ep)
	}
}

// escalate acts on one observed pending episode. The observation may be stale
// or replayed: only the winner of the pending-to-next compare-and-set acts.
func (e *startupHealthEscalator) escalate(ep session.StartupHealthEpisode) {
	e.mu.Lock()
	defer e.mu.Unlock()
	name := ep.SessionName

	switch {
	case e.to == "":
		e.settle(ep, session.AlertDispositionSkipped, "no [session].escalation_to configured")
		return
	case !validEscalationRecipient(e.to):
		e.settle(ep, session.AlertDispositionFailed, fmt.Sprintf("invalid [session].escalation_to %q", e.to))
		return
	case e.mail == nil:
		e.settle(ep, session.AlertDispositionFailed, "mail provider unavailable")
		return
	}

	if err := e.store.SetStartupHealthAlert(name, session.AlertDispositionPending, session.AlertDispositionSending); err != nil {
		if !isAlertConflict(err) {
			e.logf("startup-health escalation for session %q not claimed, retrying next tick: %v", name, err)
		}
		return
	}

	subject, body := e.compose(ep)
	if _, err := e.mail.Send(startupHealthEscalationSender, e.to, subject, body); err != nil {
		e.record(name, session.AlertDispositionFailed, fmt.Sprintf("sending to %q failed: %v", e.to, err), ep)
		return
	}
	e.record(name, session.AlertDispositionSent, fmt.Sprintf("sent to %q", e.to), ep)
}

// settle records a terminal outcome for an episode that never reached the send:
// the transition is pending to the outcome, with no claim.
func (e *startupHealthEscalator) settle(ep session.StartupHealthEpisode, outcome session.AlertDisposition, reason string) {
	if err := e.store.SetStartupHealthAlert(ep.SessionName, session.AlertDispositionPending, outcome); err != nil {
		if !isAlertConflict(err) {
			e.logf("startup-health escalation for session %q not recorded as %s, retrying next tick: %v", ep.SessionName, outcome, err)
		}
		return
	}
	e.logf("startup-health escalation %s for session %q (%d consecutive %s failures): %s",
		outcome, ep.SessionName, ep.ConsecutiveCount, kindLabel(ep.Kind), reason)
}

// record writes the outcome of a claimed escalation. When the write fails the
// episode stays sending, which keeps any later tick or restart from sending a
// second copy.
func (e *startupHealthEscalator) record(name string, outcome session.AlertDisposition, reason string, ep session.StartupHealthEpisode) {
	if err := e.store.SetStartupHealthAlert(name, session.AlertDispositionSending, outcome); err != nil {
		e.logf("startup-health escalation for session %q was %s (%s) but the outcome was not recorded; episode stays sending: %v", name, outcome, reason, err)
		return
	}
	e.logf("startup-health escalation %s for session %q (%d consecutive %s failures): %s",
		outcome, name, ep.ConsecutiveCount, kindLabel(ep.Kind), reason)
}

func (e *startupHealthEscalator) compose(ep session.StartupHealthEpisode) (subject, body string) {
	subject = fmt.Sprintf("Session %s keeps failing to start (%d consecutive)", ep.SessionName, ep.ConsecutiveCount)
	var b strings.Builder
	fmt.Fprintf(&b, "Session: %s\n", ep.SessionName)
	fmt.Fprintf(&b, "Consecutive startup failures: %d\n", ep.ConsecutiveCount)
	fmt.Fprintf(&b, "Failure kind: %s\n", kindLabel(ep.Kind))
	fmt.Fprintf(&b, "First failure: %s\n", ep.FirstFailureAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "Last failure: %s\n", ep.LastFailureAt.Format(time.RFC3339))
	if ep.Kind == session.FailureKindStartupDeath {
		b.WriteString("\n")
		b.WriteString(e.stderrSection(ep.SessionName))
	}
	return subject, b.String()
}

// stderrSection renders the redacted tail of the session's start-stderr.log, or
// a one-line note when it cannot be read.
func (e *startupHealthEscalator) stderrSection(name string) string {
	path, ok := startupStderrPath(e.cityPath, name)
	if !ok {
		return startupStderrLogName + " unavailable: session name is not a plain directory name\n"
	}
	tail, err := readStartupStderrTail(path, startupHealthEscalationTailBytes)
	switch {
	case os.IsNotExist(err):
		return startupStderrLogName + " unavailable: not found\n"
	case err != nil:
		return startupStderrLogName + " unavailable: unreadable\n"
	case tail == "":
		return startupStderrLogName + " is empty\n"
	}
	return fmt.Sprintf("Tail of %s (last %d bytes at most, secrets redacted):\n%s\n", startupStderrLogName, startupHealthEscalationTailBytes, tail)
}

func (e *startupHealthEscalator) logf(format string, args ...any) {
	if e.stderr == nil {
		return
	}
	fmt.Fprintf(e.stderr, format+"\n", args...) //nolint:errcheck // best-effort log
}

// startupStderrPath resolves the session's start-stderr.log under the city's
// session diagnostics directory. A name that is not a single path element is
// refused so it cannot reach outside that directory.
func startupStderrPath(cityPath, sessionName string) (string, bool) {
	if sessionName == "" || !filepath.IsLocal(sessionName) || strings.ContainsAny(sessionName, `/\`) {
		return "", false
	}
	return filepath.Join(citylayout.SessionDiagnosticsDir(cityPath), sessionName, startupStderrLogName), true
}

// readStartupStderrTail returns at most limit bytes from the end of the file,
// with secrets redacted and without splitting a UTF-8 sequence. The redaction
// runs over a wider window than the returned tail so a secret that straddles
// the cut is replaced whole rather than left half visible.
func readStartupStderrTail(path string, limit int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	window := int64(limit + startupHealthEscalationRedactMargin)
	start := int64(0)
	if info.Size() > window {
		start = info.Size() - window
	}
	buf := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return "", err
	}
	text := runtime.RedactSecrets(runeAligned(buf, start > 0), runtime.SetupCommandSecrets(nil))
	if len(text) > limit {
		text = runeAligned([]byte(text[len(text)-limit:]), true)
	}
	return text, nil
}

// runeAligned converts b to a string; when b begins mid-file, bytes that are
// the tail of a split multi-byte sequence are dropped from the front.
func runeAligned(b []byte, midFile bool) string {
	if midFile {
		for len(b) > 0 && !utf8.RuneStart(b[0]) {
			b = b[1:]
		}
	}
	return strings.ToValidUTF8(string(b), "")
}

// validEscalationRecipient reports whether to is a single mail address token:
// non-empty, with no whitespace or control characters.
func validEscalationRecipient(to string) bool {
	if to == "" {
		return false
	}
	for _, r := range to {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func kindLabel(k session.FailureKind) string {
	if k == session.FailureKindUnspecified {
		return "unspecified"
	}
	return string(k)
}

func isAlertConflict(err error) bool {
	return errors.Is(err, session.ErrStartupHealthAlertConflict)
}

// tickEscalateStartupHealth sends the one escalation owed to each episode that
// crossed the startup-health threshold. It runs under both reconcilers: the
// episode record is the only input.
func (cr *CityRuntime) tickEscalateStartupHealth(p *tickPass) bool {
	phaseStart := time.Now()
	if store := cr.v2SessionsStore(); store != nil {
		newStartupHealthEscalator(sessionFrontDoor(store), cr.cs, cr.serviceConfigSnapshot(), cr.cityPath, cr.stderr).sweep()
	}
	p.recordPhase(TraceSiteControllerTickPhase, "escalate_startup_health", phaseStart, nil)
	return false
}
