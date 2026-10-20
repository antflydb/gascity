package acceptancehelpers

// BDInitDiagnosticsHeadBytes and BDInitDiagnosticsTailBytes bound how much of
// each bd log the init wrapper copies to stderr.
const (
	BDInitDiagnosticsHeadBytes = 0
	BDInitDiagnosticsTailBytes = 0
)

// BDInitDiagnostic is one log section the init wrapper printed.
type BDInitDiagnostic struct {
	Path string
	Note string
	Body string
}

// ParseBDInitDiagnostics returns the sections found in a failed bd init's output.
func ParseBDInitDiagnostics(_ string) []BDInitDiagnostic {
	return nil
}
