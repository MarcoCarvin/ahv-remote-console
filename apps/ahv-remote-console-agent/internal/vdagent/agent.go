package vdagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/ahv-remote-console-agent/internal/keepwarm"
)

// frameStallTimeout bounds how long readFrameBytes will wait for the REST of
// a partially-read chunk before declaring the frame lost. A serial read
// timeout returning zero bytes BETWEEN frames is normal (idle) and waited on
// indefinitely; the same timeout MID-frame for this long means the backend
// stopped sending (e.g. the chardev was swapped on a guest reboot), so we
// abort and let readLoop reset reassembly and recover. At 115200 baud a real
// frame never pauses this long mid-transfer.
const frameStallTimeout = 3 * time.Second

// ClipboardHandler is the interface the agent uses to read/write the
// guest OS clipboard.
type ClipboardHandler interface {
	ReadText(ctx context.Context) ([]byte, error)
	WriteText(ctx context.Context, text []byte) error
	Watch(ctx context.Context) <-chan struct{}
}

// DisplayResizer is the interface the agent uses to apply a display
// resolution change requested via the clipboard side-channel.
type DisplayResizer interface {
	Resize(width, height int) error
}

// Keep-warm side-channel tokens. Like __AHVRC_RESIZE__, these are magic
// proxy-originated commands intercepted BEFORE any guest clipboard write —
// they are never operator clipboard data and never reach the guest clipboard.
const (
	// keepwarmPrefix heads a lease command "__AHVRC_KEEPWARM__<ttl_s>":
	// ttl>0 arms/extends the display keep-warm lease, ttl=0 disarms it.
	keepwarmPrefix = "__AHVRC_KEEPWARM__" // #nosec G101 -- vdagent keep-warm protocol marker, not a credential
	// keepwarmProbeText is the proxy's capability probe. It lives inside the
	// RESIZE namespace on purpose: a STALE agent routes it to
	// handleResizeCommand, which swallows an unparseable payload without ever
	// touching the guest clipboard — so a new proxy probing an old agent
	// pollutes nothing. A NEW agent answers with keepwarmAckText.
	keepwarmProbeText = "__AHVRC_RESIZE__keepwarm-probe" // #nosec G101 -- vdagent keep-warm protocol marker, not a credential
	// keepwarmAckText is the guest->host ack to the probe, sent as a normal
	// vdagent clipboard grab/data round-trip; the (new) proxy intercepts and
	// swallows it before it could reach the renderer or host clipboard.
	keepwarmAckText = "__AHVRC_KEEPWARM_ACK__" // #nosec G101 -- vdagent keep-warm protocol marker, not a credential
	// maxKeepwarmTTLSeconds bounds a lease so a buggy or malicious host
	// cannot arm hour-long background drawing; the proxy sends 45.
	maxKeepwarmTTLSeconds = 300
)

// keepwarmController is the lease surface the agent drives from the
// __AHVRC_KEEPWARM__ side-channel. *keepwarm.Lease implements it; tests
// inject a recorder.
type keepwarmController interface {
	Arm(ttl time.Duration)
	Disarm()
	// Stop disarms and blocks (bounded) until the lease loop has restored
	// the guest cursors — required on daemon shutdown, where process exit
	// would otherwise outrun the asynchronous Disarm.
	Stop()
}

// Agent implements a minimal SPICE agent that speaks the VDAgent
// protocol over a serial port. It handles clipboard sharing between
// the QEMU host and the guest OS.
//
// Edge cases handled:
//   - Bridge starts BEFORE hook fires (COM3 is socket-backed):
//     caps re-send loop runs every 5s. When hook fires chardev-change,
//     the backend swaps and the vdagent sends caps. The bridge's next
//     re-send triggers a fresh handshake.
//   - Bridge starts AFTER hook fires (COM3 is already vdagent):
//     initial caps exchange succeeds immediately if clipboard=true
//     was applied. If not (QEMU bug), re-send loop keeps trying.
//   - Install.cmd run minutes/hours after boot:
//     hook already fired. Bridge opens COM3 (vdagent backend).
//     Same as "after hook" case.
//   - chardev-change disrupts serial mid-read:
//     readChunk returns error, logged, retried after 100ms.
//     The re-send loop re-negotiates caps on the new backend.
//   - Multiple chardev-changes (operator power-cycles hook):
//     each change triggers serial_be_change → caps sent.
//     Bridge picks them up on next read cycle.
//   - Serial port doesn't exist (no acli serial_port_create):
//     bridge's OpenSerial fails → graceful disable.
//   - NGA holds the port (wrong COM port index):
//     OpenSerial gets "Access denied" → graceful disable.
//   - QEMU vdagent clipboard=true not applied (the known bug):
//     caps exchange shows clipboard_by_demand=false. Re-send
//     loop keeps trying. If never resolves, clipboard stays disabled.
//     No crash, no interference with other systems.
//   - Large clipboard data spanning multiple VDI chunks:
//     reassembly buffer accumulates until full message received.
//   - Guest clipboard read fails (clipboard locked by another app):
//     silently skipped, retried on next poll tick.
//   - Host sends clipboard while bridge is writing:
//     serial port is full-duplex. Concurrent read/write is safe.
//     Mutex protects shared state, not I/O.
type Agent struct {
	port      io.ReadWriteCloser
	clipboard ClipboardHandler
	display   DisplayResizer
	keepwarm  keepwarmController
	log       *slog.Logger

	mu                  sync.Mutex
	writeMu             sync.Mutex // serializes all port.Write calls
	hostCaps            []uint32
	useSelection        bool
	connected           bool
	clipboardEnabled    bool
	lastClipHash        string
	lastClipTime        time.Time
	lastClipEchoPending bool

	// Serial receive ring (serial_drain.go). The drain goroutine is the ONLY
	// reader of a.port; readFrameBytes consumes these instead, so a multi-second
	// clipboard write can never stall the tty into dropping bytes.
	drainCh   chan []byte
	drainLeft []byte // consumer-only remainder of the last ring entry
	drainErr  atomic.Pointer[error]
	// lastOverruns is the last reported serial drop count. Touched only by the
	// caps heartbeat goroutine, so it needs no lock.
	lastOverruns uint64

	// Reassembly buffer for multi-chunk messages.
	reassembly          []byte
	reassemblyRemaining uint32
	reassemblyDeadline  time.Time

	// Big-Clipboard Protocol receiver (bigclip.go, host→guest). Read-loop-only
	// state, so unlocked — same discipline as the reassembly fields above.
	blob blobRx

	// Big-Clipboard Protocol sender (bigclip_tx.go, guest→host reverse). Runs a
	// background goroutine per large guest copy; acks arrive on the read loop.
	blobTx blobTx

	// Pending guest->host transfer: the clipboard text captured when the
	// GRAB was sent, served to the host's CLIPBOARD_REQUEST without a
	// second OS clipboard read (which can stall ~2s under Windows
	// OpenClipboard contention or transiently read empty, losing the
	// copy). Single round-trip lifetime: wiped on use, on every new watch
	// event, on read-loop recovery, and after grabCacheTTL.
	grabCache     []byte
	grabCacheTime time.Time
	// grabCacheProtocol marks the cached text as protocol side-channel (a BCP
	// frame/ack or keep-warm ack) rather than operator clipboard data. Protocol
	// entries are exempt from pollClipboard's stale-on-change wipe: the clipboard
	// change that races them is our OWN host→guest write echoing back, and wiping
	// would strand the ack the host is about to REQUEST (the final-frame stall).
	grabCacheProtocol bool
	// grabCacheOwner stamps a reverse-BCP frame with its transfer id (empty for
	// operator/keep-warm entries) so a finishing or superseded reverse goroutine
	// only ever wipes its OWN residual frame — never a newer transfer's frame or
	// a fresh operator copy that took the slot in between (see armFrame/releaseTransfer).
	grabCacheOwner string
	// pollDedupResetPending: readLoop sets this on a chardev swap so pollClipboard
	// clears its lastText dedup key. Without it, after a reconnect the operator's
	// re-copy of the SAME text is deduped against a pre-swap lastText and never
	// re-sent — a stale-latch, since the reconnect reset QEMU's clipboard so the
	// host no longer holds that text.
	pollDedupResetPending bool
}

func New(port io.ReadWriteCloser, clipboard ClipboardHandler, log *slog.Logger) *Agent {
	return &Agent{
		port:      port,
		clipboard: clipboard,
		keepwarm:  keepwarm.New(log),
		log:       log,
	}
}

// SetDisplay attaches a display resizer. When non-nil, the agent
// handles resize commands received via the clipboard side-channel
// (__AHVRC_RESIZE__ magic prefix).
func (a *Agent) SetDisplay(d DisplayResizer) {
	a.display = d
}

// Run starts the agent. Blocks until ctx is cancelled or a fatal
// error occurs. All transient errors (read timeouts, partial chunks,
// chardev-change disruptions) are retried automatically.
func (a *Agent) Run(ctx context.Context) error {
	a.log.Info("vdagent: starting on serial port", "rx_capacity", a.rxCapacity())

	// Drain goroutine FIRST: from here on it is the only reader of the port, so
	// nothing the agent does downstream (notably a multi-second clipboard write)
	// can stall the tty into silently dropping inbound bytes.
	a.startDrain(ctx)

	// Initial caps send — may go to socket backend (lost) or to
	// vdagent backend (triggers handshake). Either way, the
	// periodic re-send below handles all timing scenarios.
	a.safeSendCaps(ctx, true)

	// Permanent caps heartbeat: fast 5s retry until clipboard is first
	// negotiated, then a slow lifetime heartbeat. Covers bridge-before-hook,
	// hook restart, chardev-change disruption, Install.cmd after boot, and —
	// critically for the console-following design — a FRESH vdagent peer that
	// (re)appears after a console-open backend swap or a live migration: the
	// destination re-attaches a new vdagent peer, and the heartbeat re-answers
	// it within capsHeartbeatInterval, re-establishing clipboard without a
	// restart.
	go a.capsRetryLoop(ctx, capsFastRetryInterval, capsHeartbeatInterval)

	// Clipboard poll — only active after clipboardEnabled=true.
	go a.pollClipboard(ctx)

	// Main read loop — reads VDI chunks from the serial port.
	// Handles: chardev-change disruption (read error → retry),
	// partial chunks (reassembly), unknown messages (ignored).
	err := a.readLoop(ctx)
	// Shutdown: stop any live keep-warm lease with the daemon and WAIT for
	// the cursor restore to land — Disarm alone is asynchronous and the
	// process would exit before the op loop's next tick, leaving the guest
	// pointer invisible until the next daemon start.
	a.keepwarm.Stop()
	return err
}

// Caps cadence. fast is used for quick initial bring-up until clipboard is
// first negotiated; heartbeat is the permanent lifetime cadence afterward so a
// fresh vdagent peer (after a backend swap or live migration) is always
// answered within the interval. Re-sending caps to an already-active peer is a
// benign idempotent re-negotiation, so the loop always heartbeats rather than
// trying to detect a peer change.
const (
	capsFastRetryInterval = 5 * time.Second
	capsHeartbeatInterval = 15 * time.Second
)

// capsRetryLoop sends ANNOUNCE_CAPABILITIES on a timer for the agent's
// lifetime. It ticks at fast until clipboard is first negotiated (quick
// bring-up across all timing scenarios), then slows to heartbeat and keeps
// going forever: each beat re-requests the host's caps, so a fresh peer that
// appeared after a chardev swap or a destination re-attach is re-negotiated
// without a restart. Stops only on ctx cancellation.
func (a *Agent) capsRetryLoop(ctx context.Context, fast, heartbeat time.Duration) {
	ticker := time.NewTicker(fast)
	defer ticker.Stop()
	slowed := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.mu.Lock()
			enabled := a.clipboardEnabled
			a.mu.Unlock()
			if enabled && !slowed {
				// First negotiation complete: drop to the lifetime heartbeat
				// cadence but keep sending — never stop.
				ticker.Reset(heartbeat)
				slowed = true
			}
			if enabled {
				a.log.Debug("vdagent: caps heartbeat (re-answering any fresh peer)")
			} else {
				a.log.Info("vdagent: re-sending caps (clipboard not yet negotiated)")
			}
			// Piggyback the drop check on the existing beat: cheap, periodic, and
			// it makes a receive overrun impossible to miss in the field.
			a.logSerialDrops()
			a.safeSendCaps(ctx, true)
		}
	}
}

func (a *Agent) readLoop(ctx context.Context) error {
	consecutiveErrors := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		chunk, err := a.readChunk()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			consecutiveErrors++
			if consecutiveErrors <= 3 {
				a.log.Warn("vdagent: read error", "err", err, "consecutive", consecutiveErrors)
			}
			// After chardev-change, the serial port may briefly
			// return errors. Back off progressively.
			backoff := time.Duration(consecutiveErrors) * 100 * time.Millisecond
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
			time.Sleep(backoff)
			// Reset reassembly state — partial data from old
			// backend is garbage after chardev-change.
			a.reassembly = nil
			a.reassemblyRemaining = 0
			a.reassemblyDeadline = time.Time{}
			// A pending REQUEST will never arrive across a backend
			// swap; drop the grab-time cleartext too.
			a.wipeGrabCache()
			// The swap reset QEMU's clipboard — clear pollClipboard's dedup key so
			// the operator's re-copy of the same text re-fires (RC-D recovery).
			a.mu.Lock()
			a.pollDedupResetPending = true
			a.mu.Unlock()
			continue
		}
		consecutiveErrors = 0

		if chunk.Port != PortClient {
			continue
		}

		if err := a.handleChunkData(ctx, chunk.Data); err != nil {
			a.log.Warn("vdagent: handle error", "err", err)
		}
	}
}

type chunkWithData struct {
	Port uint32
	Data []byte
}

func (a *Agent) readChunk() (chunkWithData, error) {
	hdr := make([]byte, ChunkHeaderSize)
	if err := a.readFrameBytes(hdr); err != nil {
		return chunkWithData{}, fmt.Errorf("read chunk header: %w", err)
	}

	port := binary.LittleEndian.Uint32(hdr[0:4])
	size := binary.LittleEndian.Uint32(hdr[4:8])

	// Sanity bound only. QEMU chunks host→guest at MaxChunkSize unconditionally,
	// so this cannot fire on a well-formed stream — but the old bound was 4096,
	// sitting exactly on the measured host→guest wall, which made it look like
	// the cause. It is not. Bound generously and LOG: draining here was the wrong
	// response to a desync anyway, since it discards a valid stream's tail.
	// readLoop's backoff + reassembly reset is the real recovery.
	if size > MaxChunkSize*64 {
		return chunkWithData{}, fmt.Errorf("chunk size implausible: %d (port=0x%x)", size, port)
	}
	if size == 0 {
		return chunkWithData{Port: port}, nil
	}

	data := make([]byte, size)
	if err := a.readFrameBytes(data); err != nil {
		return chunkWithData{}, fmt.Errorf("read chunk data: %w", err)
	}

	return chunkWithData{Port: port, Data: data}, nil
}

const reassemblyTimeout = 10 * time.Second

func (a *Agent) handleChunkData(ctx context.Context, data []byte) error {
	// Body-reassembly state: we already parsed a full message header in a prior
	// chunk and know exactly how many body bytes are still outstanding.
	if a.reassemblyRemaining > 0 {
		if !a.reassemblyDeadline.IsZero() && time.Now().After(a.reassemblyDeadline) {
			held := len(a.reassembly)
			a.reassembly = nil
			a.reassemblyRemaining = 0
			a.reassemblyDeadline = time.Time{}
			a.log.Warn("vdagent: reassembly timed out, dropping partial message", "held_bytes", held)
			return nil
		}
		a.reassembly = append(a.reassembly, data...)
		chunkLen := uint32(len(data)) // #nosec G115 -- len(data) is one serial read (KB-scale), fits uint32
		if chunkLen >= a.reassemblyRemaining {
			a.reassemblyRemaining = 0
			a.reassemblyDeadline = time.Time{}
			msg := a.reassembly
			a.reassembly = nil
			return a.handleMessage(ctx, msg)
		}
		a.reassemblyRemaining -= chunkLen
		// Progress re-arms the deadline: it bounds GAPS between chunks, not
		// total message time, or any paste larger than rate x timeout would
		// be deterministically dropped on a slow serial link. Dead streams
		// stay bounded by frameStallTimeout and MaxMessageSize.
		if len(data) > 0 {
			a.reassemblyDeadline = time.Now().Add(reassemblyTimeout)
		}
		return nil
	}

	// PROT-01: header-straddle state. A prior chunk ended with fewer than
	// MessageHeaderSize bytes left over, so we could not yet read msgSize.
	// Those bytes were stashed in a.reassembly (with reassemblyRemaining==0);
	// prepend them here and re-evaluate once enough bytes have arrived. This
	// avoids losing the partial header to "chunk too small for message header".
	if len(a.reassembly) > 0 {
		if !a.reassemblyDeadline.IsZero() && time.Now().After(a.reassemblyDeadline) {
			held := len(a.reassembly)
			a.reassembly = nil
			a.reassemblyDeadline = time.Time{}
			a.log.Warn("vdagent: reassembly timed out, dropping partial header", "held_bytes", held)
			return nil
		}
		data = append(a.reassembly, data...)
		a.reassembly = nil
		a.reassemblyDeadline = time.Time{}
	}

	if len(data) < MessageHeaderSize {
		// Trailing remainder shorter than a message header: the header itself
		// straddles the chunk boundary. Stash it and wait for the next chunk(s)
		// instead of erroring and dropping the bytes.
		a.reassembly = make([]byte, len(data))
		copy(a.reassembly, data)
		a.reassemblyDeadline = time.Now().Add(reassemblyTimeout)
		return nil
	}

	msgSize := binary.LittleEndian.Uint32(data[16:20])
	if msgSize > MaxMessageSize {
		return fmt.Errorf("message too large: %d bytes", msgSize)
	}
	totalNeeded := MessageHeaderSize + int(msgSize)

	if len(data) < totalNeeded {
		a.reassembly = make([]byte, len(data))
		copy(a.reassembly, data)
		a.reassemblyRemaining = uint32(totalNeeded - len(data)) // #nosec G115 -- totalNeeded <= MaxMessageSize (guarded above) and > len(data), fits uint32
		a.reassemblyDeadline = time.Now().Add(reassemblyTimeout)
		return nil
	}

	if err := a.handleMessage(ctx, data[:totalNeeded]); err != nil {
		return err
	}

	// A single VDI chunk may carry more than one message.
	// Process any remaining bytes as the start of the next message.
	if remaining := data[totalNeeded:]; len(remaining) > 0 {
		return a.handleChunkData(ctx, remaining)
	}
	return nil
}

func (a *Agent) handleMessage(ctx context.Context, data []byte) error {
	if len(data) < MessageHeaderSize {
		return fmt.Errorf("message too short: %d", len(data))
	}

	protocol := binary.LittleEndian.Uint32(data[0:4])
	if protocol != ProtocolVersion {
		return fmt.Errorf("unexpected protocol version: %d", protocol)
	}

	msgType := binary.LittleEndian.Uint32(data[4:8])
	payload := data[MessageHeaderSize:]

	// SECURITY: never log message payload bytes. A MsgClipboard payload is the
	// full host->guest clipboard text (e.g. a pasted admin password); dumping it
	// to a guest-readable log leaks operator secrets. Log only non-sensitive
	// metadata (message type + payload length).
	a.log.Info("vdagent: received message",
		"type", msgType,
		"payload_len", len(payload),
	)

	switch msgType {
	case MsgAnnounceCapabilities:
		return a.handleCapabilities(ctx, payload)
	case MsgClipboardGrab:
		return a.handleClipboardGrab(ctx, payload)
	case MsgClipboardRequest:
		return a.handleClipboardRequest(ctx, payload)
	case MsgClipboard:
		return a.handleClipboardData(ctx, payload)
	case MsgClipboardRelease:
		a.log.Debug("vdagent: host released clipboard")
		return nil
	default:
		a.log.Debug("vdagent: ignoring message", "type", msgType)
		return nil
	}
}

func (a *Agent) handleCapabilities(ctx context.Context, payload []byte) error {
	if len(payload) < 4 {
		return fmt.Errorf("caps payload too short")
	}
	request := binary.LittleEndian.Uint32(payload[0:4])

	nWords := (len(payload) - 4) / 4
	a.mu.Lock()
	a.hostCaps = make([]uint32, nWords)
	for i := 0; i < nWords; i++ {
		a.hostCaps[i] = binary.LittleEndian.Uint32(payload[4+i*4 : 4+i*4+4])
	}
	a.useSelection = a.hasHostCap(CapClipboardSelection)
	a.clipboardEnabled = a.hasHostCap(CapClipboardByDemand)
	a.connected = true
	a.mu.Unlock()

	// Log decoded capability flags + counts only; do not dump raw payload bytes.
	a.log.Info("vdagent: received host capabilities",
		"request", request != 0,
		"words", nWords,
		"clipboard_by_demand", a.hasHostCap(CapClipboardByDemand),
		"clipboard_selection", a.hasHostCap(CapClipboardSelection),
		"clipboard_grab_serial", a.hasHostCap(CapClipboardGrabSerial),
	)

	if a.clipboardEnabled {
		a.log.Info("vdagent: clipboard negotiated successfully")
	}

	// Always respond with our caps so the host knows what we support.
	// Use request=true if clipboard isn't enabled yet — this triggers
	// the host to re-send caps (which may include clipboard after
	// processing our caps).
	return a.sendCapabilities(ctx, !a.clipboardEnabled)
}

func (a *Agent) hasHostCap(bit uint32) bool {
	wordIdx := bit / 32
	bitIdx := bit % 32
	if int(wordIdx) >= len(a.hostCaps) {
		return false
	}
	return a.hostCaps[wordIdx]&(1<<bitIdx) != 0
}

func (a *Agent) sendCapabilities(ctx context.Context, request bool) error {
	caps := BuildCapabilities(request,
		CapClipboardByDemand,
		CapClipboardSelection,
	)
	msg := EncodeChunkedMessage(PortServer, MsgAnnounceCapabilities, caps)
	a.log.Info("vdagent: sending capabilities",
		"request", request,
		"bytes", len(msg),
	)
	_, err := a.writeSerial(ctx, msg)
	return err
}

// serialWriteSlice caps each port.Write. On Windows the port uses a fixed
// WriteTotalTimeoutConstant of 1s with no per-byte multiplier, and a timed-out
// synchronous WriteFile reports SUCCESS with a partial count; small slices
// keep every WriteFile comfortably inside that budget at any drain rate
// >= 4KB/s. Harmless on Linux (os.File.Write already loops on short writes).
const serialWriteSlice = 4 * 1024

// maxOutboundClipboard caps a single guest->host clipboard transfer. At ISA
// serial rates anything larger would monopolize the link for minutes; refuse
// it loudly instead of jamming the stream.
const maxOutboundClipboard = 256 * 1024

// writeSerial writes data fully to the serial port in slices of at most
// serialWriteSlice bytes. The vdagent wire framing has no resync marker, so a
// short write (Windows WriteFile timeout) would permanently desync the host
// parser — short writes are therefore retried until every byte is delivered.
// Two consecutive zero-byte writes mean the port stopped draining: fail
// bounded rather than spin. A cancelled ctx or hard port error stops the loop.
func (a *Agent) writeSerial(ctx context.Context, data []byte) (int, error) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	written := 0
	zeroWrites := 0
	for written < len(data) {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		end := written + serialWriteSlice
		if end > len(data) {
			end = len(data)
		}
		n, err := a.port.Write(data[written:end])
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			zeroWrites++
			if zeroWrites >= 2 {
				return written, fmt.Errorf("serial write stalled after %d/%d bytes", written, len(data))
			}
			continue
		}
		zeroWrites = 0
	}
	return written, nil
}

// safeSendCaps sends capabilities, logging errors instead of returning them.
func (a *Agent) safeSendCaps(ctx context.Context, request bool) {
	if err := a.sendCapabilities(ctx, request); err != nil {
		a.log.Debug("vdagent: caps send failed (serial may not be ready)", "err", err)
	}
}

func (a *Agent) handleClipboardGrab(ctx context.Context, payload []byte) error {
	a.mu.Lock()
	useSelection := a.useSelection
	a.mu.Unlock()

	// CapClipboardGrabSerial (cap bit 17): QEMU 9.1+ advertises this in its host
	// caps UNCONDITIONALLY, but the grab serial is only placed on the wire when
	// BOTH peers negotiate the cap. We never advertise it (see sendCapabilities),
	// so QEMU sends LEGACY-format grabs — the selection prefix followed by the
	// uint32 type list, with NO serial (verified on AHV/QEMU 9.1: an 8-byte
	// [selection][type] payload). We therefore parse the legacy layout and must
	// NOT refuse merely because the host advertised the cap: an earlier guard did
	// exactly that, which silently broke host->guest paste because this inbound
	// GRAB is step 1 of that flow. The type loop below ignores any unrecognised
	// word, so even a stray serial (e.g. a future host that emitted one
	// unilaterally) could never trigger a bogus request.

	// PROT-06: when the selection prefix is active, echo the host's inbound
	// selection byte back on our reply rather than hard-coding
	// SelectionClipboard. The agent advertises CapClipboardSelection, so we
	// must round-trip whatever selection the host addressed.
	offset := 0
	selection := SelectionClipboard
	if useSelection {
		if len(payload) < 4 {
			return nil
		}
		selection = payload[0]
		offset = 4
	}

	var types []uint32
	for i := offset; i+4 <= len(payload); i += 4 {
		types = append(types, binary.LittleEndian.Uint32(payload[i:i+4]))
	}

	a.log.Info("vdagent: host grabbed clipboard", "types", types)

	for _, t := range types {
		if t == ClipboardUTF8Text {
			reqData := BuildClipboardRequest(selection, ClipboardUTF8Text, useSelection)
			msg := EncodeChunkedMessage(PortServer, MsgClipboardRequest, reqData)
			_, err := a.writeSerial(ctx, msg)
			return err
		}
	}
	return nil
}

func (a *Agent) handleClipboardRequest(ctx context.Context, payload []byte) error {
	a.mu.Lock()
	useSelection := a.useSelection
	a.mu.Unlock()

	// PROT-06: echo the host's inbound selection byte on the data reply.
	offset := 0
	selection := SelectionClipboard
	if useSelection {
		if len(payload) < 4 {
			return nil
		}
		selection = payload[0]
		offset = 4
	}
	if len(payload) < offset+4 {
		return nil
	}
	clipType := binary.LittleEndian.Uint32(payload[offset : offset+4])

	if clipType != ClipboardUTF8Text {
		a.log.Debug("vdagent: unsupported clipboard type requested", "type", clipType)
		return nil
	}

	// Serve the text captured when the GRAB was sent: re-reading the OS
	// clipboard here can stall ~2s under Windows OpenClipboard contention,
	// and a transient empty second read would silently lose the guest copy.
	// Fall back to a live read when no cache is pending (REQUEST retries).
	text := a.takeGrabCache()
	if len(text) == 0 {
		var err error
		text, err = a.clipboard.ReadText(ctx)
		if err != nil {
			a.log.Debug("vdagent: clipboard read failed", "err", err)
			return nil
		}
	}
	defer wipeBytes(text)
	if len(text) == 0 {
		return nil
	}
	// Reverse-BCP frames and protocol acks (all __AHVRC_ side-channel tokens)
	// carry the chunking that lets a large guest copy cross at all — each frame
	// is deliberately bigger than a single direct copy. They are never operator
	// data and are individually bounded by the sender's chunk size, so exempt
	// them from the direct-copy cap that guards a naive whole-clipboard PROVIDE.
	if !bytes.HasPrefix(text, []byte("__AHVRC_")) && len(text) > maxOutboundClipboard {
		a.log.Warn("vdagent: guest clipboard too large to send, dropped",
			"bytes", len(text), "max", maxOutboundClipboard)
		return nil
	}

	clipData := BuildClipboardData(selection, ClipboardUTF8Text, text, useSelection)
	msg := EncodeChunkedMessage(PortServer, MsgClipboard, clipData)
	a.log.Info("vdagent: sending clipboard data to host", "bytes", len(text))
	_, err := a.writeSerial(ctx, msg)
	return err
}

// sanitizeClipboardText strips characters that have no business in pasted text
// and are classic injection/spoofing vectors: C0 controls (NUL, ESC, ...), C1
// controls and DEL, Unicode bidi overrides, zero-width characters and the
// line/paragraph separators. Tab, newline and carriage return are preserved.
// Mirrors the renderer's sanitizeGuestText as defense-in-depth so the guest
// clipboard stays clean even if the host side is bypassed or buggy (audit SEC-01).
func sanitizeClipboardText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			// legitimate whitespace — keep (falls through to WriteRune below)
		case r < 0x20 || r == 0x7f: // C0 controls (NUL, ESC, ...) + DEL
			continue
		case r >= 0x80 && r <= 0x9f: // C1 controls (incl. NEL 0x85)
			continue
		case r == 0x200b || r == 0x200c || r == 0x200d || r == 0xfeff: // zero-width / BOM
			continue
		case r >= 0x202a && r <= 0x202e: // bidi embeddings / overrides
			continue
		case r >= 0x2066 && r <= 0x2069: // bidi isolates
			continue
		case r == 0x2028 || r == 0x2029: // line / paragraph separator
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (a *Agent) handleClipboardData(ctx context.Context, payload []byte) error {
	a.mu.Lock()
	useSelection := a.useSelection
	a.mu.Unlock()

	offset := 0
	if useSelection {
		if len(payload) < 4 {
			return nil
		}
		offset = 4
	}
	if len(payload) < offset+4 {
		return nil
	}

	clipType := binary.LittleEndian.Uint32(payload[offset : offset+4])
	text := payload[offset+4:]

	if clipType != ClipboardUTF8Text {
		a.log.Debug("vdagent: received non-text clipboard", "type", clipType)
		return nil
	}

	if len(text) > MaxClipboardSize {
		a.log.Warn("vdagent: clipboard too large, dropped", "bytes", len(text), "max", MaxClipboardSize)
		return nil
	}

	textStr := string(text)

	// Intercept resize side-channel commands before touching the
	// guest clipboard. The magic prefix is injected by the proxy
	// when the renderer sends a SetDesktopSize request.
	if strings.HasPrefix(textStr, "__AHVRC_RESIZE__") {
		// Record the token hash so a QEMU broadcast of it back onto the guest
		// clipboard is echo-suppressed. (Single-slot marker: a token arriving
		// mid-paste can clobber the paste's marker — a pre-existing MINOR;
		// separate slots are a follow-up.)
		a.mu.Lock()
		a.markSelfWriteLocked(hashText(textStr))
		a.mu.Unlock()
		if textStr == keepwarmProbeText {
			return a.handleKeepwarmProbe(ctx)
		}
		return a.handleResizeCommand(textStr)
	}

	// Intercept keep-warm lease commands the same way: side-channel only,
	// never written to the guest clipboard.
	if strings.HasPrefix(textStr, keepwarmPrefix) {
		a.mu.Lock()
		a.markSelfWriteLocked(hashText(textStr))
		a.mu.Unlock()
		return a.handleKeepwarmCommand(textStr)
	}

	// Big-Clipboard Protocol frames (bigclip.go): chunks of a large host→guest
	// paste. Side-channel only — the frame text never touches the guest
	// clipboard; the assembled payload is written on the final frame.
	if strings.HasPrefix(textStr, blobFramePrefix) {
		a.mu.Lock()
		a.markSelfWriteLocked(hashText(textStr))
		a.mu.Unlock()
		return a.handleBlobFrame(ctx, textStr)
	}

	// Reverse Big-Clipboard acks (bigclip_tx.go): the host acking a frame of a
	// large guest→host paste. Side-channel only — route it to the in-flight
	// sender and never toward the guest clipboard. (blobAckPrefix is a distinct
	// token from blobFramePrefix, so it never matches the frame branch above.)
	if strings.HasPrefix(textStr, blobAckPrefix) {
		a.mu.Lock()
		a.markSelfWriteLocked(hashText(textStr))
		a.mu.Unlock()
		a.blobTx.onAck(textStr)
		return nil
	}

	// Capability probe (hello.go): report this guest's MEASURED receive capacity
	// so the host can size frames to what we can actually absorb.
	if strings.HasPrefix(textStr, helloPrefix) {
		a.mu.Lock()
		a.markSelfWriteLocked(hashText(textStr))
		a.mu.Unlock()
		return a.handleHello(ctx)
	}

	// Version-skew guard: any OTHER __AHVRC_ token is side-channel traffic
	// from a newer host this agent doesn't understand (a future protocol
	// version). It is never operator data — dropping it beats writing frame
	// garbage into the guest clipboard.
	if strings.HasPrefix(textStr, "__AHVRC_") {
		a.mu.Lock()
		a.markSelfWriteLocked(hashText(textStr))
		a.mu.Unlock()
		a.log.Warn("vdagent: unrecognized side-channel token dropped", "bytes", len(textStr))
		return nil
	}

	a.log.Info("vdagent: received clipboard from host", "bytes", len(text))
	return a.sanitizeAndWriteClipboard(ctx, textStr)
}

// selfEchoBackstop bounds how long a recorded self-write marker can suppress a
// matching guest clipboard change. The marker is single-consume (see
// markSelfWriteLocked / pollClipboard), so this only matters when the OS
// coalesced our write and no echo ever arrives to consume it — long enough to
// cover any real host→guest write + read-back, short enough that a later
// genuine identical operator copy is not suppressed for long.
const selfEchoBackstop = 30 * time.Second

// markSelfWriteLocked records the canonical hash of content this agent just put
// on (or accepted for) the guest clipboard, so pollClipboard suppresses the OS
// change-event echo of our OWN write exactly once. Single-consume (+ backstop)
// replaces a fragile 3 s wall-clock window that let a slow large-paste echo
// escape suppression and loop back through reverse-BCP, wedging the channel.
// Caller holds a.mu.
func (a *Agent) markSelfWriteLocked(hash string) {
	a.lastClipHash = hash
	a.lastClipTime = time.Now()
	a.lastClipEchoPending = true
}

// sanitizeAndWriteClipboard is the SINGLE host→guest clipboard write path,
// shared by direct pastes and completed blob transfers (bigclip.go) so the
// sanitize / retry / echo-hash discipline can never diverge between them.
func (a *Agent) sanitizeAndWriteClipboard(ctx context.Context, textStr string) error {
	// Defense-in-depth: re-sanitize host-origin text before it reaches the guest
	// clipboard, independent of the renderer's sanitizeGuestText. A bypassed or
	// buggy host/proxy must not be able to inject control characters here (SEC-01).
	textStr = sanitizeClipboardText(textStr)

	// NEVER clobber the guest clipboard with an EMPTY payload. QEMU hands us an
	// empty host clipboard when its host-side peer state was reset by a reconnect
	// (observed: bytes=0 host→guest writes wiping whatever the operator just
	// copied in the guest). Because the guest clipboard is the SHARED endpoint,
	// that wipe breaks BOTH directions — the fresh guest copy is gone before it
	// can be sent host-ward, and a fresh host paste lands then vanishes — so new
	// content "never updates". An empty transfer is a no-op; and it must NOT
	// record a self-write hash, or the next genuine identical copy would be
	// echo-suppressed.
	if textStr == "" {
		a.log.Debug("vdagent: empty host clipboard — skipping guest write (no clobber)")
		return nil
	}

	// Record the echo marker BEFORE the OS write. The clipboard-change
	// notification can fire DURING a large WriteText (before a post-write record
	// would run), and a slow write must never push the echo comparison outside a
	// wall-clock window — either way pollClipboard would latch a stale hash and
	// bounce our own paste back through reverse-BCP, wedging the channel. Cleared
	// below if the write ultimately fails, so a lost write never suppresses a
	// later genuine identical operator copy.
	a.mu.Lock()
	a.markSelfWriteLocked(hashText(canonicalClip(textStr)))
	a.mu.Unlock()

	// Write with a bounded retry: Windows OpenClipboard contention (another
	// app momentarily holding the clipboard) is routine and transient.
	var writeErr error
	for attempt := 1; attempt <= 3; attempt++ {
		ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
		writeErr = a.clipboard.WriteText(ctx2, []byte(textStr))
		cancel()
		if writeErr == nil {
			break
		}
		a.log.Warn("vdagent: clipboard write failed, retrying", "attempt", attempt, "err", writeErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	if writeErr != nil {
		// The guest clipboard does NOT hold this text, so clear the marker we set
		// pre-write: a later identical guest copy must not be suppressed as an
		// echo. Surface the failure (the read loop logs the returned error)
		// rather than pretending the paste succeeded.
		a.mu.Lock()
		a.lastClipEchoPending = false
		a.mu.Unlock()
		a.log.Error("vdagent: clipboard write failed after retries — host→guest paste lost", "err", writeErr)
		return fmt.Errorf("clipboard write failed after retries: %w", writeErr)
	}

	// The echo marker was recorded over the canonical (LF) form BEFORE the write
	// (above) so a CRLF round-trip through the OS clipboard still matches and we
	// never bounce our own write back to the host; it survives a successful write.
	a.log.Info("vdagent: clipboard written to guest OS", "bytes", len(textStr))
	return nil
}

// handleResizeCommand parses and applies a resize side-channel command
// of the form "__AHVRC_RESIZE__<width>x<height>".
func (a *Agent) handleResizeCommand(cmd string) error {
	parts := strings.TrimPrefix(cmd, "__AHVRC_RESIZE__")
	dims := strings.SplitN(parts, "x", 2)
	if len(dims) != 2 {
		a.log.Warn("vdagent: malformed resize command", "cmd", cmd)
		return nil
	}
	w, err1 := strconv.Atoi(dims[0])
	h, err2 := strconv.Atoi(dims[1])
	// SECU-05: bound BOTH ends. The lower bound rejects degenerate modes; the
	// upper bound (8K: 7680x4320) stops a host/proxy from persisting an
	// arbitrary oversized display mode (DoS / bad-mode) on the guest.
	if err1 != nil || err2 != nil || w < 320 || h < 240 || w > 7680 || h > 4320 {
		a.log.Warn("vdagent: invalid resize dimensions", "cmd", cmd)
		return nil
	}

	if a.display == nil {
		a.log.Debug("vdagent: resize requested but no display handler")
		return nil
	}

	if err := a.display.Resize(w, h); err != nil {
		a.log.Warn("vdagent: display resize failed",
			"requested", fmt.Sprintf("%dx%d", w, h), "err", err)
		return nil
	}
	a.log.Info("vdagent: display resized", "requested", fmt.Sprintf("%dx%d", w, h))
	return nil
}

// handleKeepwarmProbe answers the proxy's keep-warm capability probe by
// sending keepwarmAckText as an ordinary guest->host clipboard transfer (grab
// now; the host's CLIPBOARD_REQUEST is served from the grab cache). The proxy
// swallows the ack and only then starts sending real KEEPWARM leases — so an
// agent too old to ack never receives a lease token at all.
func (a *Agent) handleKeepwarmProbe(ctx context.Context) error {
	a.mu.Lock()
	useSelection := a.useSelection
	// Fresh-only: a cache older than grabCacheTTL is already expired (lazily,
	// in takeGrabCache) and must not block re-acks forever when a REQUEST
	// never arrived for it.
	grabPending := a.grabCache != nil && time.Since(a.grabCacheTime) <= grabCacheTTL
	a.mu.Unlock()

	// A REAL guest copy is mid-flight (grabbed, host REQUEST not served yet):
	// acking now would clobber its cache and silently lose the operator's
	// copy — the starving-real-copies regression class this design must
	// avoid. Skip; the proxy re-probes within its renewal interval.
	if grabPending {
		a.log.Info("vdagent: keepwarm probe deferred — guest copy in flight")
		return nil
	}

	a.log.Info("vdagent: keepwarm probe received, sending ack grab")
	a.setGrabCache([]byte(keepwarmAckText), true)

	grabData := BuildClipboardGrab(SelectionClipboard, []uint32{ClipboardUTF8Text}, useSelection)
	grabMsg := EncodeChunkedMessage(PortServer, MsgClipboardGrab, grabData)
	if _, err := a.writeSerial(ctx, grabMsg); err != nil {
		a.wipeGrabCache()
		return err
	}
	return nil
}

// handleKeepwarmCommand parses a "__AHVRC_KEEPWARM__<ttl_s>" lease command:
// ttl>0 arms/extends the display keep-warm lease, ttl=0 disarms it
// immediately. Malformed or out-of-range commands are dropped; like every
// side-channel token, the text is NEVER written to the guest clipboard.
func (a *Agent) handleKeepwarmCommand(cmd string) error {
	ttl, err := strconv.Atoi(strings.TrimPrefix(cmd, keepwarmPrefix))
	if err != nil || ttl < 0 || ttl > maxKeepwarmTTLSeconds {
		a.log.Warn("vdagent: invalid keepwarm command", "cmd", cmd)
		return nil
	}
	if ttl == 0 {
		a.log.Info("vdagent: keepwarm lease cancelled")
		a.keepwarm.Disarm()
		return nil
	}
	a.log.Info("vdagent: keepwarm lease armed", "ttl_s", ttl)
	a.keepwarm.Arm(time.Duration(ttl) * time.Second)
	return nil
}

// grabCacheTTL bounds how long grab-time clipboard text is held waiting for
// the host's CLIPBOARD_REQUEST. The proxy auto-requests on NOTIFY, so the
// round-trip is normally sub-second; a request that hasn't arrived after this
// long never will, and the cleartext must not linger.
const grabCacheTTL = 10 * time.Second

// setGrabCache stores a copy of text as the pending guest->host transfer,
// wiping any previous cache first. protocol marks side-channel data (BCP
// frame/ack, keep-warm ack) that pollClipboard must not wipe on a self-echo.
func (a *Agent) setGrabCache(text []byte, protocol bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setGrabCacheLocked(text, protocol)
}

// setGrabCacheLocked is setGrabCache without the lock: the caller must already
// hold a.mu. It lets a caller decide-and-stamp the slot atomically (e.g.
// sendSideChannel's protocol-ack gate), so a concurrent reverse armFrame cannot
// slip a frame into the slot between the check and the overwrite.
func (a *Agent) setGrabCacheLocked(text []byte, protocol bool) {
	wipeBytes(a.grabCache)
	a.grabCache = append([]byte(nil), text...)
	a.grabCacheTime = time.Now()
	a.grabCacheProtocol = protocol
	a.grabCacheOwner = "" // operator / keep-warm / ack — not a reverse-BCP frame
}

// takeGrabCache removes and returns the pending grab-time text. Single use:
// the caller owns the returned bytes and must wipe them after sending.
// Returns nil if no cache is pending or it is older than grabCacheTTL.
func (a *Agent) takeGrabCache() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	text := a.grabCache
	a.grabCache = nil
	a.grabCacheProtocol = false
	a.grabCacheOwner = ""
	if text != nil && time.Since(a.grabCacheTime) > grabCacheTTL {
		wipeBytes(text)
		return nil
	}
	return text
}

// wipeGrabCache zeroes and drops the pending grab-time text.
func (a *Agent) wipeGrabCache() {
	a.mu.Lock()
	defer a.mu.Unlock()
	wipeBytes(a.grabCache)
	a.grabCache = nil
	a.grabCacheProtocol = false
	a.grabCacheOwner = ""
}

// armFrame atomically registers the ACK waiter for (xferID, seq) AND stamps the
// frame into the grab cache — but only while this transfer is still the active
// one. Returns the ack channel, or nil if a newer copy has superseded it. Doing
// both under blobTx.mu (then a.mu) closes the window where a concurrent
// supersede could interleave between arming and caching and clobber a fresh
// operator copy. Lock order is always blobTx.mu → a.mu; no path takes the reverse.
func (a *Agent) armFrame(xferID string, seq int, frame []byte) chan string {
	a.blobTx.mu.Lock()
	defer a.blobTx.mu.Unlock()
	if a.blobTx.activeID != xferID {
		return nil
	}
	ch := make(chan string, 1)
	a.blobTx.waiter = &txWaiter{xferID: xferID, seq: seq, ch: ch}
	a.mu.Lock()
	wipeBytes(a.grabCache)
	a.grabCache = append([]byte(nil), frame...)
	a.grabCacheTime = time.Now()
	a.grabCacheProtocol = true
	a.grabCacheOwner = xferID
	a.mu.Unlock()
	return ch
}

// releaseTransfer clears this transfer's authority and wipes its residual frame
// (compressed operator data) — but only while it is STILL active and STILL owns
// the grab cache, so a newer transfer or a fresh operator copy that took the
// slot is never disturbed.
func (a *Agent) releaseTransfer(xferID string) {
	a.blobTx.mu.Lock()
	defer a.blobTx.mu.Unlock()
	if a.blobTx.activeID != xferID {
		return
	}
	a.blobTx.activeID = ""
	a.blobTx.waiter = nil
	a.mu.Lock()
	if a.grabCacheOwner == xferID {
		wipeBytes(a.grabCache)
		a.grabCache = nil
		a.grabCacheProtocol = false
		a.grabCacheOwner = ""
	}
	a.mu.Unlock()
}

// wipeBytes zeroes b in place: clipboard cleartext must not linger on the
// heap after use.
func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func (a *Agent) pollClipboard(ctx context.Context) {
	ch := a.clipboard.Watch(ctx)
	var lastText string
	// lastSuppressedEcho is the exact text of the most recently suppressed self-
	// write echo. Consumed once to drop a platform double-fire of that echo
	// WITHOUT clobbering the persistent lastText dedup key, so a later genuine
	// identical operator re-copy is still delivered to the host.
	var lastSuppressedEcho string

	for {
		select {
		case <-ctx.Done():
			a.wipeGrabCache()
			return
		case <-ch:
			a.mu.Lock()
			connected := a.connected
			clipEnabled := a.clipboardEnabled
			useSelection := a.useSelection
			lastHash := a.lastClipHash
			lastTime := a.lastClipTime
			echoPending := a.lastClipEchoPending
			grabProtocol := a.grabCacheProtocol
			dedupReset := a.pollDedupResetPending
			a.pollDedupResetPending = false
			a.mu.Unlock()

			// A chardev swap reset QEMU's clipboard; clear the dedup key so the
			// operator's re-copy of the same text is re-sent instead of latched.
			if dedupReset {
				lastText = ""
			}

			// A pending PROTOCOL entry (a BCP ack/frame or keep-warm ack) is
			// mid-delivery: the clipboard change waking us is our OWN host→guest
			// write echoing back, not new operator content, so wiping it would
			// strand the ack the host is about to REQUEST (the large-paste
			// final-frame stall). Operator grab-text, by contrast, is stale the
			// moment the clipboard moves — drop it. Either way, a genuinely new
			// operator copy below supersedes it (setGrabCache / startReverseBlob).
			if !grabProtocol {
				a.wipeGrabCache()
			}

			if !connected || !clipEnabled {
				continue
			}

			// ReadText with a short timeout so a hung OS clipboard doesn't block
			// the watcher forever.
			readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			text, err := a.clipboard.ReadText(readCtx)
			cancel()

			if err != nil || len(text) == 0 || string(text) == lastText {
				continue
			}

			// Don't echo back text WE just wrote to the guest clipboard (a host
			// paste bouncing back). Match on the canonical (LF) form so a CRLF
			// round-trip through the OS clipboard doesn't defeat suppression, and
			// CONSUME the one-shot marker so suppression is immune to how long the
			// write/read took — the old 3 s window let a slow large-paste echo
			// escape and loop back through reverse-BCP, wedging the channel. The
			// backstop only guards the rare case where the OS coalesced our write
			// so no echo ever arrives to consume the marker.
			matchHash := hashText(canonicalClip(string(text)))
			if echoPending && matchHash == lastHash && time.Since(lastTime) < selfEchoBackstop {
				// Consume the marker, but ONLY if a newer self-write hasn't
				// replaced it since we snapshotted it above (a second paste can
				// land during the up-to-2 s ReadText): clearing a newer write's
				// LIVE marker would let ITS echo escape into reverse-BCP and
				// re-wedge the channel. Re-check under the lock against the hash
				// we actually matched.
				a.mu.Lock()
				if a.lastClipEchoPending && a.lastClipHash == matchHash {
					a.lastClipEchoPending = false
				}
				a.mu.Unlock()
				lastSuppressedEcho = string(text)
				continue
			}
			// A platform double-fire of the echo we just suppressed (the one-shot
			// marker is already consumed): drop it once, then forget it so an
			// identical operator re-copy afterward is delivered normally.
			if lastSuppressedEcho != "" && string(text) == lastSuppressedEcho {
				lastSuppressedEcho = ""
				continue
			}

			// A new operator copy supersedes any reverse transfer still in flight.
			a.blobTx.cancel()

			// Large copies can't survive the direct path: QEMU's VNC server
			// silently drops any guest→host PROVIDE that deflates over 1 MiB.
			// Chunk them through the reverse Big-Clipboard Protocol instead
			// (bigclip_tx.go), which runs a background goroutine and manages its
			// own grabs/acks — mirror of the host→guest sender in BigClipboard.ts.
			if len(text) > reverseBlobThreshold {
				a.startReverseBlob(ctx, text)
				lastText = string(text) // transfer is under way — dedup identical re-fires
				a.log.Info("vdagent: guest clipboard changed, starting large transfer", "bytes", len(text))
				continue
			}

			// Cache the text the grab announces, so the host's REQUEST is
			// served exactly this content without a second OS clipboard read.
			a.setGrabCache(text, false)

			grabData := BuildClipboardGrab(SelectionClipboard, []uint32{ClipboardUTF8Text}, useSelection)
			grabMsg := EncodeChunkedMessage(PortServer, MsgClipboardGrab, grabData)
			if _, err := a.writeSerial(ctx, grabMsg); err != nil {
				// Do NOT advance lastText: the grab never left the guest (a chardev
				// swap / write failure), so the operator's re-copy — even of the
				// IDENTICAL text — must re-fire, not be silently deduped away. This
				// is the guest→host recovery path the "had to copy it 3 times"
				// symptom needs; advancing lastText here permanently latched it.
				a.wipeGrabCache()
				a.log.Warn("vdagent: failed to send grab", "err", err)
				continue
			}
			lastText = string(text) // grab left the guest — now safe to dedup re-fires

			a.log.Info("vdagent: guest clipboard changed, sent grab", "bytes", len(text))
		}
	}
}

func hashText(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// canonicalClip folds CRLF and lone CR to LF so clipboard echo-suppression
// hashes are stable regardless of how a given OS clipboard round-trips line
// endings (Windows stores CRLF; the host sends LF). Used only for the
// dedup/echo hashes, not for what is actually written to the clipboard.
func canonicalClip(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}
