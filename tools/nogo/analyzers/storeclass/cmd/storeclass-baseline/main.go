// Command storeclass-baseline regenerates the storeclass lint's baseline
// (tools/nogo/analyzers/storeclass/baseline.txt) and its ceiling
// (ceiling.txt) from the tree. It type-checks every production package of
// the gascity module with the analyzer's Default configuration, unfiltered
// by the baseline, and counts the findings per file, enclosing function and
// rule. It does so once untagged and once per custom build tag a production
// file names (integration, productmetrics_testhook, ...), because nogo
// analyzes a package under every configuration that compiles it, and keeps
// each key's count from the variant that compiles its file.
//
// The baseline may only shrink. Without a flag it prints the census and
// whether the files on disk match it. -write rewrites both files, and
// refuses, writing nothing, when any package's total for a rule would rise
// above ceiling.txt, when a package/rule pair would appear that ceiling.txt
// lacks, or when a Sealed package would get a row. -init writes the first
// ceiling and refuses once one exists.
//
// -growth-since <ref> (CI passes the pull request's merge-base) also fails
// on any new or raised baseline row or ceiling row against ref, unless
// -allow-raise: the storeclass/baseline-raise label (see the storeclass
// package doc for the raise procedure).
//
// Run it from tools/nogo:
//
//	go run ./analyzers/storeclass/cmd/storeclass-baseline -write
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/build/constraint"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/storeclass"
)

func main() {
	repo := flag.String("repo", "", "the gascity checkout (default: the git toplevel)")
	write := flag.Bool("write", false, "rewrite baseline.txt and ceiling.txt; refuse any growth")
	initial := flag.Bool("init", false, "write the first ceiling.txt; refuse when one has rows")
	since := flag.String("growth-since", "", "also fail on any baseline or ceiling raise against this git ref (the PR's merge-base)")
	allowRaise := flag.Bool("allow-raise", false, "with -growth-since: report raises but pass (the PR carries the storeclass/baseline-raise label)")
	gateOnly := flag.Bool("gate-only", false, "with -growth-since: skip the census; only compare against the ref")
	flag.Parse()
	if !*gateOnly {
		if err := run(*repo, *write, *initial, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "storeclass-baseline:", err)
			os.Exit(1)
		}
	}
	if *since == "" {
		return
	}
	if err := gate(*repo, *since, *allowRaise, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "storeclass-baseline:", err)
		os.Exit(1)
	}
}

func run(repo string, write, initial bool, out io.Writer) error {
	if repo == "" {
		top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return fmt.Errorf("finding the checkout: %w", err)
		}
		repo = strings.TrimSpace(string(top))
	}
	dir := filepath.Join(repo, "tools", "nogo", "analyzers", "storeclass")
	rows, err := census(repo)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(out, summary(rows)); err != nil {
		return err
	}

	baselinePath := filepath.Join(dir, "baseline.txt")
	ceilingPath := filepath.Join(dir, "ceiling.txt")
	baseline := storeclass.FormatBaseline(rows)
	totals := storeclass.Totals(rows)
	ceilingText, err := os.ReadFile(ceilingPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ceiling, err := storeclass.ParseCeiling(string(ceilingText))
	if err != nil {
		return err
	}
	if initial && len(ceiling) > 0 {
		return errors.New("-init: ceiling.txt already has rows; the ceiling only ever lowers")
	}
	refusals := growth(rows, ceiling, storeclass.Default.Sealed, initial)
	onDisk, _ := os.ReadFile(baselinePath)
	matches := bytes.Equal(onDisk, []byte(baseline)) && bytes.Equal(ceilingText, []byte(storeclass.FormatCeiling(totals)))
	if !write && !initial {
		var b strings.Builder
		for _, r := range refusals {
			fmt.Fprintln(&b, "GROWTH:", r)
		}
		if matches {
			b.WriteString("baseline.txt and ceiling.txt match the tree\n")
		}
		if _, err := io.WriteString(out, b.String()); err != nil {
			return err
		}
		if matches {
			return nil
		}
		return errors.New("baseline.txt or ceiling.txt does not match the tree: rerun with -write")
	}
	if len(refusals) > 0 {
		return fmt.Errorf("refusing to grow the baseline; nothing written:\n  %s", strings.Join(refusals, "\n  "))
	}
	if err := os.WriteFile(baselinePath, []byte(baseline), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(ceilingPath, []byte(storeclass.FormatCeiling(totals)), 0o644); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "wrote %s and %s\n", baselinePath, ceilingPath)
	return err
}

// census type-checks the module's production packages once per build-tag
// variant and counts the findings per key. nogo analyzes a package under
// every configuration that compiles it (a gotags test or binary compiles its
// dependencies with the tag too), so the baseline holds the union: each
// key's count from whichever variant compiles its file.
func census(repo string) (map[storeclass.Key]int, error) {
	tags, err := repoBuildTags(repo)
	if err != nil {
		return nil, err
	}
	merged := map[storeclass.Key]int{}
	for _, tag := range append([]string{""}, tags...) {
		rows, err := censusWith(repo, tag)
		if err != nil {
			return nil, err
		}
		mergeVariant(merged, rows)
	}
	return merged, nil
}

// mergeVariant folds one variant's counts into merged, keeping each key's
// largest count: a file's findings are the same in every variant that
// compiles it.
func mergeVariant(merged, rows map[storeclass.Key]int) {
	for k, n := range rows {
		merged[k] = max(merged[k], n)
	}
}

func censusWith(repo, tag string) (map[storeclass.Key]int, error) {
	cfg := &packages.Config{
		Mode:  packages.LoadSyntax,
		Dir:   repo,
		Tests: false,
		Env:   append(os.Environ(), "GOWORK=off"),
	}
	if tag != "" {
		cfg.BuildFlags = []string{"-tags=" + tag}
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("loading packages (tags %q): %w", tag, err)
	}
	// The untagged tree must type-check. A tag alone can leave a package
	// that needs a combination of tags uncompilable; no Bazel target builds
	// it that way, so a tagged variant skips it.
	var loadErrs []string
	var ok []*packages.Package
	for _, p := range pkgs {
		if len(p.Errors) == 0 {
			ok = append(ok, p)
			continue
		}
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	}
	if tag == "" && len(loadErrs) > 0 {
		return nil, fmt.Errorf("type-checking the tree:\n  %s", strings.Join(loadErrs, "\n  "))
	}
	rows := map[storeclass.Key]int{}
	a := storeclass.NewCollector(storeclass.Default, func(f storeclass.Finding) { rows[f.Key]++ })
	graph, err := checker.Analyze([]*analysis.Analyzer{a}, ok, nil)
	if err != nil {
		return nil, err
	}
	for act := range graph.All() {
		if act.Err != nil {
			return nil, fmt.Errorf("%s: %w", act.Package.PkgPath, act.Err)
		}
	}
	return rows, nil
}

// repoBuildTags is every custom build tag a production file of the module
// names in its //go:build line, sorted.
func repoBuildTags(repo string) ([]string, error) {
	seen := map[string]bool{}
	err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch name := d.Name(); {
			case p == repo:
			case name == "testdata" || name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "bazel-"):
				return filepath.SkipDir
			case fileExists(filepath.Join(p, "go.mod")):
				return filepath.SkipDir // another module (tools/nogo, ...)
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, t := range buildTags(string(src)) {
			seen[t] = true
		}
		return nil
	})
	tags := make([]string, 0, len(seen))
	for t := range seen {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	return tags, err
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// platformTags are build tags the toolchain sets from the target, not ones a
// build chooses.
var platformTags = map[string]bool{
	"unix": true, "cgo": true, "gc": true, "gccgo": true, "ignore": true,
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true,
	"illumos": true, "ios": true, "js": true, "linux": true, "nacl": true, "netbsd": true,
	"openbsd": true, "plan9": true, "solaris": true, "wasip1": true, "windows": true, "zos": true,
	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true, "mips": true, "mipsle": true,
	"mips64": true, "mips64le": true, "ppc64": true, "ppc64le": true, "riscv64": true, "s390x": true, "wasm": true,
}

// buildTags is the custom tags src's //go:build constraint names: those a
// build opts into, not platform or release tags.
func buildTags(src string) []string {
	var tags []string
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			continue
		}
		expr.Eval(func(tag string) bool {
			if !platformTags[tag] && !strings.HasPrefix(tag, "go1.") && !slices.Contains(tags, tag) {
				tags = append(tags, tag)
			}
			return true
		})
	}
	sort.Strings(tags)
	return tags
}

// summary is the census: totals per rule, then per package and rule.
func summary(rows map[storeclass.Key]int) string {
	var out strings.Builder
	byRule := map[storeclass.Rule]int{}
	byPkg := map[string]map[storeclass.Rule]int{}
	total := 0
	for k, n := range rows {
		byRule[k.Rule] += n
		if byPkg[k.Package()] == nil {
			byPkg[k.Package()] = map[storeclass.Rule]int{}
		}
		byPkg[k.Package()][k.Rule] += n
		total += n
	}
	fmt.Fprintf(&out, "storeclass census: %d findings in %d rows\n", total, len(rows))
	for _, r := range storeclass.Rules {
		fmt.Fprintf(&out, "  %-17s %6d\n", r, byRule[r])
	}
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	fmt.Fprintf(&out, "\n%-40s", "package")
	for _, r := range storeclass.Rules {
		fmt.Fprintf(&out, " %s", r)
	}
	out.WriteString("\n")
	for _, p := range pkgs {
		fmt.Fprintf(&out, "%-40s", p)
		for _, r := range storeclass.Rules {
			fmt.Fprintf(&out, " %*d", len(r), byPkg[p][r])
		}
		out.WriteString("\n")
	}
	return out.String()
}

// growth is every reason rows may not replace the baseline: a finding in a
// sealed package, and, unless this is the first ceiling, a package/rule
// total above ceiling or absent from it.
func growth(rows map[storeclass.Key]int, ceiling map[storeclass.CeilingKey]int, sealed []string, initial bool) []string {
	var refusals []string
	for k := range rows {
		for _, s := range sealed {
			if k.Package() == s {
				refusals = append(refusals, fmt.Sprintf("%s is sealed but has a finding: %s", s, k))
			}
		}
	}
	if !initial {
		for k, n := range storeclass.Totals(rows) {
			switch pinned, ok := ceiling[k]; {
			case !ok:
				refusals = append(refusals, fmt.Sprintf("%s %s: %d findings where ceiling.txt allows none", k.Package, k.Rule, n))
			case n > pinned:
				refusals = append(refusals, fmt.Sprintf("%s %s: %d findings, ceiling.txt allows %d", k.Package, k.Rule, n, pinned))
			}
		}
	}
	sort.Strings(refusals)
	return refusals
}

const (
	baselineRel = "tools/nogo/analyzers/storeclass/baseline.txt"
	ceilingRel  = "tools/nogo/analyzers/storeclass/ceiling.txt"
)

// gate compares the checkout's baseline and ceiling with ref's: any new row,
// raised count, new ceiling row or raised ceiling is a raise, which fails
// unless allowRaise. A ref with no baseline is the PR that introduces it.
func gate(repo, ref string, allowRaise bool, out io.Writer) error {
	if repo == "" {
		top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return fmt.Errorf("finding the checkout: %w", err)
		}
		repo = strings.TrimSpace(string(top))
	}
	show := func(rel string) (string, bool) {
		b, err := exec.Command("git", "-C", repo, "show", ref+":"+rel).Output()
		return string(b), err == nil
	}
	oldBaseline, ok := show(baselineRel)
	if !ok {
		_, err := fmt.Fprintf(out, "%s has no storeclass baseline: this change introduces it\n", ref)
		return err
	}
	oldCeilingText, _ := show(ceilingRel)
	oldRows, err := storeclass.ParseBaseline(oldBaseline)
	if err != nil {
		return fmt.Errorf("%s baseline: %w", ref, err)
	}
	oldCeil, err := storeclass.ParseCeiling(oldCeilingText)
	if err != nil {
		return fmt.Errorf("%s ceiling: %w", ref, err)
	}
	read := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join(repo, rel))
		return string(b), err
	}
	newBaseline, err := read(baselineRel)
	if err != nil {
		return err
	}
	newCeilingText, err := read(ceilingRel)
	if err != nil {
		return err
	}
	newRows, err := storeclass.ParseBaseline(newBaseline)
	if err != nil {
		return err
	}
	newCeil, err := storeclass.ParseCeiling(newCeilingText)
	if err != nil {
		return err
	}
	rs := raises(oldRows, newRows, oldCeil, newCeil)
	// --no-renames lists a renamed file under its old name too, so moving a
	// file out of a protected path changes that path.
	changed, err := exec.Command("git", "-C", repo, "diff", "--name-only", "--no-renames", ref, "HEAD").Output()
	if err != nil {
		return fmt.Errorf("listing the files changed since %s: %w", ref, err)
	}
	rs = append(rs, protectedChanges(strings.Fields(string(changed)))...)
	if len(rs) == 0 {
		_, err := fmt.Fprintf(out, "no storeclass baseline raise against %s\n", ref)
		return err
	}
	msg := fmt.Sprintf("storeclass baseline raises against %s:\n  %s\n", ref, strings.Join(rs, "\n  "))
	if allowRaise {
		_, err := fmt.Fprintf(out, "%sapproved by the storeclass/baseline-raise label\n", msg)
		return err
	}
	return fmt.Errorf("%snew raw-store debt needs the coordinator's approval: the storeclass/baseline-raise label (see the storeclass package doc)", msg)
}

// protectedPaths govern the lint and its gate: a change to any of them (the
// analyzer, its allowlist and sealed lists, the generator, the rules_go
// patches that keep nolint from suppressing it and hand it the baseline, this
// workflow, the code owners) needs the same approval as a baseline raise.
// baseline.txt and ceiling.txt are judged by raises instead.
var protectedPaths = []string{
	"tools/nogo/analyzers/storeclass/",
	"third_party/patches/rules_go_nogo_golangci_nolint.patch",
	"third_party/patches/rules_go_nogo_main_repo_data.patch",
	".github/workflows/storeclass-baseline.yml",
	".github/CODEOWNERS",
}

// protectedChanges is a refusal for each changed file under protectedPaths.
func protectedChanges(changed []string) []string {
	var out []string
	for _, f := range changed {
		if f == baselineRel || f == ceilingRel {
			continue
		}
		for _, p := range protectedPaths {
			if f == p || strings.HasSuffix(p, "/") && strings.HasPrefix(f, p) {
				out = append(out, "protected path changed: "+f)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// raises is every way the new baseline and ceiling grow past the old.
func raises(oldRows, newRows map[storeclass.Key]int, oldCeil, newCeil map[storeclass.CeilingKey]int) []string {
	var out []string
	for k, n := range newRows {
		switch was, ok := oldRows[k]; {
		case !ok:
			out = append(out, fmt.Sprintf("new row %s (%d)", k, n))
		case n > was:
			out = append(out, fmt.Sprintf("%s raised %d -> %d", k, was, n))
		}
	}
	for k, n := range newCeil {
		switch was, ok := oldCeil[k]; {
		case !ok:
			out = append(out, fmt.Sprintf("new ceiling row %s %s (%d)", k.Package, k.Rule, n))
		case n > was:
			out = append(out, fmt.Sprintf("ceiling %s %s raised %d -> %d", k.Package, k.Rule, was, n))
		}
	}
	sort.Strings(out)
	return out
}
