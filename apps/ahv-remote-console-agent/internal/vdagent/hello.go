// Capability negotiation over the __AHVRC_ clipboard side-channel.
//
// The host needs ONE number to size host→guest frames correctly: how large a
// burst this guest can actually absorb. It cannot be inferred, because both
// obvious paths are dead ends in QEMU:
//   - QEMU DISCARDS the VNC client's Extended-Clipboard CAPS
//     (ui/vnc-clipboard.c vnc_client_cut_text_ext: "if (flags & CAPS) return;"),
//   - and it never relays the guest's vdagent capabilities to the VNC peer.
//
// So the only channel that reaches the host is the clipboard itself, which is
// what this handshake rides — the same side-channel pattern as the keep-warm ack.
//
// The reported capacity is MEASURED, never declared: on Windows it is the receive
// queue the driver actually GRANTED (GetCommProperties read-back after SetupComm),
// on Linux the drain ring. A driver that silently clamps SetupComm therefore
// self-reports the smaller number and the host stays on small frames — instead of
// a version string promising a ceiling the guest would then drop bytes past.
//
// Wire grammar (keep in lockstep with
// apps/desktop/src/renderer/clipboard/BigClipboard.ts):
//
//	host→guest:  __AHVRC_HELLO__v1:
//	guest→host:  __AHVRC_HELLOACK__v1:<ver>:<os>:<rxCapacity>
//
// A guest too old to answer simply never replies, and the host keeps its
// conservative default frame size — so old field ISOs keep working unchanged and
// there is no flag day.

package vdagent

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/MarcoCarvin/ahv-remote-console/apps/ahv-remote-console-agent/internal/build"
)

const (
	helloPrefix    = "__AHVRC_HELLO__v1:"
	helloAckPrefix = "__AHVRC_HELLOACK__v1:"
)

// handleHello answers the host's capability probe with this guest's measured
// receive capacity.
func (a *Agent) handleHello(ctx context.Context) error {
	rx := a.rxCapacity()
	ack := fmt.Sprintf("%s%s:%s:%d", helloAckPrefix, sanitizeField(build.Version), runtime.GOOS, rx)
	a.log.Info("vdagent: hello probe — reporting measured receive capacity",
		"rx_capacity", rx, "os", runtime.GOOS, "version", build.Version)
	return a.sendSideChannel(ctx, ack)
}

// sanitizeField keeps a value safe to embed in the colon-delimited wire format.
// build.Version is set at link time and is not expected to contain a colon, but
// a stray one would silently shift every field the host parses.
func sanitizeField(s string) string {
	return strings.ReplaceAll(s, ":", "_")
}
