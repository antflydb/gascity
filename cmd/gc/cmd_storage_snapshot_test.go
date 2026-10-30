package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageMaintenanceReportsBindingErrors(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nprovider = \"claude\"\n[providers.claude]\nbase = \"builtin:claude\"\n[beads]\nprovider = \"file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", t.TempDir())
	for _, verb := range []string{"backup", "restore", "gc", "retain"} {
		t.Run(verb, func(t *testing.T) {
			output := filepath.Join(city, "unexpected.archive")
			args := []string{"--city", city, "storage", verb, "absent"}
			if verb == "backup" || verb == "restore" {
				args = append(args, "--file", output)
			}
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code == 0 {
				t.Fatal("unknown binding succeeded")
			}
			if !strings.Contains(stderr.String(), "gc storage "+verb+":") || !strings.Contains(stderr.String(), "binding") {
				t.Fatalf("failure was silent or unclear: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("failed operator action wrote an archive: %v", err)
			}
		})
	}
}
