package main

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPackageVersion(t *testing.T) {
	cases := map[string]string{
		"v1.2.3":            "1.2.3",
		"v0.4.0-12-gabc123": "0.4.0.12.gabc123", // rpm forbids dashes
		"dev":               "0.0.0.dev",
		"":                  "0.0.0.dev",
		"v1.0.0_rc1!":       "1.0.0.rc1", // no empty/trailing segments
		// Non-semver tags (the repo's real `git describe` shape): Debian
		// policy requires a digit-leading version, so these get a numeric
		// prefix instead of sorting before every numeric version.
		"stable-request-pump-6-gd354f74-dirty": "0.0.0.stable.request.pump.6.gd354f74.dirty",
		"!!!":                                  "0.0.0.dev",
	}
	for in, want := range cases {
		if got := packageVersion(in); got != want {
			t.Errorf("packageVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// Build both package formats from a stand-in binary + icon and verify each
// container's magic bytes — proof the nfpm wiring produces real rpm/deb
// archives a guest package manager will accept.
func TestBuildPackageFormats(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	icon := filepath.Join(dir, "icon.png")
	desktop := filepath.Join(dir, "setup.desktop")
	for _, f := range []struct {
		path string
		data []byte
	}{
		{bin, []byte("#!fake-elf")},
		{icon, []byte("\x89PNG fake")},
		{desktop, setupDesktop},
	} {
		if err := os.WriteFile(f.path, f.data, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	stamp := time.Unix(1750000000, 0).UTC()
	magics := map[string][]byte{
		"rpm": {0xED, 0xAB, 0xEE, 0xDB}, // RPM lead magic
		"deb": []byte("!<arch>\n"),      // ar archive magic
	}
	for format, magic := range magics {
		out := filepath.Join(dir, "out."+format)
		if err := buildPackage(format, bin, icon, desktop, "v1.2.3-4-gabc", stamp, out); err != nil {
			t.Fatalf("buildPackage(%s): %v", format, err)
		}
		head := make([]byte, len(magic))
		f, err := os.Open(out)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Read(head); err != nil {
			t.Fatalf("read %s head: %v", format, err)
		}
		f.Close()
		if !bytes.Equal(head, magic) {
			t.Errorf("%s: magic = %x, want %x", format, head, magic)
		}
	}
}

// Packages must be byte-for-byte reproducible for a fixed -mtime: the catalog
// image the ISO uploads to is deduped by the SHA-256 embedded in its NAME, so
// timestamp churn in package metadata would re-upload an "new" image on every
// rebuild of identical inputs (the known catalog-growth failure mode).
func TestBuildPackageDeterministic(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	icon := filepath.Join(dir, "icon.png")
	desktop := filepath.Join(dir, "setup.desktop")
	for _, p := range []string{bin, icon, desktop} {
		if err := os.WriteFile(p, []byte("payload"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	stamp := time.Unix(1750000000, 0).UTC()
	for _, format := range []string{"rpm", "deb"} {
		sums := map[[sha256.Size]byte]bool{}
		for i := 0; i < 2; i++ {
			out := filepath.Join(dir, "det."+format)
			// Sleep across a second boundary so a regression to wall-clock
			// timestamps cannot accidentally pass.
			if i == 1 {
				time.Sleep(1100 * time.Millisecond)
			}
			if err := buildPackage(format, bin, icon, desktop, "v1.0.0", stamp, out); err != nil {
				t.Fatalf("buildPackage(%s): %v", format, err)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			sums[sha256.Sum256(data)] = true
		}
		if len(sums) != 1 {
			t.Errorf("%s: identical inputs with fixed mtime produced %d distinct outputs", format, len(sums))
		}
	}
}
