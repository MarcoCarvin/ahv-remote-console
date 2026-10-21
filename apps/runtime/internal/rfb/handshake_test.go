package rfb

import (
	"encoding/binary"
	"testing"
)

func buildServerInit(w, h uint16, name string) []byte {
	buf := make([]byte, 24+len(name))
	binary.BigEndian.PutUint16(buf[0:2], w)
	binary.BigEndian.PutUint16(buf[2:4], h)
	// Pixel format: 32-bit BGRX
	buf[4] = 32  // bpp
	buf[5] = 24  // depth
	buf[6] = 0   // big-endian = false
	buf[7] = 1   // true-colour
	binary.BigEndian.PutUint16(buf[8:10], 255)  // red-max
	binary.BigEndian.PutUint16(buf[10:12], 255) // green-max
	binary.BigEndian.PutUint16(buf[12:14], 255) // blue-max
	buf[14] = 16 // red-shift
	buf[15] = 8  // green-shift
	buf[16] = 0  // blue-shift
	// 17-19: padding
	binary.BigEndian.PutUint32(buf[20:24], uint32(len(name)))
	copy(buf[24:], name)
	return buf
}

func TestHandshakeFullSequence(t *testing.T) {
	ht := &HandshakeTracker{}

	// Server sends ProtocolVersion.
	serverVersion := []byte("RFB 003.008\n")
	ht.FeedServer(serverVersion)
	if ht.State != HsClientVersion {
		t.Fatalf("after server version: state = %d, want HsClientVersion", ht.State)
	}

	// Client sends ProtocolVersion.
	clientVersion := []byte("RFB 003.008\n")
	ht.FeedClient(clientVersion)
	if ht.State != HsSecurityTypes {
		t.Fatalf("after client version: state = %d, want HsSecurityTypes", ht.State)
	}

	// Server sends SecurityTypes: 1 type, type=1 (None).
	secTypes := []byte{1, 1}
	ht.FeedServer(secTypes)
	if ht.State != HsSecurityChoice {
		t.Fatalf("after sec types: state = %d, want HsSecurityChoice", ht.State)
	}

	// Client chooses security type 1 (None).
	ht.FeedClient([]byte{1})
	if ht.State != HsSecurityResult {
		t.Fatalf("after sec choice: state = %d, want HsSecurityResult", ht.State)
	}

	// Server sends SecurityResult: OK (0).
	secResult := make([]byte, 4)
	ht.FeedServer(secResult)
	if ht.State != HsClientInit {
		t.Fatalf("after sec result: state = %d, want HsClientInit", ht.State)
	}

	// Client sends ClientInit: shared=1.
	ht.FeedClient([]byte{1})
	if ht.State != HsServerInit {
		t.Fatalf("after client init: state = %d, want HsServerInit", ht.State)
	}

	// Server sends ServerInit.
	serverInit := buildServerInit(1920, 1080, "QEMU")
	ht.FeedServer(serverInit)
	if !ht.Done() {
		t.Fatal("handshake not done after ServerInit")
	}
	if ht.ServerInfo == nil {
		t.Fatal("ServerInfo is nil")
	}
	if ht.ServerInfo.Width != 1920 || ht.ServerInfo.Height != 1080 {
		t.Fatalf("dimensions = %dx%d, want 1920x1080", ht.ServerInfo.Width, ht.ServerInfo.Height)
	}
	if ht.ServerInfo.BPP != 32 {
		t.Fatalf("bpp = %d, want 32", ht.ServerInfo.BPP)
	}
	if ht.ServerInfo.Name != "QEMU" {
		t.Fatalf("name = %q, want %q", ht.ServerInfo.Name, "QEMU")
	}
}

// PROT-03: a non-zero SecurityResult is an auth FAILURE. RFB 3.8 follows it
// with a (4-byte length + string) reason and closes. The tracker must consume
// the whole reason and enter the terminal HsError state — NOT blindly advance
// to HsClientInit and mis-parse the reason bytes as ClientInit/ServerInit.
func TestHandshakeSecurityResultFailureConsumesReason(t *testing.T) {
	ht := &HandshakeTracker{State: HsSecurityResult}

	reason := "authentication failed"
	buf := make([]byte, 8+len(reason))
	binary.BigEndian.PutUint32(buf[0:4], 1) // non-zero result = failed
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(reason)))
	copy(buf[8:], reason)

	n := ht.FeedServer(buf)
	if n != len(buf) {
		t.Fatalf("consumed %d, want %d (4 result + 4 len + reason)", n, len(buf))
	}
	if !ht.Failing() {
		t.Fatalf("state = %d, want HsError after failed SecurityResult", ht.State)
	}
	if ht.Done() {
		t.Fatal("Done() must be false on a failed handshake")
	}
	if ht.ErrReason != reason {
		t.Fatalf("ErrReason = %q, want %q", ht.ErrReason, reason)
	}
}

// PROT-03: SecurityResult failure whose reason is not yet fully buffered must
// NOT transition — it waits for the rest, then completes on the next feed.
func TestHandshakeSecurityResultFailurePartialReason(t *testing.T) {
	ht := &HandshakeTracker{State: HsSecurityResult}
	reason := "denied"
	buf := make([]byte, 8+len(reason))
	binary.BigEndian.PutUint32(buf[0:4], 2)
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(reason)))
	copy(buf[8:], reason)

	// Feed only the result + length + a partial reason.
	if n := ht.FeedServer(buf[:8+3]); n != 0 {
		t.Fatalf("partial feed consumed %d, want 0 (incomplete reason)", n)
	}
	if ht.State != HsSecurityResult {
		t.Fatalf("state = %d, want HsSecurityResult (still waiting)", ht.State)
	}
	if n := ht.FeedServer(buf); n != len(buf) {
		t.Fatalf("full feed consumed %d, want %d", n, len(buf))
	}
	if !ht.Failing() {
		t.Fatalf("state = %d, want HsError", ht.State)
	}
}

// PROT-03: SecurityTypes count==0 is the RFB 3.8 connection-failed path —
// server sends a (4-byte length + string) reason and closes. Consume the whole
// reason and go terminal, instead of advancing as if the handshake succeeded.
func TestHandshakeSecurityTypesZeroCountConsumesReason(t *testing.T) {
	ht := &HandshakeTracker{State: HsSecurityTypes}

	reason := "no security types available"
	buf := make([]byte, 5+len(reason))
	buf[0] = 0 // count==0
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(reason)))
	copy(buf[5:], reason)

	n := ht.FeedServer(buf)
	if n != len(buf) {
		t.Fatalf("consumed %d, want %d (1 count + 4 len + reason)", n, len(buf))
	}
	if !ht.Failing() {
		t.Fatalf("state = %d, want HsError after count==0", ht.State)
	}
	if ht.Done() {
		t.Fatal("Done() must be false after count==0")
	}
	if ht.ErrReason != reason {
		t.Fatalf("ErrReason = %q, want %q", ht.ErrReason, reason)
	}
}

// PROT-04: only security type 1 (None) is accepted. A chosen type the AHV path
// never uses (e.g. VNC challenge-response, type 2) must terminate the handshake
// rather than silently advance into auth bytes we cannot frame.
func TestHandshakeSecurityChoiceRejectsNonNone(t *testing.T) {
	// Server offered None(1) + VNC(2); client (mis)chooses VNC(2).
	ht := &HandshakeTracker{State: HsSecurityTypes}
	ht.FeedServer([]byte{2, 1, 2}) // count=2, types {1,2}
	if ht.State != HsSecurityChoice {
		t.Fatalf("state = %d, want HsSecurityChoice", ht.State)
	}

	ht.FeedClient([]byte{2}) // chose VNC-Auth
	if !ht.Failing() {
		t.Fatalf("state = %d, want HsError on non-None security type", ht.State)
	}
	if ht.Done() {
		t.Fatal("Done() must be false when an unsupported security type is chosen")
	}
}

// PROT-04: type 1 is accepted only if the server actually offered it. A client
// choosing None(1) when the server advertised only VNC(2) is rejected.
func TestHandshakeSecurityChoiceRejectsUnofferedType(t *testing.T) {
	ht := &HandshakeTracker{State: HsSecurityTypes}
	ht.FeedServer([]byte{1, 2}) // count=1, type {2} only
	if ht.State != HsSecurityChoice {
		t.Fatalf("state = %d, want HsSecurityChoice", ht.State)
	}
	ht.FeedClient([]byte{1}) // chose None, which was never offered
	if !ht.Failing() {
		t.Fatalf("state = %d, want HsError when chosen type was not offered", ht.State)
	}
}

func TestHandshakeServerInitDimensions(t *testing.T) {
	tests := []struct {
		w, h uint16
		name string
	}{
		{1024, 768, "test-vm"},
		{2560, 1440, ""},
		{800, 600, "AHV Console"},
	}

	for _, tt := range tests {
		ht := &HandshakeTracker{State: HsServerInit}
		data := buildServerInit(tt.w, tt.h, tt.name)
		ht.FeedServer(data)
		if !ht.Done() {
			t.Errorf("%dx%d: not done", tt.w, tt.h)
			continue
		}
		if ht.ServerInfo.Width != tt.w || ht.ServerInfo.Height != tt.h {
			t.Errorf("got %dx%d, want %dx%d", ht.ServerInfo.Width, ht.ServerInfo.Height, tt.w, tt.h)
		}
		if ht.ServerInfo.Name != tt.name {
			t.Errorf("name = %q, want %q", ht.ServerInfo.Name, tt.name)
		}
	}
}
