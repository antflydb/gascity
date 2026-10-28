package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/storeclass"
)

func TestGrowthRefusesAnyRise(t *testing.T) {
	ceiling := map[storeclass.CeilingKey]int{
		{Package: "cmd/gc", Rule: storeclass.RawParam}: 3,
	}
	row := func(p, fn string, r storeclass.Rule) storeclass.Key {
		return storeclass.Key{Path: p, Func: fn, Rule: r}
	}
	for name, tc := range map[string]struct {
		rows    map[storeclass.Key]int
		sealed  []string
		initial bool
		want    string
	}{
		"at the ceiling": {rows: map[storeclass.Key]int{row("cmd/gc/a.go", "f", storeclass.RawParam): 3}},
		"below it":       {rows: map[storeclass.Key]int{row("cmd/gc/a.go", "f", storeclass.RawParam): 1}},
		"above it, spread over two files": {
			rows: map[storeclass.Key]int{row("cmd/gc/a.go", "f", storeclass.RawParam): 2, row("cmd/gc/b.go", "g", storeclass.RawParam): 2},
			want: "cmd/gc raw-param: 4 findings, ceiling.txt allows 3",
		},
		"a new rule": {
			rows: map[storeclass.Key]int{row("cmd/gc/a.go", "f", storeclass.Mint): 1},
			want: "cmd/gc mint: 1 findings where ceiling.txt allows none",
		},
		"a new package": {
			rows: map[storeclass.Key]int{row("internal/x/a.go", "f", storeclass.RawParam): 1},
			want: "internal/x raw-param: 1 findings where ceiling.txt allows none",
		},
		"a sealed package": {
			rows:   map[storeclass.Key]int{row("cmd/gc/a.go", "f", storeclass.RawParam): 1},
			sealed: []string{"cmd/gc"},
			want:   "cmd/gc is sealed",
		},
		"the first ceiling takes anything unsealed": {
			rows:    map[storeclass.Key]int{row("internal/x/a.go", "f", storeclass.RawParam): 9},
			initial: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := growth(tc.rows, ceiling, tc.sealed, tc.initial)
			switch {
			case tc.want == "" && len(got) != 0:
				t.Fatalf("refused: %v", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Fatalf("refusals = %v, want one containing %q", got, tc.want)
			}
		})
	}
}

func TestBuildTagsOfAFile(t *testing.T) {
	got := buildTags("//go:build integration && (linux || darwin) && !productmetrics_testhook && go1.22 && !ignore\n\npackage p\n")
	want := []string{"integration", "productmetrics_testhook"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("buildTags = %v, want %v", got, want)
	}
	if got := buildTags("package p\n\n//go:build integration\n"); len(got) != 0 {
		t.Fatalf("a constraint after the package clause counted: %v", got)
	}
	if got := buildTags("// Copyright\n\n//go:build unix && cgo\n\npackage p\n"); len(got) != 0 {
		t.Fatalf("platform tags counted: %v", got)
	}
}

func TestMergeVariantsTakesEachKeysMax(t *testing.T) {
	k := func(p string) storeclass.Key { return storeclass.Key{Path: p, Func: "f", Rule: storeclass.RawParam} }
	merged := map[storeclass.Key]int{}
	mergeVariant(merged, map[storeclass.Key]int{k("a.go"): 2, k("b.go"): 1})
	mergeVariant(merged, map[storeclass.Key]int{k("a.go"): 2, k("c.go"): 3})
	if merged[k("a.go")] != 2 || merged[k("b.go")] != 1 || merged[k("c.go")] != 3 || len(merged) != 3 {
		t.Fatalf("merged = %v", merged)
	}
}

func TestRaisesAgainstTheMergeBase(t *testing.T) {
	k := func(p, fn string) storeclass.Key { return storeclass.Key{Path: p, Func: fn, Rule: storeclass.RawParam} }
	c := storeclass.CeilingKey{Package: "cmd/gc", Rule: storeclass.RawParam}
	oldRows := map[storeclass.Key]int{k("cmd/gc/a.go", "f"): 2, k("cmd/gc/b.go", "g"): 1}
	oldCeil := map[storeclass.CeilingKey]int{c: 3}
	for name, tc := range map[string]struct {
		rows map[storeclass.Key]int
		ceil map[storeclass.CeilingKey]int
		want []string
	}{
		"unchanged":         {rows: oldRows, ceil: oldCeil},
		"a shrink":          {rows: map[storeclass.Key]int{k("cmd/gc/a.go", "f"): 1}, ceil: map[storeclass.CeilingKey]int{c: 1}},
		"a new key":         {rows: map[storeclass.Key]int{k("cmd/gc/a.go", "f"): 2, k("cmd/gc/c.go", "h"): 1}, ceil: oldCeil, want: []string{"new row cmd/gc/c.go\th\traw-param (1)"}},
		"a raised count":    {rows: map[storeclass.Key]int{k("cmd/gc/a.go", "f"): 3}, ceil: oldCeil, want: []string{"cmd/gc/a.go\tf\traw-param raised 2 -> 3"}},
		"a raised ceiling":  {rows: oldRows, ceil: map[storeclass.CeilingKey]int{c: 4}, want: []string{"ceiling cmd/gc raw-param raised 3 -> 4"}},
		"a new ceiling row": {rows: oldRows, ceil: map[storeclass.CeilingKey]int{c: 3, {Package: "internal/x", Rule: storeclass.Mint}: 1}, want: []string{"new ceiling row internal/x mint (1)"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := raises(oldRows, tc.rows, oldCeil, tc.ceil)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("raises = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProtectedChanges(t *testing.T) {
	got := protectedChanges([]string{
		"tools/nogo/analyzers/storeclass/baseline.txt",
		"tools/nogo/analyzers/storeclass/ceiling.txt",
		"tools/nogo/analyzers/storeclass/config.go",
		"tools/nogo/analyzers/storeclass/sealed_floor.txt",
		"tools/nogo/analyzers/storeclass/storeclass.go",
		"tools/nogo/analyzers/storeclass/cmd/storeclass-baseline/main.go",
		".github/workflows/storeclass-baseline.yml",
		".github/CODEOWNERS",
		"cmd/gc/main.go",
	})
	want := []string{
		"protected path changed: .github/CODEOWNERS",
		"protected path changed: .github/workflows/storeclass-baseline.yml",
		"protected path changed: tools/nogo/analyzers/storeclass/cmd/storeclass-baseline/main.go",
		"protected path changed: tools/nogo/analyzers/storeclass/config.go",
		"protected path changed: tools/nogo/analyzers/storeclass/sealed_floor.txt",
		"protected path changed: tools/nogo/analyzers/storeclass/storeclass.go",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("protectedChanges = %q\nwant %q", got, want)
	}
}
