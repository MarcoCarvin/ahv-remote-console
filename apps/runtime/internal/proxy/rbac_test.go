package proxy

import (
	"log/slog"
	"testing"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/rfb"
)

// newRBACProxy builds a bare proxy whose roleCheck reports a fixed role,
// exercising handleClientBinary's input-filtering branch in isolation (no WS).
func newRBACProxy(role string) *proxy {
	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	p := &proxy{
		log:    logger,
		sess:   &plugins.Session{ID: "sess-rbac"},
		stream: rfb.NewStreamProcessor(),
	}
	if role != "" {
		p.roleCheck = func(string) string { return role }
	}
	return p
}

// TestPresenceRBACStripsViewerInput proves AHV-02 is wired: with a viewer role
// the proxy strips KeyEvent / PointerEvent / QEMU-client input before it
// reaches the guest, while a controller's identical input is forwarded
// untouched. A nil roleCheck (presence disabled → single operator) is treated
// as controller.
func TestPresenceRBACStripsViewerInput(t *testing.T) {
	key := rfb.BuildKeyEvent(true, 0x41)          // 'A' down
	pointer := rfb.BuildPointerEvent(1, 100, 200) // left button at (100,200)
	// QEMU client message, subtype 0 (Extended Key Event): a fixed 12-byte
	// frame (type, submsg=0, then 10 payload bytes the parser frames opaquely).
	qemu := []byte{rfb.MsgQEMUClientMessage, rfb.QEMUSubmsgExtKey, 0, 1, 0, 0, 0, 0x41, 0, 0, 0, 0}

	cases := []struct {
		name  string
		input []byte
	}{
		{"key", key},
		{"pointer", pointer},
		{"qemu", qemu},
	}

	for _, tc := range cases {
		t.Run("viewer/"+tc.name, func(t *testing.T) {
			p := newRBACProxy("viewer")
			out := p.handleClientBinary(tc.input)
			// A viewer's input must be filtered: handleClientBinary returns a
			// (non-nil) rewritten buffer with the input stripped — here the frame
			// held ONLY the input, so the rewrite is empty.
			if out == nil {
				t.Fatalf("viewer %s: expected input to be stripped (non-nil filtered buffer), got nil (forwarded unchanged)", tc.name)
			}
			if len(out) != 0 {
				t.Fatalf("viewer %s: expected empty buffer after stripping, got %d bytes %v", tc.name, len(out), out)
			}
		})

		t.Run("controller/"+tc.name, func(t *testing.T) {
			p := newRBACProxy("controller")
			out := p.handleClientBinary(tc.input)
			// A controller's input is forwarded unchanged → nil signals
			// "forward original data".
			if out != nil {
				t.Fatalf("controller %s: expected input forwarded unchanged (nil), got rewritten %d bytes %v", tc.name, len(out), out)
			}
		})

		t.Run("presence-disabled/"+tc.name, func(t *testing.T) {
			p := newRBACProxy("") // nil roleCheck → single operator = controller
			out := p.handleClientBinary(tc.input)
			if out != nil {
				t.Fatalf("nil roleCheck %s: expected input forwarded unchanged (nil), got rewritten %d bytes %v", tc.name, len(out), out)
			}
		})
	}
}

// TestPresenceRBACViewerKeepsSetEncodings ensures the viewer filter strips only
// input — a non-input client message (SetEncodings) still flows so guest→host
// clipboard (Extended Clipboard injection) is unaffected for a viewer.
func TestPresenceRBACViewerKeepsSetEncodings(t *testing.T) {
	p := newRBACProxy("viewer")
	setEncodings := []byte{rfb.MsgSetEncodings, 0, 0, 1, 0, 0, 0, 0} // SetEncodings [Raw]
	out := p.handleClientBinary(setEncodings)
	if out == nil {
		t.Fatal("viewer SetEncodings: expected a rewritten buffer (Extended Clipboard injection), got nil")
	}
	if len(out) == 0 {
		t.Fatal("viewer SetEncodings: must not be stripped — clipboard must keep working for a viewer")
	}
}
