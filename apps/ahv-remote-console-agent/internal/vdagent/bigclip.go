// Big-Clipboard Protocol (BCP) — host→guest receiver.
//
// A single large clipboard message cannot survive the path. Two distinct limits
// shape this; do not conflate them:
//
//  1. QEMU's caps, which we cannot change but which rarely bite: the VNC server
//     DISCONNECTS the client for any ClientCutText over 1 MiB (ui/vnc.c
//     "exceeds our limit of 1MB"), and qemu-vdagent's outbuf toward the guest
//     serial drops whole messages when its 1 MiB buffer is full.
//
//  2. THIS guest's serial receive capacity, which is what actually binds — and
//     is ours to fix. Host→guest was capped near 4 KB for months (a hard wall at
//     4096 across 11.5k real transfers). Not a QEMU limit: we never called
//     SetupComm, so the Windows driver's receive queue sat at its ~4 KB default;
//     Linux's line discipline buffers 4096; neither has flow control here; and
//     the tty was read on the same goroutine that blocks for seconds writing the
//     guest clipboard. See serial_windows.go and serial_drain.go.
//
// So the host sizes frames to the capacity this guest REPORTS it can absorb
// (hello.go — measured, never declared) and splits gzip(text) into base64 chunks,
// each an ordinary clipboard text frame, ACK-paced. Stop-and-wait is REQUIRED,
// not an optimization: QEMU's clipboard is last-writer-wins (no queue), so a
// second in-flight frame would clobber the first before the guest requested it.
//
// Wire grammar (ASCII, single line; keep in lockstep with
// apps/desktop/src/renderer/clipboard/BigClipboard.ts):
//
//	frame: __AHVRC_BLOB__v1:<xferId>:<seq>:<total>:<rawLen>:<crc32>:<b64>
//	ack:   __AHVRC_BLOBACK__v1:<xferId>:<seq>:<status>
//
// seq is 1-based; rawLen is the UNCOMPRESSED UTF-8 byte length of the final
// text; crc32 is IEEE, lowercase hex, over the b64 field's bytes; b64 is a
// standalone base64 (std alphabet) of one slice of the gzip stream. status is
// ok | done | err:crc | err:too_large | err:decode | err:write — err:crc is
// retryable (the sender retransmits that seq), the other errors are fatal to
// the transfer. Like every __AHVRC_ side-channel token, frames and acks are
// NEVER written to a real clipboard on either side.

package vdagent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	blobFramePrefix = "__AHVRC_BLOB__v1:"
	blobAckPrefix   = "__AHVRC_BLOBACK__v1:"
)

// blobRx is the receiver state machine. It is PURE (no Agent, no I/O) so the
// protocol is unit-testable without serial/clipboard fakes; Agent adapts it.
// Accessed only from the read loop goroutine — no locking needed.
type blobRx struct {
	xferID string
	total  int
	rawLen int
	next   int    // next expected 1-based seq
	buf    []byte // accumulated compressed bytes
	// Last completed transfer: a late duplicate of its final frame (our done
	// ack was lost, the sender retransmitted) must be re-acked done, not
	// misread as the start of a new transfer.
	doneID string
}

// blobIngest is the outcome of one frame: Ack to send back ("" = drop the
// frame silently — malformed or out-of-order; the sender's retry recovers),
// and, when the transfer completed, the decoded final text.
type blobIngest struct {
	Ack      string
	Text     string
	Complete bool
}

func blobAck(xferID string, seq int, status string) string {
	return blobAckPrefix + xferID + ":" + strconv.Itoa(seq) + ":" + status
}

func crc32Hex(b []byte) string {
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE(b))
}

// ingest advances the state machine by one frame (without the prefix).
// maxClip bounds the DECOMPRESSED payload (MaxClipboardSize at the caller).
func (st *blobRx) ingest(body string, maxClip int) blobIngest {
	parts := strings.SplitN(body, ":", 6)
	if len(parts) != 6 {
		return blobIngest{}
	}
	xferID := parts[0]
	seq, err1 := strconv.Atoi(parts[1])
	total, err2 := strconv.Atoi(parts[2])
	rawLen, err3 := strconv.Atoi(parts[3])
	wantCRC, b64 := parts[4], parts[5]
	if err1 != nil || err2 != nil || err3 != nil ||
		xferID == "" || seq < 1 || total < 1 || seq > total || rawLen < 1 {
		return blobIngest{}
	}

	// Late duplicate of a completed transfer's frame: the done ack was lost.
	if xferID == st.doneID {
		return blobIngest{Ack: blobAck(xferID, seq, "done")}
	}
	if rawLen > maxClip {
		return blobIngest{Ack: blobAck(xferID, seq, "err:too_large")}
	}
	// New transfer supersedes any in-progress one (latest copy wins — same
	// semantics as the clipboard itself).
	if xferID != st.xferID {
		*st = blobRx{xferID: xferID, total: total, rawLen: rawLen, next: 1, doneID: st.doneID}
	}
	if total != st.total || rawLen != st.rawLen {
		// Header drifted mid-transfer — unrecoverable; drop the state and let
		// the sender's timeout surface the failure.
		st.xferID = ""
		return blobIngest{}
	}
	if seq == st.next-1 {
		// Duplicate of the frame we already consumed: our ok ack was lost.
		return blobIngest{Ack: blobAck(xferID, seq, "ok")}
	}
	if seq != st.next {
		// Out-of-order under stop-and-wait = retransmit skew; sender retries.
		return blobIngest{}
	}
	if crc32Hex([]byte(b64)) != wantCRC {
		return blobIngest{Ack: blobAck(xferID, seq, "err:crc")}
	}
	chunk, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		// Corrupt in transit exactly like a crc miss — retryable.
		return blobIngest{Ack: blobAck(xferID, seq, "err:crc")}
	}
	// gzip of text is smaller than the text; allow slack for incompressible
	// payloads, but never let the COMPRESSED accumulation outgrow the cap.
	if len(st.buf)+len(chunk) > maxClip+64*1024 {
		st.xferID = ""
		return blobIngest{Ack: blobAck(xferID, seq, "err:too_large")}
	}
	st.buf = append(st.buf, chunk...)
	st.next++
	if seq < total {
		return blobIngest{Ack: blobAck(xferID, seq, "ok")}
	}

	// Final frame: decompress and hand the text to the adapter. State is
	// consumed either way; only a successful decode records doneID (a failed
	// transfer retried by the sender must not be re-acked "done").
	text, derr := gunzipText(st.buf, st.rawLen)
	st.xferID, st.buf = "", nil
	if derr != nil {
		return blobIngest{Ack: blobAck(xferID, seq, "err:decode")}
	}
	st.doneID = xferID
	return blobIngest{Ack: blobAck(xferID, seq, "done"), Text: text, Complete: true}
}

// gunzipText inflates the accumulated stream, bounded by the declared length
// (LimitReader = zip-bomb guard: one byte past rawLen is an error, and gzip's
// own trailing CRC32 covers payload integrity end-to-end).
func gunzipText(compressed []byte, rawLen int) (string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return "", err
	}
	defer zr.Close() //nolint:errcheck // read errors surface below
	out, err := io.ReadAll(io.LimitReader(zr, int64(rawLen)+1))
	if err != nil {
		return "", err
	}
	if len(out) != rawLen {
		return "", fmt.Errorf("blob length mismatch: got %d want %d", len(out), rawLen)
	}
	return string(out), nil
}

// handleBlobFrame is the Agent adapter: run the pure state machine, write the
// completed text to the guest clipboard (same sanitize+retry+bookkeeping path
// as a direct paste), and ack via the same grab mechanism as the keep-warm
// ack. A write failure downgrades the done ack to err:write so the host never
// audits a paste that did not land.
func (a *Agent) handleBlobFrame(ctx context.Context, frame string) error {
	res := a.blob.ingest(strings.TrimPrefix(frame, blobFramePrefix), MaxClipboardSize)
	if res.Complete {
		if err := a.sanitizeAndWriteClipboard(ctx, res.Text); err != nil {
			a.log.Warn("vdagent: blob transfer write failed", "err", err)
			// Recompute the ack: same xferId:seq, fatal write status.
			parts := strings.SplitN(strings.TrimPrefix(res.Ack, blobAckPrefix), ":", 3)
			res.Ack = blobAckPrefix + parts[0] + ":" + parts[1] + ":err:write"
		} else {
			a.log.Info("vdagent: blob transfer complete", "bytes", len(res.Text))
		}
	}
	if res.Ack == "" {
		return nil
	}
	return a.sendBlobAck(ctx, res.Ack)
}

// sendBlobAck sends one ack back to the host. When deferred behind a real guest
// copy, the sender's retransmit re-acks.
func (a *Agent) sendBlobAck(ctx context.Context, ack string) error {
	return a.sendSideChannel(ctx, ack)
}

// sendSideChannel sends one __AHVRC_ protocol token as an ordinary guest→host
// clipboard transfer (grab now; the host's CLIPBOARD_REQUEST is served from the
// grab cache) — identical mechanics to handleKeepwarmProbe, including the
// real-guest-copy deferral: clobbering a pending operator copy to send a
// protocol token would silently lose their data.
func (a *Agent) sendSideChannel(ctx context.Context, text string) error {
	a.mu.Lock()
	useSelection := a.useSelection
	// Defer ONLY for data a CLIPBOARD_REQUEST must still deliver intact: a real
	// operator copy (grabCacheProtocol==false) or an in-flight reverse-BCP frame
	// (grabCacheOwner!=""). Our own transient side-channel acks (protocol==true &&
	// owner=="": a prior blob ack, keep-warm ack, or hello ack) carry no operator
	// data and are regenerated by the host's retransmit / the proxy's re-probe —
	// deferring on them stalled every large host->guest paste for grabCacheTTL.
	// Decide AND stamp under ONE a.mu hold so a concurrent reverse armFrame cannot
	// slip a frame into the slot between the check and the overwrite and be lost.
	mustDefer := a.grabCache != nil && time.Since(a.grabCacheTime) <= grabCacheTTL &&
		(!a.grabCacheProtocol || a.grabCacheOwner != "")
	if mustDefer {
		a.mu.Unlock()
		a.log.Info("vdagent: side-channel send deferred — guest copy in flight")
		return nil
	}
	a.setGrabCacheLocked([]byte(text), true)
	a.mu.Unlock()
	grabData := BuildClipboardGrab(SelectionClipboard, []uint32{ClipboardUTF8Text}, useSelection)
	grabMsg := EncodeChunkedMessage(PortServer, MsgClipboardGrab, grabData)
	if _, err := a.writeSerial(ctx, grabMsg); err != nil {
		a.wipeGrabCache()
		return err
	}
	return nil
}
