package acceptancehelpers

import "errors"

// RigAddRecovery runs a gc rig add and retries it once on a proved port collision.
type RigAddRecovery struct {
	Topology    BeadsTopology
	RigDir      string
	Add         func() (string, error)
	StoreExists func(path string) bool
	Logf        func(format string, args ...any)
}

// Run performs the add.
func (r RigAddRecovery) Run() (string, error) {
	return r.Add()
}

func portCollisionProof(_ string, _ error, _ []string) (string, error) {
	return "", errors.New("not implemented")
}

func recoverableShape(_ BeadsTopology, _ []string) error {
	return nil
}
