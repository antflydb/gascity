package storeclass

import (
	_ "embed"
	"fmt"
)

const (
	gascity = "github.com/gastownhall/gascity/"
	beads   = gascity + "internal/beads"
)

//go:embed baseline.txt
var baselineText string

// BaselineText is the embedded baseline, as generated.
func BaselineText() string { return baselineText }

// Default is the gascity configuration nogo runs.
var Default = Config{
	Module:      gascity,
	Store:       beads + ".Store",
	HandleField: "Store",
	Handles: []string{
		beads + ".WorkStore",
		beads + ".GraphStore",
		beads + ".SessionStore",
		beads + ".MailStore",
		beads + ".OrdersStore",
		beads + ".NudgesStore",
	},
	UnwrapMethods: []string{"Raw", "ConditionalWritesResolveTarget"},
	AsFuncs:       []string{beads + ".As"},
	// The mint lands with the router (T2): storeref.NewStores is its only
	// production caller. Until then the handles are minted by literal, and
	// every literal outside the allowlist is baselined.
	MintFuncs: []string{beads + ".MintClassStores"},
	TestMintPackages: []string{
		beads + "/beadstest",
		beads + "/splittest",
	},
	// D9: the split-strict gate's markers (#8335) are frozen. A call site
	// lands only as a reviewed baseline row; T7 replaces them with plan roles.
	Markers: []string{
		beads + ".FederatedRead",
		beads + ".FederatedGet",
		beads + ".RelicRead",
		beads + ".RelicGet",
	},
	Allow: []Allow{
		{Package: "internal/beads/...", Reason: "the engines, the typed handles, the capability helpers, and the test minting helpers (beadstest, splittest)"},
		{Package: "internal/storebinding/...", Reason: "the binding engines and the migration copy and clear: they move rows between physical stores, raw by nature"},
		{Package: "internal/storeref", Rules: AllRulesBut(CapAssert), Reason: "the residency resolver: the mint (T2) and the plan executors that run legs over physical stores. A capability assertion here still goes through beads.As"},
		{Package: "cmd/gc", File: "storage_boot.go", Reason: "storage-class routing at boot: opens and holds the physical engines the router later wraps"},
		{Package: "cmd/gc", File: "infra_class_migrate.go", Rules: rawRules, Reason: "the storage-class migration: copies a class's rows between physical stores, raw by nature (S5)"},
		{Package: "cmd/gc", File: "infra_class_clear.go", Rules: rawRules, Reason: "the migration's clear: deletes migrated rows from the source physical store"},
		{Package: "cmd/gc", File: "infra_class_recover.go", Rules: rawRules, Reason: "the stranded-write recovery: moves rows between physical stores"},
		{Package: "cmd/gc", File: "infra_class_recover_held.go", Rules: rawRules, Reason: "the stranded-write recovery for held rows: moves rows between physical stores"},
		{Package: "cmd/gc", File: "infra_class_relower.go", Rules: rawRules, Reason: "the reverse migration: copies rows back to the work store"},
	},
	// Sealed packages have no baseline rows: a phase ends by adding its
	// package here, and the list only grows (TestSealedOnlyGrows).
	Sealed: []string{
		"internal/workladder",
	},
	Baseline: mustParseBaseline(baselineText),
}

// rawRules are the raw-signature rules: the migration engine takes physical
// stores, but it still mints, unwraps and asserts only through the baseline.
var rawRules = []Rule{RawParam, RawResult, RawField, RawVar, RawType}

// Analyzer runs the rules with Default.
var Analyzer = New(Default)

func mustParseBaseline(text string) map[Key]int {
	rows, err := ParseBaseline(text)
	if err != nil {
		panic(fmt.Sprintf("storeclass: embedded baseline.txt: %v", err))
	}
	return rows
}
