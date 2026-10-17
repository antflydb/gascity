package session

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func newAlertTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(beads.SessionStore{Store: beads.NewMemStore()})
}

// A terminal escalation outcome must survive further failure accrual: the
// threshold crossing is the one edge that may start an escalation, so a
// sending, sent, skipped, or failed episode is never put back to pending.
func TestRecordStartupFailureKeepsEveryNonNoneAlertDisposition(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, disp := range []AlertDisposition{
		AlertDispositionPending,
		AlertDispositionSending,
		AlertDispositionSent,
		AlertDispositionSkipped,
		AlertDispositionFailed,
	} {
		t.Run(string(disp), func(t *testing.T) {
			prior := StartupHealthEpisode{SessionName: "s", ConsecutiveCount: 5, AlertDisposition: disp}
			next := RecordStartupFailure(prior, FailureKindOther, "boom", now, 3, time.Hour)
			if next.AlertDisposition != disp {
				t.Fatalf("AlertDisposition = %q, want %q kept through accrual", next.AlertDisposition, disp)
			}
		})
	}
}

func TestRecordStartupFailureEscalatesToPendingOnlyFromNone(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	prior := StartupHealthEpisode{SessionName: "s", ConsecutiveCount: 1}
	below := RecordStartupFailure(prior, FailureKindOther, "x", now, 3, time.Hour)
	if below.AlertDisposition != AlertDispositionNone {
		t.Fatalf("below threshold disposition = %q, want none", below.AlertDisposition)
	}
	at := RecordStartupFailure(below, FailureKindOther, "x", now, 3, time.Hour)
	if at.AlertDisposition != AlertDispositionPending {
		t.Fatalf("at threshold disposition = %q, want pending", at.AlertDisposition)
	}
}

func TestStartupDeathFailureKindValue(t *testing.T) {
	if FailureKindStartupDeath != "startup_death" {
		t.Fatalf("FailureKindStartupDeath = %q, want startup_death", FailureKindStartupDeath)
	}
}

func TestClearStartupHealthEpisodeRearmsAlert(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ep := RecordStartupFailure(StartupHealthEpisode{SessionName: "s", ConsecutiveCount: 2, AlertDisposition: AlertDispositionSent}, FailureKindOther, "x", now, 3, time.Hour)
	if ep.AlertDisposition != AlertDispositionSent {
		t.Fatalf("setup: disposition = %q, want sent", ep.AlertDisposition)
	}
	cleared := ClearStartupHealthEpisode("s")
	next := RecordStartupFailure(cleared, FailureKindOther, "x", now, 1, time.Hour)
	if next.AlertDisposition != AlertDispositionPending {
		t.Fatalf("episode after clear: disposition = %q, want pending", next.AlertDisposition)
	}
}

func TestSetStartupHealthAlertTransitionsOnlyTheAlertKey(t *testing.T) {
	s := newAlertTestStore(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ep := StartupHealthEpisode{
		SessionName:      "s",
		ConsecutiveCount: 4,
		FirstFailureAt:   now.Add(-time.Hour),
		LastFailureAt:    now,
		LastDetail:       "detail",
		Kind:             FailureKindStartupDeath,
		AlertDisposition: AlertDispositionPending,
		QuarantinedUntil: now.Add(time.Hour),
	}
	if err := s.SaveStartupHealthEpisode(ep); err != nil {
		t.Fatalf("SaveStartupHealthEpisode: %v", err)
	}
	if err := s.SetStartupHealthAlert("s", AlertDispositionPending, AlertDispositionSending); err != nil {
		t.Fatalf("SetStartupHealthAlert: %v", err)
	}
	got, err := s.LoadStartupHealthEpisode("s")
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	want := ep
	want.AlertDisposition = AlertDispositionSending
	if got != want {
		t.Fatalf("episode = %+v, want %+v", got, want)
	}
}

// The compare-and-set is what keeps a replayed or concurrent observation of
// the same pending snapshot from claiming the escalation twice.
func TestSetStartupHealthAlertRejectsStaleExpectation(t *testing.T) {
	s := newAlertTestStore(t)
	if err := s.SaveStartupHealthEpisode(StartupHealthEpisode{SessionName: "s", ConsecutiveCount: 3, AlertDisposition: AlertDispositionPending}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStartupHealthAlert("s", AlertDispositionPending, AlertDispositionSending); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	err := s.SetStartupHealthAlert("s", AlertDispositionPending, AlertDispositionSending)
	if !errors.Is(err, ErrStartupHealthAlertConflict) {
		t.Fatalf("second claim error = %v, want ErrStartupHealthAlertConflict", err)
	}
	got, _ := s.LoadStartupHealthEpisode("s")
	if got.AlertDisposition != AlertDispositionSending {
		t.Fatalf("disposition = %q, want sending untouched", got.AlertDisposition)
	}
}

func TestSetStartupHealthAlertMissingEpisodeIsAConflict(t *testing.T) {
	s := newAlertTestStore(t)
	err := s.SetStartupHealthAlert("absent", AlertDispositionPending, AlertDispositionSending)
	if !errors.Is(err, ErrStartupHealthAlertConflict) {
		t.Fatalf("error = %v, want ErrStartupHealthAlertConflict", err)
	}
	if eps, _ := s.ListStartupHealthEpisodes(); len(eps) != 0 {
		t.Fatalf("a missing episode must not be created, got %d", len(eps))
	}
}

func TestSetStartupHealthAlertPropagatesWriteFailure(t *testing.T) {
	mem := beads.NewMemStore()
	s := NewStore(beads.SessionStore{Store: mem})
	if err := s.SaveStartupHealthEpisode(StartupHealthEpisode{SessionName: "s", ConsecutiveCount: 3, AlertDisposition: AlertDispositionPending}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("write refused")
	bad := NewStore(beads.SessionStore{Store: &erroringStore{Store: mem, setErr: boom}})
	err := bad.SetStartupHealthAlert("s", AlertDispositionPending, AlertDispositionSending)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the write error", err)
	}
	if errors.Is(err, ErrStartupHealthAlertConflict) {
		t.Fatal("a write failure must not read as a conflict")
	}
}

func TestListStartupHealthEpisodesByAlertFiltersExactly(t *testing.T) {
	s := newAlertTestStore(t)
	for name, disp := range map[string]AlertDisposition{
		"a": AlertDispositionPending,
		"b": AlertDispositionSent,
		"c": AlertDispositionPending,
		"d": AlertDispositionNone,
	} {
		if err := s.SaveStartupHealthEpisode(StartupHealthEpisode{SessionName: name, ConsecutiveCount: 3, AlertDisposition: disp}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListStartupHealthEpisodesByAlert(AlertDispositionPending)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, ep := range got {
		names[ep.SessionName] = true
	}
	if len(got) != 2 || !names["a"] || !names["c"] {
		t.Fatalf("pending episodes = %+v, want exactly a and c", got)
	}
}
