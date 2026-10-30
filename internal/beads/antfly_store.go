package beads

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/beads/backend"
	antfly "github.com/steveyegge/beads/backend/antfly"
)

// AntflyStore persists Gas City coordination records in a dedicated Antfly
// workspace. MemStore supplies transaction-local domain semantics; the Antfly
// workspace head is the only publication point, including multi-record writes.
type AntflyStore struct {
	condWritesStamp
	mu         sync.Mutex
	transport  antfly.SnapshotTransport
	prefix     string
	namespaces []string
	closed     bool
	local      map[string]map[string]string
}
type antflyRecord struct {
	Bead     Bead  `json:"bead"`
	Revision int64 `json:"revision"`
	Fence    int64 `json:"fence"`
}
type antflyState struct {
	Schema       string                  `json:"schema"`
	Prefix       string                  `json:"prefix"`
	Seq          int                     `json:"seq"`
	Records      map[string]antflyRecord `json:"records"`
	Dependencies map[string]Dep          `json:"dependencies"`
	EdgeMetadata map[string]string       `json:"edge_metadata"`
	Revisions    map[string]int64        `json:"revisions"`
	Fences       map[string]int64        `json:"fences"`
	IndexVersion int                     `json:"index_version,omitempty"`
	Indexes      map[string][]string     `json:"indexes,omitempty"`
}

const antflySchema = "gascity-antfly-v1"

// OpenAntflyStore opens an embedded or remote coordination workspace.
func OpenAntflyStore(target antfly.Target, prefix string, namespaces ...string) (*AntflyStore, error) {
	tr, err := antfly.OpenSnapshot(context.Background(), target, backend.OpenOptions{})
	if err != nil {
		return nil, err
	}
	s, err := NewAntflyStore(tr, prefix, namespaces...)
	if err != nil {
		_ = tr.Close()
	}
	return s, err
}

// NewAntflyStore takes ownership of a snapshot transport on success. Its workspace must
// be empty or already carry this schema and prefix; core Beads is a separate workspace.
func NewAntflyStore(tr antfly.SnapshotTransport, prefix string, namespaces ...string) (*AntflyStore, error) {
	if tr == nil {
		return nil, fmt.Errorf("antfly: nil snapshot transport")
	}
	prefix = normalizeIDPrefix(prefix)
	if prefix == "" {
		return nil, fmt.Errorf("antfly: empty prefix")
	}
	s := &AntflyStore{transport: tr, prefix: prefix, namespaces: collectReservedIDPrefixes(nil, namespaces), local: map[string]map[string]string{}}
	_, _, _, err := s.load(context.Background())
	// An interrupted collector owns a durable head fence. Keep an operator
	// handle so checked collection can resume; ordinary reads and writes still
	// fail closed until the fence is released. The collector validates the
	// schema before adopting the fence, including for a newly opened handle.
	if errors.Is(err, antfly.ErrSnapshotMaintenance) {
		return s, nil
	}
	return s, err
}

// IDPrefix reports the prefix used for minted coordination IDs.
func (s *AntflyStore) IDPrefix() string { return s.prefix }

// CloseStore releases the native or remote transport exactly once.
func (s *AntflyStore) CloseStore() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.transport.Close()
}

func (s *AntflyStore) load(ctx context.Context) (*antflyState, *MemStore, uint64, error) {
	if s.closed {
		return nil, nil, 0, fmt.Errorf("antfly: store closed")
	}
	data, v, err := s.transport.Load(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	st := &antflyState{Schema: antflySchema, Prefix: s.prefix, Records: map[string]antflyRecord{}, Dependencies: map[string]Dep{}, EdgeMetadata: map[string]string{}, Revisions: map[string]int64{}, Fences: map[string]int64{}}
	if len(data) > 0 && string(data) != "null" && string(data) != "{}" {
		if err = json.Unmarshal(data, st); err != nil {
			return nil, nil, 0, err
		}
		if st.Schema != antflySchema || st.Prefix != s.prefix {
			return nil, nil, 0, fmt.Errorf("antfly: workspace schema/prefix mismatch")
		}
	}
	if st.Records == nil {
		st.Records = map[string]antflyRecord{}
	}
	if st.Dependencies == nil {
		st.Dependencies = map[string]Dep{}
	}
	if st.EdgeMetadata == nil {
		st.EdgeMetadata = map[string]string{}
	}
	if st.Revisions == nil {
		st.Revisions = map[string]int64{}
	}
	if st.Fences == nil {
		st.Fences = map[string]int64{}
	}
	rows := make([]Bead, 0, len(st.Records))
	for id, r := range st.Records {
		if id != r.Bead.ID {
			return nil, nil, 0, fmt.Errorf("antfly: corrupt record key")
		}
		b := r.Bead
		b.Revision = r.Revision
		b.ClaimFence = r.Fence
		rows = append(rows, b)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	keys := make([]string, 0, len(st.Dependencies))
	for k := range st.Dependencies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	deps := make([]Dep, 0, len(keys))
	for _, k := range keys {
		deps = append(deps, st.Dependencies[k])
	}
	m := NewMemStoreFrom(st.Seq, rows, deps)
	m.IDPrefix = s.prefix
	m.HonorExplicitIDs = true
	m.storageOnly = true
	return st, m, v, nil
}

func (s *AntflyStore) read(ctx context.Context) (*MemStore, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, m, _, err := s.load(ctx)
	return m, err
}

// change retries only known rejected commits. A user callback is never replayed;
// unknown commit outcomes propagate so callers reconcile by reading durable state.
func (s *AntflyStore) change(ctx context.Context, retry bool, fn func(*MemStore, *antflyState) error) (*MemStore, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for attempt := 0; attempt < 32; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, m, v, err := s.load(ctx)
		if err != nil {
			return nil, err
		}
		before := maps.Clone(st.Records)
		if err = fn(m, st); err != nil {
			return nil, err
		}
		m.mu.Lock()
		seq, rows, deps := m.snapshot()
		m.mu.Unlock()
		st.Seq = seq
		st.Records = make(map[string]antflyRecord, len(rows))
		for i, b := range rows {
			if _, exists := before[b.ID]; !exists {
				if floor, ok := st.Revisions[b.ID]; ok {
					b.Revision = floor + 1
					b.ClaimFence = st.Fences[b.ID] + 1
				}
				rows[i] = b
			}
			st.Records[b.ID] = antflyRecord{b, b.Revision, b.ClaimFence}
			st.Revisions[b.ID] = b.Revision
			st.Fences[b.ID] = b.ClaimFence
		}
		st.Dependencies = make(map[string]Dep, len(deps))
		for _, d := range deps {
			st.Dependencies[antflyEdgeKey(d)] = d
		}
		for k := range st.EdgeMetadata {
			if _, ok := st.Dependencies[k]; !ok {
				delete(st.EdgeMetadata, k)
			}
		}
		st.buildIndexes()
		raw, err := json.Marshal(st)
		if err != nil {
			return nil, err
		}
		err = s.transport.CompareAndSwap(ctx, v, raw)
		if err != nil {
			if retry && errors.Is(err, antfly.ErrSnapshotConflict) {
				continue
			}
			return nil, err
		}
		m.restoreFrom(seq, rows, deps)
		for id := range before {
			if _, exists := st.Records[id]; !exists {
				delete(s.local, id)
			}
		}
		for _, b := range rows {
			old, exists := before[b.ID]
			if !exists {
				noteSessionCreate(b)
			} else if !maps.Equal(old.Bead.Metadata, b.Metadata) {
				noteSessionKeys(b, b.Metadata)
			}
		}
		return m, nil
	}
	return nil, fmt.Errorf("antfly: contention retry limit: %w", antfly.ErrSnapshotConflict)
}

func antflyEdgeKey(d Dep) string {
	b, _ := json.Marshal([]string{d.IssueID, d.DependsOnID, d.Type})
	return string(b)
}

func (s *AntflyStore) checkCreate(m *MemStore, b Bead, foreign bool) (Bead, error) {
	if !foreign {
		if err := checkPinnedIDNamespace("antfly create", b.ID, s.namespaces); err != nil {
			return Bead{}, err
		}
	}
	if b.ID != "" {
		if strings.TrimSpace(b.ID) != b.ID {
			return Bead{}, fmt.Errorf("antfly: invalid pinned ID")
		}
		if _, err := m.Get(b.ID); err == nil {
			return Bead{}, fmt.Errorf("%w: %s", ErrAlreadyExists, b.ID)
		}
	}
	return m.Create(b)
}

// Create atomically creates one record.
func (s *AntflyStore) Create(b Bead) (Bead, error) { return s.create(b, false) }

// CreateWithForeignID preserves a migration record's ID outside the serving fence.
func (s *AntflyStore) CreateWithForeignID(b Bead) (Bead, error) { return s.create(b, true) }

func (s *AntflyStore) create(b Bead, foreign bool) (Bead, error) {
	var id string
	m, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error { r, e := s.checkCreate(m, b, foreign); id = r.ID; return e })
	if err != nil {
		return Bead{}, err
	}
	return m.Get(id)
}

// CreateWithStorage applies the explicitly requested storage policy.
func (s *AntflyStore) CreateWithStorage(b Bead, storage StorageClass) (Bead, error) {
	if storage != StorageDefault {
		e, n, err := graphStorageFlags(storage)
		if err != nil {
			return Bead{}, err
		}
		b.Ephemeral = e
		b.NoHistory = n
	}
	return s.Create(b)
}

// Get reads a current committed Antfly snapshot.
func (s *AntflyStore) Get(id string) (Bead, error) {
	if tr, ok := s.transport.(antfly.IndexedSnapshotTransport); ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return Bead{}, fmt.Errorf("antfly: store closed")
		}
		got, e := tr.ReadSnapshotRecords(context.Background(), "records", []string{id}, nil)
		if e != nil {
			return Bead{}, e
		}
		if e = s.validateIndexedFields(got.Fields); e != nil {
			return Bead{}, e
		}
		raw, ok := got.Records[id]
		if !ok {
			return Bead{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return decodeIndexedAntflyRecord(id, raw)
	}
	m, err := s.read(context.Background())
	if err != nil {
		return Bead{}, err
	}
	return m.Get(id)
}

// List reads a current committed Antfly snapshot.
func (s *AntflyStore) List(query ListQuery) ([]Bead, error) {
	if rows, used, e := s.indexedList(query); used {
		return rows, e
	}
	m, err := s.read(context.Background())
	if err != nil {
		return nil, err
	}
	return m.List(query)
}

// ListOpen reads a current committed Antfly snapshot.
func (s *AntflyStore) ListOpen(status ...string) ([]Bead, error) {
	m, err := s.read(context.Background())
	if err != nil {
		return nil, err
	}
	return m.ListOpen(status...)
}

// Ready reads a current committed Antfly snapshot.
func (s *AntflyStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	m, err := s.read(context.Background())
	if err != nil {
		return nil, err
	}
	return m.Ready(query...)
}

// Children reads a current committed Antfly snapshot.
func (s *AntflyStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{ParentID: parentID, IncludeClosed: HasOpt(opts, IncludeClosed), Sort: SortCreatedAsc, TierMode: TierModeFromOpts(opts)})
}

// ListByLabel reads a current committed Antfly snapshot.
func (s *AntflyStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{Label: label, Limit: limit, IncludeClosed: HasOpt(opts, IncludeClosed), Sort: SortCreatedDesc, TierMode: TierModeFromOpts(opts)})
}

// ListByAssignee reads a current committed Antfly snapshot.
func (s *AntflyStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return s.List(ListQuery{Assignee: assignee, Status: status, Limit: limit, Sort: SortCreatedDesc})
}

// ListByMetadata reads a current committed Antfly snapshot.
func (s *AntflyStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{Metadata: filters, Limit: limit, IncludeClosed: HasOpt(opts, IncludeClosed), Sort: SortCreatedDesc, TierMode: TierModeFromOpts(opts)})
}

// DepList reads a current committed Antfly snapshot.
func (s *AntflyStore) DepList(id, direction string) ([]Dep, error) {
	m, err := s.read(context.Background())
	if err != nil {
		return nil, err
	}
	return m.DepList(id, direction)
}

// DepListBatch reads a current committed Antfly snapshot.
func (s *AntflyStore) DepListBatch(ids []string) (map[string][]Dep, error) {
	m, err := s.read(context.Background())
	if err != nil {
		return nil, err
	}
	return m.DepListBatch(ids)
}

// Update publishes one conditional snapshot mutation.
func (s *AntflyStore) Update(id string, opts UpdateOpts) error {
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error { return m.Update(id, opts) })
	return err
}

// Close publishes one conditional snapshot mutation.
func (s *AntflyStore) Close(id string) error {
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error { return m.Close(id) })
	return err
}

// Reopen publishes one conditional snapshot mutation.
func (s *AntflyStore) Reopen(id string) error {
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error { return m.Reopen(id) })
	return err
}

// Delete publishes one conditional snapshot mutation.
func (s *AntflyStore) Delete(id string) error {
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error { return m.Delete(id) })
	return err
}

// SetMetadata publishes one conditional snapshot mutation.
func (s *AntflyStore) SetMetadata(id, key, value string) error {
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error { return m.SetMetadata(id, key, value) })
	return err
}

// SetMetadataBatch publishes one conditional snapshot mutation.
func (s *AntflyStore) SetMetadataBatch(id string, kvs map[string]string) error {
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error { return m.SetMetadataBatch(id, kvs) })
	return err
}

// DepAdd publishes one conditional snapshot mutation.
func (s *AntflyStore) DepAdd(issueID, dependsOnID, depType string) error {
	return s.DepAddWithMetadata(issueID, dependsOnID, depType, "")
}

// DepRemove publishes one conditional snapshot mutation.
func (s *AntflyStore) DepRemove(issueID, dependsOnID string) error {
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error { return m.DepRemove(issueID, dependsOnID) })
	return err
}

// UpdateIfMatch publishes one conditional snapshot mutation.
func (s *AntflyStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error { return m.UpdateIfMatch(id, expectedRevision, opts) })
	return err
}

// CloseIfMatch publishes one conditional snapshot mutation.
func (s *AntflyStore) CloseIfMatch(id string, expectedRevision int64) error {
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error { return m.CloseIfMatch(id, expectedRevision) })
	return err
}

// DeleteIfMatch publishes one conditional snapshot mutation.
func (s *AntflyStore) DeleteIfMatch(id string, expectedRevision int64) error {
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error { return m.DeleteIfMatch(id, expectedRevision) })
	return err
}

// CloseAll commits its entire transition atomically.
func (s *AntflyStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	var result int
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error { var e error; result, e = m.CloseAll(ids, metadata); return e })
	if err != nil {
		return 0, err
	}
	return result, nil
}

// ReleaseIfCurrent commits its entire transition atomically.
func (s *AntflyStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	var result bool
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error {
		var e error
		result, e = m.ReleaseIfCurrent(id, expectedAssignee)
		return e
	})
	if err != nil {
		return false, err
	}
	return result, nil
}

// CompareAndSetMetadataKey commits its entire transition atomically.
func (s *AntflyStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	var result bool
	_, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error {
		var e error
		result, e = m.CompareAndSetMetadataKey(id, key, expected, next)
		return e
	})
	if err != nil {
		return false, err
	}
	return result, nil
}

// ReadyContext honors cancellation during the durable read and graph query.
func (s *AntflyStore) ReadyContext(ctx context.Context, q ...ReadyQuery) ([]Bead, error) {
	m, e := s.read(ctx)
	if e != nil {
		return nil, e
	}
	return m.ReadyContext(ctx, q...)
}

// Ping reads the current workspace head.
func (s *AntflyStore) Ping() error { _, e := s.read(context.Background()); return e }

// SetLocalString stores clone-local scratch data for this handle.
func (s *AntflyStore) SetLocalString(id, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, m, _, e := s.load(context.Background())
	if e != nil {
		return e
	}
	if _, e = m.Get(id); e != nil {
		return e
	}
	if s.local[id] == nil {
		s.local[id] = map[string]string{}
	}
	s.local[id][key] = value
	return nil
}

// GetLocalString reads this handle's scratch data.
func (s *AntflyStore) GetLocalString(id, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, m, _, e := s.load(context.Background())
	if e != nil {
		return "", e
	}
	if _, e = m.Get(id); e != nil {
		return "", e
	}
	return s.local[id][key], nil
}

type antflyTx struct {
	s *AntflyStore
	m *MemStore
}

func (t antflyTx) Create(b Bead) (Bead, error)          { return t.s.checkCreate(t.m, b, false) }
func (t antflyTx) Update(id string, o UpdateOpts) error { return t.m.Update(id, o) }
func (t antflyTx) SetMetadataBatch(id string, kv map[string]string) error {
	return t.m.SetMetadataBatch(id, kv)
}
func (t antflyTx) Close(id string) error { return t.m.Close(id) }

// Tx executes the callback once and publishes all its changes in one CAS.
func (s *AntflyStore) Tx(_ string, fn func(Tx) error) error {
	if fn == nil {
		return fmt.Errorf("antfly: nil transaction callback")
	}
	_, e := s.change(context.Background(), false, func(m *MemStore, _ *antflyState) error { return fn(antflyTx{s, m}) })
	return e
}
func (s *AntflyStore) conditionalLabelsGuarded() bool { return true }
func (s *AntflyStore) probeConditionalWriteCapability() (bool, string) {
	return true, "Antfly conditional workspace snapshots"
}

// CloseWithMetadataIfMatch merges metadata and closes in the same revision.
func (s *AntflyStore) CloseWithMetadataIfMatch(id string, rev int64, metadata map[string]string) (Bead, error) {
	m, e := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error {
		_, e := m.closeWithMetadataIfMatch(id, rev, metadata)
		return e
	})
	if e != nil {
		return Bead{}, e
	}
	return m.Get(id)
}

// ApplyGuarded checks all guards and atomically publishes the entire batch.
func (s *AntflyStore) ApplyGuarded(ctx context.Context, b GuardedBatch) (GuardedResult, error) {
	var result GuardedResult
	m, e := s.change(ctx, true, func(m *MemStore, _ *antflyState) error {
		for _, item := range b.Items {
			if item.Create != nil {
				if err := checkPinnedIDNamespace("antfly guarded create", item.Create.ID, s.namespaces); err != nil {
					return fmt.Errorf("%w: %w", ErrInvalidGuardedBatch, err)
				}
			}
		}
		var err error
		result, err = m.ApplyGuarded(ctx, b)
		return err
	})
	if e != nil {
		return GuardedResult{}, e
	}
	for id := range result.Revisions {
		row, _ := m.Get(id)
		result.Revisions[id] = row.Revision
	}
	return result, nil
}

// DepMetadata reports the durable payload on a dependency edge.
func (s *AntflyStore) DepMetadata(from, to string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, _, _, e := s.load(context.Background())
	if e != nil {
		return "", false, e
	}
	for k, d := range st.Dependencies {
		if d.IssueID == from && d.DependsOnID == to {
			if value, ok := st.EdgeMetadata[k]; ok && DepMetadataCarries(value) {
				return value, true, nil
			}
		}
	}
	return "", false, nil
}

// DepAddWithMetadata persists the edge and its opaque payload atomically.
func (s *AntflyStore) DepAddWithMetadata(from, to, typ, metadata string) error {
	if typ == "" {
		typ = "blocks"
	}
	_, e := s.change(context.Background(), true, func(m *MemStore, st *antflyState) error {
		if e := m.DepAdd(from, to, typ); e != nil {
			return e
		}
		st.EdgeMetadata[antflyEdgeKey(Dep{IssueID: from, DependsOnID: to, Type: typ})] = metadata
		return nil
	})
	return e
}

var (
	_ Store                   = (*AntflyStore)(nil)
	_ ConditionalWriter       = (*AntflyStore)(nil)
	_ GuardedBatcher          = (*AntflyStore)(nil)
	_ AtomicConditionalCloser = (*AntflyStore)(nil)
)

// Claim atomically acquires an unassigned open record. A same-owner reclaim is
// idempotent; a competing owner or terminal record returns ok=false.
func (s *AntflyStore) Claim(id, assignee string) (Bead, bool, error) {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return Bead{}, false, fmt.Errorf("antfly claim: empty assignee")
	}
	ok := false
	m, err := s.change(context.Background(), true, func(m *MemStore, _ *antflyState) error {
		ok = false
		b, e := m.Get(id)
		if e != nil {
			return e
		}
		if b.Status != "open" && b.Status != "in_progress" {
			return nil
		}
		owner := strings.TrimSpace(b.Assignee)
		if owner != "" && owner != assignee {
			return nil
		}
		ok = true
		if owner == assignee && b.Status == "in_progress" {
			return nil
		}
		status := "in_progress"
		return m.Update(id, UpdateOpts{Assignee: &assignee, Status: &status})
	})
	if err != nil || !ok {
		return Bead{}, false, err
	}
	b, err := m.Get(id)
	return b, err == nil, err
}

// BeadsNudgeQueueMode opts this born-split engine into the canonical bead queue.
func (*AntflyStore) BeadsNudgeQueueMode() bool { return true }

// OwnsClosedSessionRetention declares that this dedicated infrastructure ledger
// participates in the controller's bounded closed-session cleanup.
func (s *AntflyStore) OwnsClosedSessionRetention() bool { return true }

func antflyIndexKey(field, value, shard string) string {
	raw, _ := json.Marshal([]string{field, value, shard})
	return string(raw)
}

func (st *antflyState) buildIndexes() {
	st.IndexVersion = 1
	st.Indexes = map[string][]string{}
	for id, r := range st.Records {
		sum := sha256.Sum256([]byte(id))
		shard := fmt.Sprintf("%02x", sum[0])
		add := func(field, value string) {
			key := antflyIndexKey(field, value, shard)
			st.Indexes[key] = append(st.Indexes[key], id)
		}
		b := r.Bead
		add("type", b.Type)
		add("status", b.Status)
		add("assignee", b.Assignee)
		add("parent", b.ParentID)
		for _, label := range b.Labels {
			add("label", label)
		}
		for key, value := range b.Metadata {
			add("metadata:"+key, value)
		}
	}
	for _, ids := range st.Indexes {
		sort.Strings(ids)
	}
}

func (s *AntflyStore) validateIndexedFields(fields map[string]json.RawMessage) error {
	if len(fields) == 0 {
		return nil
	}
	var schema, prefix string
	if err := json.Unmarshal(fields["schema"], &schema); err != nil {
		return err
	}
	if err := json.Unmarshal(fields["prefix"], &prefix); err != nil {
		return err
	}
	if schema != antflySchema || prefix != s.prefix {
		return fmt.Errorf("antfly: workspace schema/prefix mismatch")
	}
	return nil
}

func decodeIndexedAntflyRecord(id string, raw json.RawMessage) (Bead, error) {
	var r antflyRecord
	if e := json.Unmarshal(raw, &r); e != nil {
		return Bead{}, e
	}
	if r.Bead.ID != id {
		return Bead{}, fmt.Errorf("antfly: corrupt indexed record key")
	}
	r.Bead.Revision = r.Revision
	r.Bead.ClaimFence = r.Fence
	return r.Bead, nil
}

func (s *AntflyStore) indexedList(q ListQuery) ([]Bead, bool, error) {
	tr, ok := s.transport.(antfly.IndexedSnapshotTransport)
	if !ok {
		return nil, false, nil
	}
	if !q.HasFilter() && !q.AllowScan {
		return nil, true, ErrQueryRequiresScan
	}
	field, value := "", ""
	switch {
	case len(q.IDs) > 0:
	case len(q.Metadata) > 0:
		keys := make([]string, 0, len(q.Metadata))
		for k := range q.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		field = "metadata:" + keys[0]
		value = q.Metadata[keys[0]]
	case q.ParentID != "":
		field = "parent"
		value = q.ParentID
	case q.Assignee != "":
		field = "assignee"
		value = q.Assignee
	case q.Label != "":
		field = "label"
		value = q.Label
	case q.Type != "":
		field = "type"
		value = q.Type
	case q.Status != "":
		field = "status"
		value = q.Status
	default:
		return nil, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, true, fmt.Errorf("antfly: store closed")
	}
	for attempt := 0; attempt < 8; attempt++ {
		ids := append([]string(nil), q.IDs...)
		var expected *uint64
		if len(ids) == 0 {
			keys := make([]string, 256)
			for n := range keys {
				keys[n] = antflyIndexKey(field, value, fmt.Sprintf("%02x", n))
			}
			index, e := tr.ReadSnapshotRecords(context.Background(), "indexes", keys, nil)
			if e != nil {
				return nil, true, e
			}
			if e = s.validateIndexedFields(index.Fields); e != nil {
				return nil, true, e
			}
			var format int
			if raw := index.Fields["index_version"]; len(raw) > 0 {
				if e = json.Unmarshal(raw, &format); e != nil {
					return nil, true, e
				}
			}
			if format != 1 {
				return nil, false, nil
			}
			for _, raw := range index.Records {
				var members []string
				if e = json.Unmarshal(raw, &members); e != nil {
					return nil, true, e
				}
				ids = append(ids, members...)
			}
			expected = &index.Version
		}
		rows, e := tr.ReadSnapshotRecords(context.Background(), "records", ids, expected)
		if errors.Is(e, antfly.ErrSnapshotConflict) {
			continue
		}
		if e != nil {
			return nil, true, e
		}
		if e = s.validateIndexedFields(rows.Fields); e != nil {
			return nil, true, e
		}
		beads := make([]Bead, 0, len(rows.Records))
		for id, raw := range rows.Records {
			b, e := decodeIndexedAntflyRecord(id, raw)
			if e != nil {
				return nil, true, e
			}
			beads = append(beads, b)
		}
		SortBeads(beads, SortCreatedAsc)
		return ApplyListQuery(beads, q), true, nil
	}
	return nil, true, fmt.Errorf("antfly: indexed read contention: %w", antfly.ErrSnapshotConflict)
}

// BackupSnapshot returns a checksummed archive of this infrastructure workspace.
func (s *AntflyStore) BackupSnapshot(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("antfly: store closed")
	}
	return antfly.BackupSnapshot(ctx, s.transport)
}

// RestoreSnapshot restores an infrastructure archive only into a fresh workspace.
// Existing ledgers are refused so old revision/ownership fences cannot reappear.
func (s *AntflyStore) RestoreSnapshot(ctx context.Context, archive []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("antfly: store closed")
	}
	data, e := antfly.DecodeSnapshotBackup(archive)
	if e != nil {
		return e
	}
	var st antflyState
	if e = json.Unmarshal(data, &st); e != nil {
		return e
	}
	if st.Schema != antflySchema || st.Prefix != s.prefix {
		return fmt.Errorf("antfly: archive schema/prefix mismatch")
	}
	_, _, v, e := s.load(ctx)
	if e != nil {
		return e
	}
	if v != 0 {
		return fmt.Errorf("antfly: restore requires a fresh workspace")
	}
	return antfly.RestoreSnapshotBackup(ctx, s.transport, archive, v)
}

// RunSnapshot executes selection and mutation against one isolated domain
// snapshot, then publishes once. Callback code is never replayed on conflict.
func (s *AntflyStore) RunSnapshot(ctx context.Context, fn func(Store) error) error {
	if fn == nil {
		return fmt.Errorf("antfly: nil snapshot callback")
	}
	_, err := s.change(ctx, false, func(m *MemStore, _ *antflyState) error { return fn(m) })
	return err
}

// ViewSnapshot gives a caller an isolated domain view whose edits are discarded.
func (s *AntflyStore) ViewSnapshot(ctx context.Context, fn func(Store) error) error {
	if fn == nil {
		return fmt.Errorf("antfly: nil snapshot view callback")
	}
	m, err := s.read(ctx)
	if err != nil {
		return err
	}
	return fn(m)
}

// CollectGarbage reclaims immutable history in this dedicated infrastructure
// workspace. Validate schema metadata before adopting any interrupted fence.
func (s *AntflyStore) CollectGarbage(ctx context.Context) (antfly.SnapshotGCResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return antfly.SnapshotGCResult{}, fmt.Errorf("antfly: store closed")
	}
	collector, ok := s.transport.(antfly.SnapshotMaintenance)
	if !ok {
		return antfly.SnapshotGCResult{}, fmt.Errorf("antfly: transport does not support immutable history collection")
	}
	return collector.CollectGarbageChecked(ctx, s.validateIndexedFields)
}

// OpJournalRecordsByDefault opts this native fenced store into the builtin
// operation journal policy. Opening the journal still probes live capabilities.
func (s *AntflyStore) OpJournalRecordsByDefault() bool { return true }
