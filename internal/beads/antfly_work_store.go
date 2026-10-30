package beads

import (
	"context"
	"path/filepath"

	"github.com/steveyegge/beads/backend"
	"github.com/steveyegge/beads/backend/antfly"
)

// AntflyWorkStore reuses the backend-neutral issue-role adapter for the native
// work plane. It holds one SDK handle rather than spawning bd for each read.
// Provider process lifecycle and infrastructure storage remain separate owners.
type AntflyWorkStore struct {
	*NativeDoltStore
	workspace antfly.Workspace
}

// OpenAntflyWorkStoreAt opens the existing, identity-checked native work workspace.
func OpenAntflyWorkStoreAt(ctx context.Context, scopeRoot string) (*AntflyWorkStore, error) {
	storage, err := antfly.Open(ctx, filepath.Join(scopeRoot, ".beads"), backend.OpenOptions{})
	if err != nil {
		return nil, err
	}
	prefix, err := storage.GetConfig(ctx, "issue_prefix")
	if err != nil {
		_ = storage.Close()
		return nil, err
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "", prefix)
	return &AntflyWorkStore{NativeDoltStore: store, workspace: storage}, nil
}

// Close preserves the executable provider's guarded work-close semantics.
// The shared role adapter's policy-free orchestrator close is not appropriate
// here: work-plane blockers and assignment fences remain engine-owned.
func (s *AntflyWorkStore) Close(id string) error {
	ctx, cancel := nativeDoltOperationContext(context.Background())
	defer cancel()
	_, err := s.workspace.ExecuteFactory(ctx, "close", []string{id}, nil, s.actor)
	return nativeStoreError(id, err)
}

// CloseAll applies guarded work closes and reports any partial completion.
func (s *AntflyWorkStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		if len(metadata) > 0 {
			if err := s.Update(id, UpdateOpts{Metadata: metadata}); err != nil {
				return closed, err
			}
		}
		if err := s.Close(id); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}
