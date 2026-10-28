// Package storeclass is the typed-store lint (typed-stores design §3.8, phase
// T1). A coordination class's store reaches code as a typed class handle
// (beads.WorkStore, GraphStore, SessionStore, MailStore, OrdersStore,
// NudgesStore), and only the residency layer decides which physical store a
// handle wraps. Outside the allowlisted packages it reports, by type rather
// than by name:
//
//   - raw-param, raw-result, raw-field, raw-var, raw-type: a parameter,
//     result, struct field, package variable or declared type whose type is
//     or holds a raw beads.Store (through a pointer, slice, array, map,
//     channel, function signature or type argument; an interface embedding
//     it);
//   - mint: a typed handle built outside the mint, by a composite literal
//     with fields or a conversion, and any reference to a mint function;
//   - unwrap: a handle's embedded store read back (.Store, through any
//     embedding) or an unwrap method (Raw) named;
//   - cap-assert: a type assertion or type switch on a store value (a
//     beads.Store, an interface that is one, or a store or handle boxed by a
//     conversion): use beads.As or an XFor helper, which see through the
//     handle;
//   - test-mint-import: production code importing a test-only minting
//     package;
//   - read-marker: a federated or relic read marker named (D9 freezes them:
//     a new federation is a storeref intent change).
//
// Every finding counts against a generated baseline keyed by file, enclosing
// function and rule (baseline.txt). A count above its pin reports every site
// of that key; a pin the tree no longer reaches reports too, so the baseline
// shrinks in the commit that retires a site. A package compiled under several
// build-tag sets is checked in each, against the rows for the files that set
// compiles. A Sealed package has no
// baseline at all. Test files and generated files are not linted, and a
// //nolint:storeclass directive is itself a finding: the baseline is the only
// exemption.
package storeclass

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"golang.org/x/tools/go/analysis"
)

// Rule names one kind of finding; it is a baseline row's third column.
type Rule string

// The rules.
const (
	RawParam       Rule = "raw-param"
	RawResult      Rule = "raw-result"
	RawField       Rule = "raw-field"
	RawVar         Rule = "raw-var"
	RawType        Rule = "raw-type"
	Mint           Rule = "mint"
	Unwrap         Rule = "unwrap"
	CapAssert      Rule = "cap-assert"
	TestMintImport Rule = "test-mint-import"
	ReadMarker     Rule = "read-marker"
)

// Rules is every rule, in reporting order.
var Rules = []Rule{RawParam, RawResult, RawField, RawVar, RawType, Mint, Unwrap, CapAssert, TestMintImport, ReadMarker}

// AllRulesBut is every rule except rs, for an Allow entry.
func AllRulesBut(rs ...Rule) []Rule {
	var out []Rule
	for _, r := range Rules {
		if !slices.Contains(rs, r) {
			out = append(out, r)
		}
	}
	return out
}

// Allow exempts a package (a module-relative path; a trailing "/..." takes
// its subpackages too), or one file of it (File, a base name or a path.Match
// glob), from Rules (nil: every rule), for a reviewed Reason. A File entry
// that matches no file of its package is stale and reports.
type Allow struct {
	Package string
	File    string
	Rules   []Rule
	Reason  string
}

// Config is what the lint guards, by fully qualified name ("importpath.Name").
type Config struct {
	Module           string   // module path prefix with a trailing "/": only its packages are linted
	Store            string   // the raw store interface
	Handles          []string // the typed class handle types
	HandleField      string   // the handles' embedded store field
	UnwrapMethods    []string // handle methods that return the raw store
	MintFuncs        []string // functions that mint handles
	TestMintPackages []string // packages production code may not import
	Markers          []string // federated/relic read marker functions
	Allow            []Allow
	Sealed           []string    // module-relative packages with no baseline
	Baseline         map[Key]int // the pinned counts
}

// Finding is one site: its baseline key, where it is, and what it is.
type Finding struct {
	Key  Key
	Pos  token.Pos
	Desc string
}

// New builds the analyzer for cfg.
func New(cfg Config) *analysis.Analyzer {
	return &analysis.Analyzer{
		Name: "storeclass",
		Doc:  "reports raw beads.Store signatures, handle mints, unwraps and capability assertions outside the residency layer, against a shrink-only baseline (typed-stores T1)",
		Run:  func(pass *analysis.Pass) (any, error) { return nil, run(pass, cfg, nil) },
	}
}

// NewCollector builds an analyzer that hands every finding to collect,
// unfiltered by the baseline, and reports nothing: the baseline generator.
// collect may be called from several goroutines' passes; it is serialized.
func NewCollector(cfg Config, collect func(Finding)) *analysis.Analyzer {
	var mu sync.Mutex
	serial := func(f Finding) {
		mu.Lock()
		defer mu.Unlock()
		collect(f)
	}
	return &analysis.Analyzer{
		Name: "storeclass",
		Doc:  "collects storeclass findings for the baseline generator",
		Run:  func(pass *analysis.Pass) (any, error) { return nil, run(pass, cfg, serial) },
	}
}

// relPackage is pkgPath relative to the module, "." for its root, and
// whether it is in the module at all.
func relPackage(cfg Config, pkgPath string) (string, bool) {
	root := strings.TrimSuffix(cfg.Module, "/")
	switch {
	case pkgPath == root:
		return ".", true
	case strings.HasPrefix(pkgPath, cfg.Module):
		return strings.TrimPrefix(pkgPath, cfg.Module), true
	}
	return "", false
}

func allowMatches(a Allow, rel string) bool {
	if pkg, ok := strings.CutSuffix(a.Package, "/..."); ok {
		return rel == pkg || strings.HasPrefix(rel, pkg+"/")
	}
	return rel == a.Package
}

func fileMatches(a Allow, base string) bool {
	if a.File == "" {
		return true
	}
	ok, err := path.Match(a.File, base)
	return err == nil && ok
}

func (c Config) allowed(rel, base string, r Rule) bool {
	for _, a := range c.Allow {
		if allowMatches(a, rel) && fileMatches(a, base) && (a.Rules == nil || slices.Contains(a.Rules, r)) {
			return true
		}
	}
	return false
}

// linted reports whether a file of the pass is linted: not a test file, and
// not generated (nogo's config excludes _gen.go files; the generator must
// agree with it).
func linted(filename string) bool {
	base := path.Base(strings.ReplaceAll(filename, "\\", "/"))
	return !strings.HasSuffix(base, "_test.go") && !strings.HasSuffix(base, "_gen.go")
}

func run(pass *analysis.Pass, cfg Config, collect func(Finding)) error {
	rel, ok := relPackage(cfg, pass.Pkg.Path())
	if !ok {
		return nil
	}
	c := &checker{cfg: cfg, pass: pass, rel: rel}
	c.storeIface = c.findStoreInterface()
	var first *ast.File
	var bases []string
	for _, file := range pass.Files {
		name := pass.Fset.Position(file.Pos()).Filename
		if !linted(name) {
			continue
		}
		if first == nil {
			first = file
		}
		base := path.Base(strings.ReplaceAll(name, "\\", "/"))
		bases = append(bases, base)
		c.file(file, base)
	}
	if first == nil {
		return nil // a test-only pass
	}
	if collect != nil {
		for _, f := range c.found {
			collect(f)
		}
		return nil
	}
	c.report(first, bases)
	return nil
}

type checker struct {
	cfg        Config
	pass       *analysis.Pass
	rel        string
	storeIface *types.Interface
	found      []Finding
	nolint     []token.Pos
}

// report checks the pass's findings against the baseline.
func (c *checker) report(first *ast.File, bases []string) {
	sealed := slices.Contains(c.cfg.Sealed, c.rel)
	byKey := map[Key][]Finding{}
	var keys []Key
	for _, f := range c.found {
		if byKey[f.Key] == nil {
			keys = append(keys, f.Key)
		}
		byKey[f.Key] = append(byKey[f.Key], f)
	}
	sortKeys(keys)
	for _, k := range keys {
		pinned := c.cfg.Baseline[k]
		if sealed {
			pinned = 0
		}
		fs := byKey[k]
		if len(fs) <= pinned {
			continue
		}
		for _, f := range fs {
			c.pass.Reportf(f.Pos, "storeclass[%s] %s (%s %s: %d here, baseline %d)", k.Rule, f.Desc, k.Path, k.Func, len(fs), pinned)
		}
	}
	var stale []string
	for k, pinned := range c.cfg.Baseline {
		// A row for a file this pass does not compile is another build-tag
		// variant's (nogo analyzes a package once per configuration that
		// compiles it), or a deleted file's, which the generator's check
		// reports: it is not this pass's to judge.
		if k.Package() != c.rel || !slices.Contains(bases, path.Base(k.Path)) {
			continue
		}
		switch have := len(byKey[k]); {
		case sealed:
			stale = append(stale, fmt.Sprintf("storeclass[baseline] %s is sealed: drop its baseline row %s", c.rel, k))
		case have < pinned:
			stale = append(stale, fmt.Sprintf("storeclass[baseline] %s pins %d, the tree has %d: regenerate the baseline (it may only shrink)", k, pinned, have))
		}
	}
	for _, a := range c.cfg.Allow {
		if a.File == "" || a.Package != c.rel {
			continue
		}
		if !slices.ContainsFunc(bases, func(b string) bool { return fileMatches(a, b) }) {
			stale = append(stale, fmt.Sprintf("storeclass[allowlist] allowlisted file %s/%s names nothing: drop it", c.rel, a.File))
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		c.pass.Report(analysis.Diagnostic{Pos: first.Package, Message: s})
	}
	for _, pos := range c.nolint {
		c.pass.Reportf(c.fileOf(pos).Package, "storeclass[nolint] a //nolint:storeclass directive at %s: the baseline is the only exemption", c.pass.Fset.Position(pos))
	}
}

func (c *checker) fileOf(pos token.Pos) *ast.File {
	for _, f := range c.pass.Files {
		if f.FileStart <= pos && pos <= f.FileEnd {
			return f
		}
	}
	return c.pass.Files[0]
}

func (c *checker) add(base, fn string, r Rule, pos token.Pos, desc string) {
	if c.cfg.allowed(c.rel, base, r) {
		return
	}
	p := base
	if c.rel != "." {
		p = c.rel + "/" + base
	}
	c.found = append(c.found, Finding{Key: Key{Path: p, Func: fn, Rule: r}, Pos: pos, Desc: desc})
}

// file collects one file's findings.
func (c *checker) file(file *ast.File, base string) {
	for _, cg := range file.Comments {
		for _, cm := range cg.List {
			if names, ok := strings.CutPrefix(cm.Text, "//nolint:"); ok {
				names, _, _ = strings.Cut(names, " ")
				if slices.Contains(strings.Split(names, ","), "storeclass") {
					c.nolint = append(c.nolint, cm.Pos())
				}
			}
		}
	}
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, "`\"")
		if slices.Contains(c.cfg.TestMintPackages, p) {
			c.add(base, "(import)", TestMintImport, imp.Pos(), "production code imports the test-only package "+p)
		}
	}
	info := c.pass.TypesInfo
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		fn := enclosing(stack)
		switch v := n.(type) {
		case *ast.FuncDecl:
			c.signature(base, fn, v.Type)
		case *ast.FuncLit:
			c.signature(base, fn, v.Type)
		case *ast.StructType:
			for _, f := range v.Fields.List {
				c.fieldList(base, fn, RawField, "field", f)
			}
		case *ast.InterfaceType:
			for _, m := range v.Methods.List {
				if ft, ok := m.Type.(*ast.FuncType); ok {
					c.signature(base, fn, ft)
					continue
				}
				if c.isStore(info.TypeOf(m.Type)) {
					c.add(base, fn, RawType, m.Pos(), "an interface embeds the raw beads.Store: take a typed class handle")
				}
			}
		case *ast.TypeSpec:
			switch v.Type.(type) {
			case *ast.StructType, *ast.InterfaceType:
			default:
				if t := info.TypeOf(v.Type); c.holds(t) {
					c.add(base, fn, RawType, v.Name.Pos(), fmt.Sprintf("type %s is or holds a raw beads.Store (%s): take a typed class handle", v.Name.Name, c.typeString(t)))
				}
			}
		case *ast.ValueSpec:
			if len(stack) >= 3 {
				if _, top := stack[len(stack)-3].(*ast.File); top {
					for _, name := range v.Names {
						if obj := info.Defs[name]; obj != nil && c.holds(obj.Type()) {
							c.add(base, name.Name, RawVar, name.Pos(), fmt.Sprintf("package variable %s is or holds a raw beads.Store (%s): hold a typed class handle", name.Name, c.typeString(obj.Type())))
						}
					}
				}
			}
		case *ast.CompositeLit:
			if len(v.Elts) > 0 && c.isHandle(info.TypeOf(v)) {
				c.add(base, fn, Mint, v.Pos(), fmt.Sprintf("a %s minted by a literal: handles come from the residency layer (storeref), or beadstest in tests", c.typeString(info.TypeOf(v))))
			}
		case *ast.CallExpr:
			if tv, ok := info.Types[v.Fun]; ok && tv.IsType() && c.isHandle(tv.Type) {
				c.add(base, fn, Mint, v.Pos(), fmt.Sprintf("a conversion to %s: a handle is never relabeled to another class", c.typeString(tv.Type)))
			}
		case *ast.Ident:
			if f, ok := info.Uses[v].(*types.Func); ok && f.Pkg() != nil && f.Type().(*types.Signature).Recv() == nil {
				full := f.Pkg().Path() + "." + f.Name()
				switch {
				case slices.Contains(c.cfg.MintFuncs, full):
					c.add(base, fn, Mint, v.Pos(), fmt.Sprintf("%s mints handles: only the residency layer may call it", f.Name()))
				case slices.Contains(c.cfg.Markers, full):
					c.add(base, fn, ReadMarker, v.Pos(), fmt.Sprintf("the read marker %s: markers are frozen (D9); a new federation is a storeref intent change", f.Name()))
				}
			}
		case *ast.SelectorExpr:
			c.selector(base, fn, v)
		case *ast.TypeAssertExpr:
			if c.isStoreValue(v.X) {
				what := "a type switch"
				if v.Type != nil {
					what = "a type assertion to " + c.typeString(info.TypeOf(v.Type))
				}
				c.add(base, fn, CapAssert, v.Pos(), what+" on a store: use beads.As or a beads XFor helper, which see through a typed handle")
			}
		}
		return true
	})
}

// signature collects a function type's raw parameters and results.
func (c *checker) signature(base, fn string, ft *ast.FuncType) {
	if ft.Params != nil {
		for _, f := range ft.Params.List {
			c.fieldList(base, fn, RawParam, "parameter", f)
		}
	}
	if ft.Results != nil {
		for _, f := range ft.Results.List {
			c.fieldList(base, fn, RawResult, "result", f)
		}
	}
}

// fieldList collects one field of a parameter, result or field list: a
// finding per name, or one for an unnamed or embedded field.
func (c *checker) fieldList(base, fn string, r Rule, what string, f *ast.Field) {
	t := c.pass.TypesInfo.TypeOf(f.Type)
	if !c.holds(t) {
		return
	}
	desc := func(name string) string {
		return fmt.Sprintf("%s %s is or holds a raw beads.Store (%s): take a typed class handle", what, name, c.typeString(t))
	}
	if len(f.Names) == 0 {
		c.add(base, fn, r, f.Pos(), desc("(unnamed)"))
		return
	}
	for _, name := range f.Names {
		c.add(base, fn, r, name.Pos(), desc(name.Name))
	}
}

func (c *checker) selector(base, fn string, sel *ast.SelectorExpr) {
	s := c.pass.TypesInfo.Selections[sel]
	if s == nil {
		return
	}
	switch s.Kind() {
	case types.FieldVal:
		if s.Obj().Name() != c.cfg.HandleField {
			return
		}
		owner := s.Recv()
		idx := s.Index()
		for _, i := range idx[:len(idx)-1] {
			st, ok := deref(owner).Underlying().(*types.Struct)
			if !ok {
				return
			}
			owner = st.Field(i).Type()
		}
		if c.isHandle(owner) {
			c.add(base, fn, Unwrap, sel.Sel.Pos(), fmt.Sprintf("%s.%s unwraps a typed handle to its raw store: keep the handle, or use beads.As for a capability", c.typeString(deref(owner)), c.cfg.HandleField))
		}
	case types.MethodVal, types.MethodExpr:
		m, ok := s.Obj().(*types.Func)
		if !ok || !slices.Contains(c.cfg.UnwrapMethods, m.Name()) {
			return
		}
		if recv := m.Type().(*types.Signature).Recv(); recv != nil && c.isHandle(recv.Type()) {
			c.add(base, fn, Unwrap, sel.Sel.Pos(), fmt.Sprintf("%s unwraps a typed handle to its raw store: keep the handle, or use beads.As for a capability", m.Name()))
		}
	}
}

// enclosing is the baseline's function column for the innermost node of
// stack: the function declaration ("Func", "Recv.Method"), else the
// package-level type or variable, else "(package)".
func enclosing(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if fn, ok := stack[i].(*ast.FuncDecl); ok {
			return funcName(fn)
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		switch s := stack[i].(type) {
		case *ast.TypeSpec:
			return s.Name.Name
		case *ast.ValueSpec:
			if len(s.Names) > 0 {
				return s.Names[0].Name
			}
		}
	}
	return "(package)"
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
			continue
		case *ast.ParenExpr:
			t = x.X
			continue
		case *ast.IndexExpr:
			t = x.X
			continue
		case *ast.IndexListExpr:
			t = x.X
			continue
		case *ast.Ident:
			return x.Name + "." + fn.Name.Name
		}
		return fn.Name.Name
	}
}

func deref(t types.Type) types.Type {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		return types.Unalias(p.Elem())
	}
	return t
}

func fullName(t types.Type) string {
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return ""
	}
	return n.Obj().Pkg().Path() + "." + n.Obj().Name()
}

// isStore reports whether t is the raw store interface itself.
func (c *checker) isStore(t types.Type) bool { return t != nil && fullName(t) == c.cfg.Store }

// isHandle reports whether t, or what it points to, is a typed handle.
func (c *checker) isHandle(t types.Type) bool {
	return t != nil && slices.Contains(c.cfg.Handles, fullName(deref(t)))
}

// holds reports whether t is or holds the raw store through type
// constructors and type arguments. It does not enter other named types
// (their own declarations report) or anonymous structs and interfaces
// (their fields and methods report where they are written).
func (c *checker) holds(t types.Type) bool {
	switch t := types.Unalias(t).(type) {
	case nil:
		return false
	case *types.Named:
		if c.isStore(t) {
			return true
		}
		for a := range t.TypeArgs().Types() {
			if c.holds(a) {
				return true
			}
		}
	case *types.Pointer:
		return c.holds(t.Elem())
	case *types.Slice:
		return c.holds(t.Elem())
	case *types.Array:
		return c.holds(t.Elem())
	case *types.Chan:
		return c.holds(t.Elem())
	case *types.Map:
		return c.holds(t.Key()) || c.holds(t.Elem())
	case *types.Signature:
		for v := range t.Params().Variables() {
			if c.holds(v.Type()) {
				return true
			}
		}
		for v := range t.Results().Variables() {
			if c.holds(v.Type()) {
				return true
			}
		}
	}
	return false
}

// isStoreValue reports whether a type assertion's operand is a store: a raw
// store, an interface type that is one, or a store or handle boxed into an
// interface by a conversion (any(h)).
func (c *checker) isStoreValue(x ast.Expr) bool {
	info := c.pass.TypesInfo
	t := info.TypeOf(x)
	if c.isStore(t) {
		return true
	}
	if t != nil && c.storeIface != nil {
		if _, isIface := t.Underlying().(*types.Interface); isIface && types.Implements(t, c.storeIface) {
			return true
		}
	}
	call, ok := ast.Unparen(x).(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	if tv, ok := info.Types[call.Fun]; !ok || !tv.IsType() {
		return false
	}
	return c.isHandle(info.TypeOf(call.Args[0])) || c.isStoreValue(call.Args[0])
}

// findStoreInterface is the store interface as the pass's imports show it,
// or nil when the package cannot see it.
func (c *checker) findStoreInterface() *types.Interface {
	pkgPath, name, ok := cutLast(c.cfg.Store, ".")
	if !ok {
		return nil
	}
	seen := map[*types.Package]bool{}
	var walk func(p *types.Package) *types.Interface
	walk = func(p *types.Package) *types.Interface {
		if p == nil || seen[p] {
			return nil
		}
		seen[p] = true
		if p.Path() == pkgPath {
			if tn, ok := p.Scope().Lookup(name).(*types.TypeName); ok {
				if iface, ok := tn.Type().Underlying().(*types.Interface); ok {
					return iface
				}
			}
			return nil
		}
		for _, imp := range p.Imports() {
			if i := walk(imp); i != nil {
				return i
			}
		}
		return nil
	}
	return walk(c.pass.Pkg)
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func (c *checker) typeString(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string {
		if p == c.pass.Pkg {
			return ""
		}
		return p.Name()
	})
}
