package config

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/builtinpacks"
)

// gascity-packs 9b4df3e ("route shipped beads commands through gc",
// 2026-07-13) moved every shipped bead command onto `gc bd`. Graph.v2 steps
// and roots live in the city binding on a split-store city; a raw `bd` in a
// role prompt, workflow page or check script reads the agent's work store
// instead, so role workers loop on CLAIM_REJECTED, steps never close and
// every check fails. These tests keep the public pack pins, and the copy the
// binary embeds, at or past that fix.

// preBdRoutingFixPins are the gascity-packs registry release commits that
// predate 9b4df3e. Fresh init must never pin one of them again.
var preBdRoutingFixPins = map[string]string{
	// gascity releases 0.1.1 through 0.1.6.
	"sha:788b6e8ec224a8951c728ef6da74dab8bc04d474": "gascity 0.1.1",
	"sha:5fc675b85d4ae0ebca2f17cb027a24b03f2832f8": "gascity 0.1.2",
	"sha:af1640917a24f88126c37a1e3697a619b731cc0f": "gascity 0.1.3",
	"sha:99464ed9240b1f6e6b7ab1d351f67016e1a973ff": "gascity 0.1.4",
	"sha:3b3b89f2011e06d84459aa7bea1552382f13930a": "gascity 0.1.6",
	// gastown releases 0.1.6 through 0.1.11. 0.1.11 was cut on
	// release/gastown-0.1.11 from 0.1.10 and does not carry the fix.
	"sha:4212acb7046c11f6f633df73307006493185233a": "gastown 0.1.6",
	"sha:342bcfb0775ad79d2c67df3b235edf70a0a7e372": "gastown 0.1.7",
	"sha:2cd3360f36b2ff55f6c306546841963cbca1ed69": "gastown 0.1.8",
	"sha:92a9e8558b86854264ccc082fe9f27d48db3c749": "gastown 0.1.9",
	"sha:33d3a430a67d1782ad364556cb566bdb01d0afe3": "gastown 0.1.10",
	"sha:66c672b5ed00af143158b4eac960523c56b977ee": "gastown 0.1.11",
}

func TestPublicPackPinsPostdateBdRoutingFix(t *testing.T) {
	for name, pin := range map[string]string{
		"PublicGastownPackVersion": PublicGastownPackVersion,
		"PublicGascityPackVersion": PublicGascityPackVersion,
	} {
		if release, bad := preBdRoutingFixPins[pin]; bad {
			t.Errorf("%s = %s (%s), which predates gascity-packs 9b4df3e: its shipped prompts and check scripts run raw bd against graph steps", name, pin, release)
		}
	}
}

// The gastown pin and the embedded module are one commit
// (scripts/update-bundled-gastown-pack pins go.mod with go get), so the
// embedded-content scan below speaks for the pinned commit. CI's
// `update-bundled-gastown-pack --check` ties the gascity pin to the same
// embedded gascity tree.
func TestPublicGastownPinIsEmbeddedModuleCommit(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := bazeltest.OverrideRoot()
	if repoRoot == "" {
		repoRoot = filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*github\.com/gastownhall/gascity-packs\s+v\S+-([0-9a-f]{12})\s*$`).FindSubmatch(data)
	if m == nil {
		t.Fatal("go.mod: gascity-packs is not pinned to a commit pseudo-version")
	}
	if !strings.HasPrefix(PublicGastownPackVersion, "sha:"+string(m[1])) {
		t.Errorf("PublicGastownPackVersion = %s, but go.mod embeds gascity-packs %s", PublicGastownPackVersion, m[1])
	}
}

var (
	rawBdCommand  = regexp.MustCompile(`\bbd[ \t]+(show|update|close|list|create|ready|query|sql|dep|label|mol|reopen|delete|search|children|count)\b`)
	gcRoutePrefix = regexp.MustCompile(`\bgc(?:[ \t]+--(?:city|rig)(?:=\S+|[ \t]+\S+))*[ \t]+$`)
)

// rawBdInvocations reports the lines of text that run bd directly instead
// of through `gc bd`.
func rawBdInvocations(text string) []int {
	var lines []int
	for i, line := range strings.Split(text, "\n") {
		for _, loc := range rawBdCommand.FindAllStringIndex(line, -1) {
			prefix := line[:loc[0]]
			if gcRoutePrefix.MatchString(prefix) {
				continue
			}
			// Part of a longer word or path (gc-bd, /usr/bin/bd is still raw).
			if loc[0] > 0 && strings.ContainsRune("-_.", rune(line[loc[0]-1])) {
				continue
			}
			lines = append(lines, i+1)
			break
		}
	}
	return lines
}

func TestRawBdInvocationsDetector(t *testing.T) {
	for line, want := range map[string]bool{
		`bd show "$WORK_ID" --json`:                        true,
		`  STATUS=$(bd show "$ID" --json | jq -r .status)`: true,
		"run `bd close $GC_BEAD_ID`":                       true,
		`/usr/local/bin/bd update x --status closed`:       true,
		`gc bd show "$WORK_ID" --json`:                     false,
		`gc --city "$GC_CITY" bd update x`:                 false,
		`gc --rig=r bd close x`:                            false,
		`a gc-bd list wrapper`:                             false,
		`the bd CLI`:                                       false,
	} {
		if got := len(rawBdInvocations(line)) > 0; got != want {
			t.Errorf("rawBdInvocations(%q) = %v, want %v", line, got, want)
		}
	}
}

// The shipped prompts, workflow pages, formulas and scripts of the embedded
// public packs must not run bd directly.
func TestEmbeddedPublicPacksRouteBeadsCommandsThroughGC(t *testing.T) {
	scanned := 0
	for _, pack := range builtinpacks.All() {
		if pack.Name != "gastown" && pack.Name != "gascity" {
			continue
		}
		err := fs.WalkDir(pack.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "tests" || d.Name() == "vendor" {
					return fs.SkipDir
				}
				return nil
			}
			switch path.Ext(p) {
			case ".md", ".tmpl", ".toml", ".sh", ".bash", ".py":
			default:
				return nil
			}
			data, err := fs.ReadFile(pack.FS, p)
			if err != nil {
				return err
			}
			scanned++
			for _, n := range rawBdInvocations(string(data)) {
				t.Errorf("%s/%s:%d runs bd directly; use gc bd so graph steps resolve in the binding", pack.Name, p, n)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", pack.Name, err)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no embedded gastown/gascity pack files")
	}
}
