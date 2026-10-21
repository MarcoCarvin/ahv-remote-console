// Big-Clipboard Protocol (BCP) — guest→host sender (the "reverse" direction).
//
// A large guest copy cannot cross in one message: QEMU's VNC server SILENTLY
// drops any guest→host clipboard PROVIDE whose DEFLATED size exceeds 1 MiB
// (ui/vnc-clipboard.c vnc_clipboard_provide: "if (!zbuf) return;"), and it
// ignores the client's advertised max-clipboard cap. So the same re-shaping
// the host→guest path uses (bigclip.go / BigClipboard.ts) is applied in
// reverse: gzip(text) is split into small base64 chunks, each sent as an
// ordinary guest→host clipboard transfer (via the grab mechanism), stop-and-
// wait ACK-paced so QEMU's clipboard — last-writer-wins, no queue — never has
// a second frame clobber the first before the host requested it.
//
// This is the exact mirror of the renderer's BigClipboardSender: identical
// wire grammar, identical stop-and-wait/retry/supersede discipline. The only
// structural difference is Go's concurrency — the send runs on a background
// goroutine (so pollClipboard keeps watching), and the per-frame ACK arrives
// on the read-loop goroutine (handleClipboardData → onAck), so the two are
// synchronised through blobTx.
//
// Wire grammar (keep in lockstep with the host→guest sender/receiver):
//
//	frame: __AHVRC_BLOB__v1:<xferId>:<seq>:<total>:<rawLen>:<crc32>:<b64>
//	ack:   __AHVRC_BLOBACK__v1:<xferId>:<seq>:<status>
//
// Frames travel guest→host here (the renderer's BigClipboardReceiver reassembles
// them); acks travel host→guest and are routed to blobTx.onAck. Frames/acks
// never touch a real clipboard on either side.

package vdagent

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Guest copies at or below this go on the zero-overhead direct path; larger
	// ones are chunked. Deliberately NOT symmetric with the renderer's
	// DIRECT_MAX_BYTES: that one is small because host→guest is bound by this
	// guest's serial receive capacity, whereas guest→host has carried 13 KB in
	// the field and is bound only by QEMU dropping a PROVIDE that DEFLATES past
	// 1 MiB. The two directions have genuinely different ceilings.
	reverseBlobThreshold = 64 * 1024
	// Compressed bytes per frame. base64 ×4/3 → ~5.5 KB of frame text. Guest→host
	// is NOT bound by the ~4 KB host→guest wall (it has carried 13 KB in the
	// field), but stay well within that proven band rather than the old 341 KB,
	// which was never exercised guest→host. The per-frame ACK rides host→guest and
	// is tiny, so it clears the 4 KB wall regardless.
	reverseChunkBytes = 4 * 1024
	// ~5× the drain time of one frame on the emulated UART — tolerant of
	// keep-warm interleaving without stalling a real failure for long.
	reverseAckTimeout = 10 * time.Second
	reverseMaxRetries = 4
	// A reconnect can reset the host receiver mid-transfer; it then asks us to
	// rewind to seq 1 (err:restart). Bound how many times we honour that so a
	// receiver stuck resetting can't loop us forever.
	maxReverseRestarts = 3
)

// blobTx is the reverse sender's shared state: which transfer is currently
// authoritative (activeID) and the single frame awaiting an ACK. A new guest
// copy supersedes by swapping activeID; the running goroutine notices on its
// next arm() and exits. Guarded by its own mutex — independent of Agent.mu.
type blobTx struct {
	mu       sync.Mutex
	activeID string
	waiter   *txWaiter
	// chunkBytes overrides reverseChunkBytes when > 0 — test hook to force a
	// multi-frame transfer without megabytes of incompressible input.
	chunkBytes int
}

type txWaiter struct {
	xferID string
	seq    int
	ch     chan string // buffered (cap 1); onAck never blocks
}

// (The waiter is armed by Agent.armFrame, which also stamps the frame into the
// grab cache atomically under this mutex; see agent.go.)

// onAck routes a host→guest ack to the frame awaiting it. Runs on the read
// loop. Unknown/stale acks (a superseded transfer, a late retransmit) are
// dropped — they never match the current waiter.
func (tx *blobTx) onAck(ackText string) {
	rest := strings.TrimPrefix(ackText, blobAckPrefix)
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) < 3 {
		return
	}
	seq, err := strconv.Atoi(parts[1])
	if err != nil {
		return
	}
	xferID, status := parts[0], parts[2]
	tx.mu.Lock()
	w := tx.waiter
	if w != nil && w.xferID == xferID && w.seq == seq {
		tx.waiter = nil
		select {
		case w.ch <- status:
		default:
		}
	}
	tx.mu.Unlock()
}

// cancel supersedes the active transfer: the running goroutine stops at its
// next arm(), and any frame currently blocked on an ACK is released with a
// "cancelled" status so it returns promptly.
func (tx *blobTx) cancel() {
	tx.mu.Lock()
	tx.activeID = ""
	if tx.waiter != nil {
		select {
		case tx.waiter.ch <- "cancelled":
		default:
		}
		tx.waiter = nil
	}
	tx.mu.Unlock()
}

// randXferID returns 8 hex chars of CSPRNG — the transfer id that scopes acks
// to this send (and lets a sibling console's traffic be ignored).
func randXferID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// startReverseBlob kicks off a guest→host chunked transfer of text on a
// background goroutine, superseding any transfer already in flight. The
// goroutine owns (and wipes) its own copy of text.
func (a *Agent) startReverseBlob(ctx context.Context, text []byte) {
	id, err := randXferID()
	if err != nil {
		a.log.Warn("vdagent: reverse blob rng failed, large guest copy dropped", "err", err)
		return
	}
	payload := append([]byte(nil), text...)
	a.blobTx.mu.Lock()
	a.blobTx.activeID = id
	a.blobTx.waiter = nil
	a.blobTx.mu.Unlock()
	go a.runReverseBlob(ctx, id, payload)
}

// runReverseBlob gzips text, then sends it frame-by-frame stop-and-wait. On any
// return it releases its own authority and wipes any unconsumed frame — but only
// if it is STILL the active transfer (releaseTransfer), so it never disturbs a
// newer one that superseded it. The compressed buffer (gzip of operator
// clipboard data) is wiped too — it must not linger on the heap.
func (a *Agent) runReverseBlob(ctx context.Context, xferID string, text []byte) {
	defer wipeBytes(text)
	defer a.releaseTransfer(xferID)

	rawLen := len(text)
	compressed, err := gzipBytes(text)
	if err != nil {
		a.log.Warn("vdagent: reverse blob gzip failed", "err", err)
		return
	}
	defer wipeBytes(compressed)

	chunkSize := reverseChunkBytes
	a.blobTx.mu.Lock()
	if a.blobTx.chunkBytes > 0 {
		chunkSize = a.blobTx.chunkBytes
	}
	a.blobTx.mu.Unlock()
	total := (len(compressed) + chunkSize - 1) / chunkSize
	if total < 1 {
		total = 1
	}

	a.mu.Lock()
	useSelection := a.useSelection
	a.mu.Unlock()

	restarts := 0
	for seq := 1; seq <= total; seq++ {
		start := (seq - 1) * chunkSize
		end := start + chunkSize
		if end > len(compressed) {
			end = len(compressed)
		}
		b64 := base64.StdEncoding.EncodeToString(compressed[start:end])
		frame := fmt.Sprintf("%s%s:%d:%d:%d:%s:%s",
			blobFramePrefix, xferID, seq, total, rawLen, crc32Hex([]byte(b64)), b64)

		status := "timeout"
		for attempt := 0; attempt <= reverseMaxRetries; attempt++ {
			// armFrame both claims the ack waiter AND caches the frame atomically,
			// so a supersede can't slip in between and clobber a fresh copy.
			ch := a.armFrame(xferID, seq, []byte(frame))
			if ch == nil {
				return // superseded by a newer copy
			}
			grabData := BuildClipboardGrab(SelectionClipboard, []uint32{ClipboardUTF8Text}, useSelection)
			grabMsg := EncodeChunkedMessage(PortServer, MsgClipboardGrab, grabData)
			if _, werr := a.writeSerial(ctx, grabMsg); werr != nil {
				a.log.Warn("vdagent: reverse blob grab write failed", "err", werr)
				return
			}
			select {
			case status = <-ch:
			case <-time.After(reverseAckTimeout):
				status = "timeout"
			case <-ctx.Done():
				return
			}
			// Retryable: a lost frame/ack (timeout) or corruption in transit
			// (err:crc). Every other status is terminal for this frame.
			if status != "timeout" && status != "err:crc" {
				break
			}
		}

		// The host receiver reset mid-transfer (a reconnect) and can't reassemble
		// from here — rewind to the first frame. Bounded so a stuck receiver can't
		// loop us forever.
		if status == "err:restart" {
			restarts++
			if restarts > maxReverseRestarts {
				a.log.Warn("vdagent: reverse blob abandoned after repeated restarts", "restarts", restarts)
				return
			}
			a.log.Info("vdagent: reverse blob rewinding to seq 1 (host receiver reset)")
			seq = 0 // the loop's seq++ makes the next iteration seq 1
			continue
		}

		want := "ok"
		if seq == total {
			want = "done"
		}
		if status != want {
			if status != "cancelled" {
				a.log.Warn("vdagent: reverse blob transfer failed", "seq", seq, "total", total, "status", status)
			}
			return
		}
	}
	a.log.Info("vdagent: reverse blob transfer complete", "bytes", rawLen, "frames", total)
}
