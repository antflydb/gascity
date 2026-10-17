package config

import "testing"

func TestParseSessionEscalationTo(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test"

[session]
escalation_to = "ops/oncall"

[[agent]]
name = "a"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Session.EscalationTo; got != "ops/oncall" {
		t.Errorf("EscalationTo = %q, want ops/oncall", got)
	}
}

// An unset escalation_to must stay empty: nothing may default it to a role
// name, and the empty value is what makes the escalator log and skip.
func TestSessionEscalationToDefaultsToEmpty(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test"

[[agent]]
name = "a"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Session.EscalationTo != "" {
		t.Errorf("EscalationTo = %q, want empty", cfg.Session.EscalationTo)
	}
	if cfg.Session.EscalationRecipient() != "" {
		t.Errorf("EscalationRecipient() = %q, want empty", cfg.Session.EscalationRecipient())
	}
}

func TestSessionEscalationRecipientTrimsSurroundingSpace(t *testing.T) {
	s := SessionConfig{EscalationTo: "  ops/oncall \n"}
	if got := s.EscalationRecipient(); got != "ops/oncall" {
		t.Errorf("EscalationRecipient() = %q, want ops/oncall", got)
	}
}
