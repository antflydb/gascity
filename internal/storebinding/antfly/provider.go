// Package antfly serves Gas City infrastructure through AntflyLite or a remote
// Antfly server. A dedicated workspace contains Gas City's coordination schema;
// core Beads work retains its independent workspace and lifecycle semantics.
package antfly

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storebinding/beadsworkspace"
	backend "github.com/steveyegge/beads/backend/antfly"
)

// ProviderID names the compiled native Antfly coordination provider.
const ProviderID storebinding.ProviderID = "antfly-beads"

// ProviderFactory constructs resource-free binding facades.
type ProviderFactory struct{}

// ID returns the registered provider identifier.
func (ProviderFactory) ID() storebinding.ProviderID { return ProviderID }

// New validates the binding without opening or creating any engine.
func (ProviderFactory) New(spec storebinding.BindingSpec) (storebinding.Provider, error) {
	root, e := bindingRoot(spec)
	if e != nil {
		return nil, e
	}
	return &provider{spec: spec, root: root}, nil
}

type provider struct {
	spec storebinding.BindingSpec
	root string
}

func bindingRoot(spec storebinding.BindingSpec) (string, error) {
	if e := spec.Validate(); e != nil {
		return "", e
	}
	if spec.Provider != ProviderID || spec.Path != "" || spec.URL != "" || spec.Auth != "" {
		return "", fmt.Errorf("antfly binding requires config_ref; endpoint and credential references belong in antfly.json")
	}
	return beadsworkspace.WorkspaceRoot(spec.CityRoot, string(spec.ConfigRef))
}

func (p *provider) bound(spec storebinding.BindingSpec) error {
	root, e := bindingRoot(spec)
	if e != nil {
		return e
	}
	if root != p.root || spec.Name != p.spec.Name || spec.ConfigRef != p.spec.ConfigRef {
		return fmt.Errorf("antfly: specification does not match bound binding")
	}
	return nil
}

func (p *provider) target() (backend.Target, error) {
	var t backend.Target
	b, e := os.ReadFile(filepath.Join(p.root, "antfly.json"))
	if e != nil {
		return t, e
	}
	if e = json.Unmarshal(b, &t); e != nil {
		return t, e
	}
	if t.Path != "" && !filepath.IsAbs(t.Path) {
		t.Path = filepath.Join(p.root, t.Path)
	}
	if t.Library != "" && !filepath.IsAbs(t.Library) {
		t.Library = filepath.Join(p.root, t.Library)
	}
	if e = t.Validate(); e != nil {
		return t, e
	}
	return t, nil
}

// BindingLocation records the actual path/endpoint, table and workspace rather
// than just the configuration reference, so re-pointing a binding is visible.
func (p *provider) BindingLocation(spec storebinding.BindingSpec) (string, error) {
	if e := p.bound(spec); e != nil {
		return "", e
	}
	t, e := p.target()
	if e != nil {
		return "", e
	}
	root := t.Path
	if root == "" {
		root = t.URL
	}
	sum := sha256.Sum256([]byte(root + "\x00" + t.Table + "\x00" + t.Workspace + "\x00" + t.Database + "\x00" + t.Namespace))
	return filepath.Join(p.root, fmt.Sprintf("target-%x", sum)), nil
}

type closer struct{ s *beads.AntflyStore }

func (c closer) Close() error { return c.s.CloseStore() }

// OpenEngine opens the explicitly configured target for the assigned classes.
func (p *provider) OpenEngine(spec storebinding.BindingSpec, classes storebinding.ClassSet) (beads.Store, io.Closer, error) {
	if e := p.bound(spec); e != nil {
		return nil, nil, e
	}
	if classes.Empty() {
		return nil, nil, fmt.Errorf("antfly: no classes assigned")
	}
	if classes.Has(coordclass.ClassWork) {
		return nil, nil, fmt.Errorf("antfly: core work must use the Beads Antfly backend; this binding serves infrastructure")
	}
	t, e := p.target()
	if e != nil {
		return nil, nil, e
	}
	s, e := beads.OpenAntflyStore(t, "gcg", storebinding.EngineReservedPrefixes(classes)...)
	if e != nil {
		return nil, nil, e
	}
	return s, closer{s}, nil
}

func (p *provider) unavailable() error {
	return fmt.Errorf("antfly binding %q has no exclusive migration-fence lifecycle; provision a born-split city", p.spec.Name)
}

// Inspect identifies the configured component without opening it. It does not
// claim a fenced physical census or a migration generation.
func (p *provider) Inspect(_ context.Context, spec storebinding.BindingSpec) (storebinding.Inspection, error) {
	if e := p.bound(spec); e != nil {
		return storebinding.Inspection{}, e
	}
	classes, e := storebinding.NewClassSet(coordclass.ClassGraph, coordclass.ClassSessions, coordclass.ClassMessaging, coordclass.ClassOrders, coordclass.ClassNudges)
	if e != nil {
		return storebinding.Inspection{}, e
	}
	sum := sha256.Sum256([]byte(p.root))
	target, e := storebinding.NewFenceTarget(ProviderID, classes, []storebinding.FenceComponentTarget{{ID: "workspace", Locator: storebinding.ComponentLocator((&url.URL{Scheme: "file", Path: p.root}).String()), PhysicalIdentity: storebinding.PhysicalIdentity(fmt.Sprintf("sha256:%x", sum)), Classes: classes}})
	if e != nil {
		return storebinding.Inspection{}, e
	}
	return storebinding.NewInspection(target, nil)
}

// AcquireFence refuses guarantees this adapter cannot hold across remote writers.
func (p *provider) AcquireFence(context.Context, storebinding.MigrationGuardClaim, storebinding.FenceRequest) (storebinding.WriterFence, error) {
	return nil, p.unavailable()
}

// InspectFenced refuses because no exclusive writer fence is available.
func (p *provider) InspectFenced(context.Context, storebinding.FencedInspectionRequest) (storebinding.Descriptor, error) {
	return storebinding.Descriptor{}, p.unavailable()
}

// Open refuses descriptor-based migration opens; serving uses OpenEngine.
func (p *provider) Open(context.Context, storebinding.OpenRequest) (storebinding.OpenedBinding, error) {
	return nil, p.unavailable()
}

// RetainedGuards reports that migration guard recovery is unavailable.
func (p *provider) RetainedGuards() (storebinding.RetainedGuardLifecycle, bool) { return nil, false }

// BindingMigration reports that automatic physical migration is unavailable.
func (p *provider) BindingMigration() (storebinding.BindingMigrationLifecycle, bool) {
	return nil, false
}

// WorkMigration reports that work stays in its core Beads workspace.
func (p *provider) WorkMigration() (storebinding.WorkMigrationLifecycle, bool) { return nil, false }

var (
	_ storebinding.ProviderFactory = ProviderFactory{}
	_ storebinding.EngineOpener    = (*provider)(nil)
	_ storebinding.BindingLocator  = (*provider)(nil)
)

// SupportsFencedReconciler admits native infrastructure to the live capability
// gate; it does not bypass revision fidelity or boot-census validation.
func (ProviderFactory) SupportsFencedReconciler() bool { return true }
