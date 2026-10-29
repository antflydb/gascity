package antfly

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/storebinding"
	backend "github.com/steveyegge/beads/backend/antfly"
)

func TestFactoryIsResourceFreeAndRejectsAmbiguousBinding(t *testing.T) {
	root := t.TempDir()
	spec := storebinding.BindingSpec{Name: "infra", Provider: ProviderID, ConfigRef: "infra", CityRoot: root}
	p, e := (ProviderFactory{}).New(spec)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(root, ".gc")); !os.IsNotExist(e) {
		t.Fatalf("factory created storage: %v", e)
	}
	if _, e = p.AcquireFence(context.Background(), storebinding.MigrationGuardClaim{}, storebinding.FenceRequest{}); e == nil {
		t.Fatal("claimed unsupported migration fence")
	}
	spec.Path = "other"
	if _, e = (ProviderFactory{}).New(spec); e == nil {
		t.Fatal("accepted ambiguous binding")
	}
}

func TestNativeBindingPersistsInfrastructure(t *testing.T) {
	lib := os.Getenv("ANTFLY_TEST_LIBRARY")
	if lib == "" {
		t.Skip("ANTFLY_TEST_LIBRARY required")
	}
	root := t.TempDir()
	dir := filepath.Join(root, ".gc", "storage", "infra")
	if e := os.MkdirAll(dir, 0o755); e != nil {
		t.Fatal(e)
	}
	target := backend.Target{Path: "infra.aflite", Library: lib, Table: "gascity_infra", Workspace: "infra"}
	raw, _ := json.Marshal(target)
	if e := os.WriteFile(filepath.Join(dir, "antfly.json"), raw, 0o600); e != nil {
		t.Fatal(e)
	}
	spec := storebinding.BindingSpec{Name: "infra", Provider: ProviderID, ConfigRef: "infra", CityRoot: root}
	facade, e := (ProviderFactory{}).New(spec)
	if e != nil {
		t.Fatal(e)
	}
	p := facade.(*provider)
	classes, e := storebinding.NewClassSet(coordclass.ClassGraph, coordclass.ClassSessions, coordclass.ClassMessaging, coordclass.ClassOrders, coordclass.ClassNudges)
	if e != nil {
		t.Fatal(e)
	}
	location, e := p.BindingLocation(spec)
	if e != nil {
		t.Fatal(e)
	}
	s, handle, e := p.OpenEngine(spec, classes)
	if e != nil {
		t.Fatal(e)
	}
	for _, typ := range []string{"session", "molecule", "message", "order", "nudge"} {
		if _, e = s.Create(beads.Bead{Type: typ, Title: typ}); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.Create(beads.Bead{ID: "bd-foreign", Title: "refuse"}); e == nil {
		t.Fatal("accepted work namespace")
	}
	if e = handle.Close(); e != nil {
		t.Fatal(e)
	}
	s, handle, e = p.OpenEngine(spec, classes)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = handle.Close() }()
	rows, e := s.List(beads.ListQuery{AllowScan: true})
	if e != nil || len(rows) != 5 {
		t.Fatalf("durability: %d %v", len(rows), e)
	}
	target.Workspace = "repoint"
	raw, _ = json.Marshal(target)
	if e = os.WriteFile(filepath.Join(dir, "antfly.json"), raw, 0o600); e != nil {
		t.Fatal(e)
	}
	next, e := p.BindingLocation(spec)
	if e != nil || next == location {
		t.Fatal("binding re-point invisible")
	}
}

func TestNativeBindingNudgeQueueDurability(t *testing.T) {
	lib := os.Getenv("ANTFLY_TEST_LIBRARY")
	if lib == "" {
		t.Skip("ANTFLY_TEST_LIBRARY required")
	}
	root := t.TempDir()
	dir := filepath.Join(root, ".gc", "storage", "infra")
	if e := os.MkdirAll(dir, 0o755); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(backend.Target{Path: "queue.aflite", Library: lib, Table: "infra", Workspace: "infra"})
	if e := os.WriteFile(filepath.Join(dir, "antfly.json"), raw, 0o600); e != nil {
		t.Fatal(e)
	}
	spec := storebinding.BindingSpec{Name: "infra", Provider: ProviderID, ConfigRef: "infra", CityRoot: root}
	facade, e := (ProviderFactory{}).New(spec)
	if e != nil {
		t.Fatal(e)
	}
	p := facade.(*provider)
	classes, e := storebinding.NewClassSet(coordclass.ClassNudges)
	if e != nil {
		t.Fatal(e)
	}
	s, handle, e := p.OpenEngine(spec, classes)
	if e != nil {
		t.Fatal(e)
	}
	front, e := storebinding.NewBeadsNudgeFrontDoors(beads.NudgesStore{Store: s})
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	item := nudgequeue.Item{ID: "native-durable", Agent: "worker", Source: "test", Message: "retain across reopen", CreatedAt: now, DeliverAfter: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour)}
	if e = front.Queue.EnqueueDeferred(item); e != nil {
		t.Fatal(e)
	}
	if e = handle.Close(); e != nil {
		t.Fatal(e)
	}
	s, handle, e = p.OpenEngine(spec, classes)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = handle.Close() }()
	front, e = storebinding.NewBeadsNudgeFrontDoors(beads.NudgesStore{Store: s})
	if e != nil {
		t.Fatal(e)
	}
	pending, _, _, e := front.Queue.ListForAgent("worker", now)
	if e != nil || len(pending) != 1 || pending[0].Message != item.Message {
		t.Fatalf("lost queued nudge: %+v %v", pending, e)
	}
	claimed, e := front.Queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{"worker"}}, now.Add(2*time.Minute))
	if e != nil || len(claimed) != 1 {
		t.Fatalf("native claim: %+v %v", claimed, e)
	}
	if e = front.Queue.Ack([]string{item.ID}, "", "delivered", "native persistence"); e != nil {
		t.Fatal(e)
	}
	terminal, found, e := front.Shadows.FindIncludingTerminal(item.ID)
	if e != nil || !found || terminal.Open {
		t.Fatalf("terminal view: %+v %v %v", terminal, found, e)
	}
}
