package vdagent

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// buildFrames produces protocol frames for text exactly as the renderer
// sender does (gzip → split → per-chunk base64 → crc over the b64 bytes).
func buildFrames(t *testing.T, xferID, text string, chunkSize int) []string {
	t.Helper()
	var zbuf bytes.Buffer
	zw := gzip.NewWriter(&zbuf)
	if _, err := zw.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := zbuf.Bytes()
	total := (len(compressed) + chunkSize - 1) / chunkSize
	if total == 0 {
		total = 1
	}
	frames := make([]string, 0, total)
	for seq := 1; seq <= total; seq++ {
		lo := (seq - 1) * chunkSize
		hi := min(seq*chunkSize, len(compressed))
		b64 := base64.StdEncoding.EncodeToString(compressed[lo:hi])
		frames = append(frames, fmt.Sprintf("%s:%d:%d:%d:%s:%s",
			xferID, seq, total, len(text), crc32Hex([]byte(b64)), b64))
	}
	return frames
}

func TestBlobIngestRoundTrip(t *testing.T) {
	text := strings.Repeat("kind: Deployment\nmetadata:\n  name: web\n", 4000) // ~156 KB
	frames := buildFrames(t, "aabbccdd", text, 256)
	if len(frames) < 2 {
		t.Fatalf("want a multi-frame transfer, got %d frames", len(frames))
	}
	var st blobRx
	for i, f := range frames {
		res := st.ingest(f, MaxClipboardSize)
		last := i == len(frames)-1
		wantStatus := "ok"
		if last {
			wantStatus = "done"
		}
		wantAck := fmt.Sprintf("%saabbccdd:%d:%s", blobAckPrefix, i+1, wantStatus)
		if res.Ack != wantAck {
			t.Fatalf("frame %d ack = %q, want %q", i+1, res.Ack, wantAck)
		}
		if last {
			if !res.Complete || res.Text != text {
				t.Fatalf("final frame: complete=%v textLen=%d wantLen=%d", res.Complete, len(res.Text), len(text))
			}
		} else if res.Complete {
			t.Fatalf("frame %d completed early", i+1)
		}
	}
}

func TestBlobIngestDuplicateAndOutOfOrder(t *testing.T) {
	text := strings.Repeat("x", 30000)
	frames := buildFrames(t, "00112233", text, 16)
	var st blobRx
	// Consume frame 1, then re-deliver it (lost ack) — must re-ack ok without
	// double-appending; then an out-of-order future frame is dropped silently.
	if res := st.ingest(frames[0], MaxClipboardSize); res.Ack != blobAck("00112233", 1, "ok") {
		t.Fatalf("frame 1 ack = %q", res.Ack)
	}
	if res := st.ingest(frames[0], MaxClipboardSize); res.Ack != blobAck("00112233", 1, "ok") {
		t.Fatalf("dup frame 1 ack = %q", res.Ack)
	}
	if res := st.ingest(frames[3], MaxClipboardSize); res.Ack != "" {
		t.Fatalf("out-of-order frame ack = %q, want silent drop", res.Ack)
	}
	// Delivery continues in order and still round-trips.
	for i := 1; i < len(frames); i++ {
		res := st.ingest(frames[i], MaxClipboardSize)
		if i == len(frames)-1 {
			if !res.Complete || res.Text != text {
				t.Fatalf("round trip failed after dup/out-of-order")
			}
		}
	}
}

func TestBlobIngestCrcMismatchIsRetryable(t *testing.T) {
	text := strings.Repeat("y", 5000)
	frames := buildFrames(t, "0a0b0c0d", text, 16)
	var st blobRx
	// Corrupt frame 1's payload tail (keeps the declared crc → mismatch).
	bad := frames[0][:len(frames[0])-2] + "zz"
	if res := st.ingest(bad, MaxClipboardSize); res.Ack != blobAck("0a0b0c0d", 1, "err:crc") {
		t.Fatalf("corrupt frame ack = %q, want err:crc", res.Ack)
	}
	// Retransmit of the intact frame recovers the transfer.
	for i, f := range frames {
		res := st.ingest(f, MaxClipboardSize)
		if i == len(frames)-1 && (!res.Complete || res.Text != text) {
			t.Fatalf("transfer did not recover after crc retry")
		}
	}
}

func TestBlobIngestTooLargeAndSupersede(t *testing.T) {
	var st blobRx
	// rawLen over the cap is refused outright.
	over := fmt.Sprintf("ffffffff:1:1:%d:%s:%s", MaxClipboardSize+1, crc32Hex([]byte("QQ==")), "QQ==")
	if res := st.ingest(over, MaxClipboardSize); res.Ack != blobAck("ffffffff", 1, "err:too_large") {
		t.Fatalf("oversize ack = %q", res.Ack)
	}
	// A new xferId mid-transfer supersedes the old one (latest copy wins).
	// Tiny chunk size: repeated text gzips to a few dozen bytes, and this
	// case needs the OLD transfer to span multiple frames.
	oldFrames := buildFrames(t, "11111111", strings.Repeat("a", 9000), 16)
	newText := "fresh"
	newFrames := buildFrames(t, "22222222", newText, 1024)
	st = blobRx{}
	st.ingest(oldFrames[0], MaxClipboardSize)
	res := st.ingest(newFrames[0], MaxClipboardSize)
	if !res.Complete || res.Text != newText {
		t.Fatalf("supersede failed: complete=%v text=%q", res.Complete, res.Text)
	}
	// Old transfer's next frame is now unknown → silently dropped.
	if res := st.ingest(oldFrames[1], MaxClipboardSize); res.Ack != "" {
		t.Fatalf("stale frame ack = %q, want silent drop", res.Ack)
	}
}

func TestBlobIngestDoneDupReacked(t *testing.T) {
	text := "small payload"
	frames := buildFrames(t, "beefbeef", text, 1024)
	if len(frames) != 1 {
		t.Fatalf("want single-frame transfer")
	}
	var st blobRx
	if res := st.ingest(frames[0], MaxClipboardSize); !res.Complete {
		t.Fatal("transfer did not complete")
	}
	// The done ack was lost; the sender retransmits the final frame — it must
	// be re-acked done WITHOUT rerunning the transfer (Complete=false).
	res := st.ingest(frames[0], MaxClipboardSize)
	if res.Complete || res.Ack != blobAck("beefbeef", 1, "done") {
		t.Fatalf("post-done dup: complete=%v ack=%q", res.Complete, res.Ack)
	}
}

func TestGunzipTextLengthGuard(t *testing.T) {
	var zbuf bytes.Buffer
	zw := gzip.NewWriter(&zbuf)
	_, _ = zw.Write([]byte("0123456789"))
	_ = zw.Close()
	if _, err := gunzipText(zbuf.Bytes(), 10); err != nil {
		t.Fatalf("exact length failed: %v", err)
	}
	if _, err := gunzipText(zbuf.Bytes(), 9); err == nil {
		t.Fatal("declared length shorter than payload must fail (bomb guard)")
	}
	if _, err := gunzipText(zbuf.Bytes(), 11); err == nil {
		t.Fatal("declared length longer than payload must fail")
	}
}
