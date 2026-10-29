package beads

import (
	"context"
	"fmt"
	"maps"
)

// SupportsEphemeralGraphApply reports that ephemeral nodes can be published atomically.
func (s *AntflyStore) SupportsEphemeralGraphApply() bool { return true }

// ApplyGraphPlan publishes a complete symbolic formula graph in one snapshot.
func (s *AntflyStore) ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	return s.ApplyGraphPlanWithStorage(ctx, plan, StorageDefault)
}

// ApplyGraphPlanWithStorage atomically creates a graph in the selected tier.
func (s *AntflyStore) ApplyGraphPlanWithStorage(ctx context.Context, plan *GraphApplyPlan, storage StorageClass) (*GraphApplyResult, error) {
	if e := validateGraphApplyPlan(plan); e != nil {
		return nil, e
	}
	ephemeral, noHistory, e := graphStorageFlags(storage)
	if e != nil {
		return nil, e
	}
	var result *GraphApplyResult
	_, e = s.change(ctx, true, func(m *MemStore, st *antflyState) error {
		ids := make(map[string]string, len(plan.Nodes))
		for _, n := range plan.Nodes {
			if e := ctx.Err(); e != nil {
				return e
			}
			priority := n.Priority
			if priority == nil {
				v := 2
				priority = &v
			}
			b, e := m.Create(Bead{Title: n.Title, Description: n.Description, Type: n.Type, Priority: priority, Assignee: n.Assignee, From: n.From, Labels: append([]string(nil), n.Labels...), Metadata: maps.Clone(n.Metadata), Ephemeral: ephemeral, NoHistory: noHistory})
			if e != nil {
				return e
			}
			ids[n.Key] = b.ID
		}
		parentPairs := map[string]bool{}
		for _, n := range plan.Nodes {
			id := ids[n.Key]
			parent := n.ParentID
			if n.ParentKey != "" {
				parent = ids[n.ParentKey]
			}
			if parent != "" {
				if e := m.Update(id, UpdateOpts{ParentID: &parent}); e != nil {
					return e
				}
				parentPairs[id+"\x00"+parent] = true
			}
			kv := make(map[string]string, len(n.MetadataRefs))
			for key, ref := range n.MetadataRefs {
				kv[key] = ids[ref]
			}
			if len(kv) > 0 {
				if e := m.SetMetadataBatch(id, kv); e != nil {
					return e
				}
			}
		}
		for i, edge := range plan.Edges {
			from := graphApplyResolveRef(edge.FromKey, edge.FromID, ids)
			to := graphApplyResolveRef(edge.ToKey, edge.ToID, ids)
			typ := edge.Type
			if typ == "" {
				typ = "blocks"
			}
			if parentPairs[from+"\x00"+to] {
				if typ == "parent-child" {
					continue
				}
				return fmt.Errorf("antfly graph: edge %d conflicts with parent relationship", i)
			}
			if parentPairs[to+"\x00"+from] && (typ == "blocks" || typ == "conditional-blocks") {
				return fmt.Errorf("antfly graph: blocking reverse parent relationship")
			}
			if e := m.DepAdd(from, to, typ); e != nil {
				return e
			}
			st.EdgeMetadata[antflyEdgeKey(Dep{IssueID: from, DependsOnID: to, Type: typ})] = edge.Metadata
		}
		for _, n := range plan.Nodes {
			parent := n.ParentID
			if n.ParentKey != "" {
				parent = ids[n.ParentKey]
			}
			if parent != "" {
				if e := m.DepAdd(ids[n.Key], parent, "parent-child"); e != nil {
					return e
				}
			}
		}
		// Refuse blocking cycles before publishing any nodes or edges. ParentID is
		// a weak reference and may point outside this workspace.
		adj := map[string][]string{}
		for _, d := range m.deps {
			if d.Type == "blocks" || d.Type == "conditional-blocks" {
				adj[d.IssueID] = append(adj[d.IssueID], d.DependsOnID)
			}
		}
		colors := map[string]int{}
		var visit func(string) bool
		visit = func(id string) bool {
			if colors[id] == 1 {
				return true
			}
			if colors[id] == 2 {
				return false
			}
			colors[id] = 1
			for _, to := range adj[id] {
				if visit(to) {
					return true
				}
			}
			colors[id] = 2
			return false
		}
		for id := range adj {
			if visit(id) {
				return fmt.Errorf("antfly graph: blocking dependency cycle")
			}
		}
		result = &GraphApplyResult{IDs: ids}
		return nil
	})
	if e != nil {
		return nil, e
	}
	return result, nil
}

var _ StorageGraphApplyStore = (*AntflyStore)(nil)
