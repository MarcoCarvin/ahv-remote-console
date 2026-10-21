package main

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Two ISO builds from identical inputs must be byte-identical when -mtime is
// fixed: the catalog image the ISO uploads to is deduped by the SHA-256
// embedded in its NAME, so timestamp churn (go-diskfs stamps wall-clock into
// the PVD/SVD and every directory record) would re-upload a "new" image on
// every rebuild. normalizeTimestamps exists to close exactly that gap.
func TestISOReproducibleWithFixedMtime(t *testing.T) {
	agentDir := t.TempDir()
	// Minimal staged-input set: one binary per platform plus a package, so
	// the walk covers the /linux subdirectory in both hierarchies.
	for name, data := range map[string]string{
		"ahv-remote-console-agent-windows-amd64.exe": "fake exe bytes",
		"ahv-remote-console-agent-linux-amd64":       "fake elf bytes",
		"ahv-remote-console-agent.rpm":               "fake rpm bytes",
	} {
		if err := os.WriteFile(filepath.Join(agentDir, name), []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	outDir := t.TempDir()
	sums := map[[sha256.Size]byte]bool{}
	for i := 0; i < 2; i++ {
		out := filepath.Join(outDir, "agent.iso")
		_ = os.Remove(out) // diskfs.Create refuses to overwrite
		// Sleep across a second boundary so a regression to wall-clock
		// timestamps cannot accidentally pass.
		if i == 1 {
			time.Sleep(1100 * time.Millisecond)
		}
		if err := run([]string{
			"-agent-dir", agentDir,
			"-out", out,
			"-version", "test",
			"-mtime", "1750000000",
		}); err != nil {
			t.Fatalf("run #%d: %v", i+1, err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		sums[sha256.Sum256(data)] = true
	}
	if len(sums) != 1 {
		t.Fatalf("identical inputs with fixed -mtime produced %d distinct ISOs", len(sums))
	}
}
