package main

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestSyncFileWriterConcurrentWriteTruncate proves GO-R-02: routing both the
// slog handler writes and the periodic truncation through a single
// syncFileWriter lock means concurrent writes + Truncate+Seek produce no data
// race (under -race) and no garbled/interleaved JSON. Before the fix the
// truncator called f.Truncate(0)+f.Seek(0,0) directly on the same *os.File the
// handler wrote to, racing the file offset.
func TestSyncFileWriterConcurrentWriteTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hcbridge.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	sf := &syncFileWriter{f: f}
	logger := slog.New(slog.NewJSONHandler(sf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	const writers = 8
	const linesPerWriter = 400

	var wg sync.WaitGroup

	// Many goroutines logging concurrently through the same handler/writer.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < linesPerWriter; i++ {
				logger.Info("tick", "writer", id, "seq", i, "payload", "xxxxxxxxxxxxxxxxxxxx")
			}
		}(w)
	}

	// A truncator hammering Truncate+Seek through the SAME lock, mirroring
	// truncateLogPeriodically but without the 5-minute ticker so the race
	// window is exercised tightly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := sf.truncate(); err != nil {
				t.Errorf("truncate: %v", err)
				return
			}
		}
	}()

	wg.Wait()

	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Every line that survives truncation must be intact, well-formed JSON.
	// A bypassed-lock race would interleave bytes from concurrent writes (or
	// a write atop a just-seeked offset), yielding lines that don't parse.
	rf, err := os.Open(path)
	if err != nil {
		t.Fatalf("reopen log: %v", err)
	}
	defer rf.Close()

	scanner := bufio.NewScanner(rf)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lines := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("garbled JSON line (interleaved write?): %q: %v", line, err)
		}
		if _, ok := rec["msg"]; !ok {
			t.Fatalf("JSON line missing msg field (corruption): %q", line)
		}
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if lines == 0 {
		t.Fatal("expected at least some surviving log lines")
	}
}

// TestTruncateLogPeriodicallyHelpers exercises the size/truncate helpers that
// truncateLogPeriodically relies on, confirming truncate rewinds the offset so
// the file actually shrinks rather than leaving a sparse tail.
func TestSyncFileWriterTruncateShrinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hcbridge.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	sf := &syncFileWriter{f: f}
	if _, err := sf.Write([]byte("aaaaaaaaaaaaaaaaaaaa\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	size, err := sf.size()
	if err != nil {
		t.Fatalf("size: %v", err)
	}
	if size == 0 {
		t.Fatal("expected non-zero size after write")
	}
	if err := sf.truncate(); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	size, err = sf.size()
	if err != nil {
		t.Fatalf("size after truncate: %v", err)
	}
	if size != 0 {
		t.Fatalf("expected size 0 after truncate, got %d", size)
	}
	// A subsequent write must start at offset 0, not leave a NUL-padded gap.
	if _, err := sf.Write([]byte("b\n")); err != nil {
		t.Fatalf("write after truncate: %v", err)
	}
	size, err = sf.size()
	if err != nil {
		t.Fatalf("size after second write: %v", err)
	}
	if size != int64(len("b\n")) {
		t.Fatalf("expected size %d after post-truncate write, got %d (offset not rewound?)", len("b\n"), size)
	}
}
