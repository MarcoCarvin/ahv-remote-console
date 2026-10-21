package vdagent

import (
	"context"
	"testing"
)

// TestEmptyHostClipboardNotWritten: an empty host→guest payload must NEVER be
// written to the guest clipboard. QEMU hands us an empty clipboard when its
// host-side peer state was reset by a reconnect; writing it wipes whatever the
// operator just copied in the guest, and because the guest clipboard is the
// SHARED endpoint that breaks BOTH directions ("new content never updates").
func TestEmptyHostClipboardNotWritten(t *testing.T) {
	writeCount := func(c *fakeClipboard) int {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.written)
	}

	clip := &fakeClipboard{}
	a := newTestAgent(&fakePort{}, clip)

	// Seed a prior echo hash (as if the operator just copied something): an empty
	// write must NOT touch it, or the next genuine identical copy is suppressed.
	a.mu.Lock()
	a.lastClipHash = "prior-copy-hash"
	a.mu.Unlock()

	if err := a.sanitizeAndWriteClipboard(context.Background(), ""); err != nil {
		t.Fatalf("empty write returned error: %v", err)
	}
	if n := writeCount(clip); n != 0 {
		t.Fatalf("empty host clipboard was written to the guest (%d writes) — clobbers fresh content", n)
	}
	a.mu.Lock()
	h := a.lastClipHash
	a.mu.Unlock()
	if h != "prior-copy-hash" {
		t.Fatalf("empty write recorded a self-write hash (%q) — would echo-suppress the next copy", h)
	}

	// Content that SANITIZES to empty (all control chars) is likewise skipped.
	if err := a.sanitizeAndWriteClipboard(context.Background(), "\x01\x02\x1b\x7f"); err != nil {
		t.Fatalf("sanitize-to-empty returned error: %v", err)
	}
	if n := writeCount(clip); n != 0 {
		t.Fatalf("all-control-char content (sanitizes to empty) was written (%d)", n)
	}

	// A real, non-empty payload IS written (guard doesn't over-reach).
	if err := a.sanitizeAndWriteClipboard(context.Background(), "real content"); err != nil {
		t.Fatalf("real write returned error: %v", err)
	}
	if n := writeCount(clip); n != 1 {
		t.Fatalf("real content was not written exactly once (%d)", n)
	}
}
