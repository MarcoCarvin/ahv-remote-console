// Package rfb provides a streaming parser for RFB 3.8 (VNC) protocol
// messages. It identifies message boundaries and extracts structural
// metadata (rectangle coordinates, encoding types) without decoding
// pixel payloads. Designed for use in a proxy that needs to inspect
// specific messages while forwarding the rest unchanged.
package rfb

import "encoding/binary"

// Server-to-client message types (RFB 3.8, §7.6).
const (
	MsgFramebufferUpdate   uint8 = 0
	MsgSetColourMapEntries uint8 = 1
	MsgBell                uint8 = 2
	MsgServerCutText       uint8 = 3
)

// Client-to-server message types (RFB 3.8, §7.5).
const (
	MsgSetPixelFormat       uint8 = 0
	MsgSetEncodings         uint8 = 2
	MsgFramebufferUpdateReq uint8 = 3
	MsgKeyEvent             uint8 = 4
	MsgPointerEvent         uint8 = 5
	MsgClientCutText        uint8 = 6
	MsgSetDesktopSize       uint8 = 251 // RFC 6143 §7.5.6
	MsgQEMUClientMessage    uint8 = 255 // QEMU client message (subtype 0 = Extended Key Event)
)

// QEMUSubmsgExtKey is the subtype of MsgQEMUClientMessage carrying a 12-byte
// Extended Key Event (u8 type=255, u8 subtype=0, u16 down, u32 keysym, u32
// keycode). We parse it only to keep the client stream framed — the proxy
// never originates it.
const QEMUSubmsgExtKey uint8 = 0

// Encoding types we care about for data-region extraction.
const (
	EncRaw      int32 = 0
	EncCopyRect int32 = 1
	EncRRE      int32 = 2
	EncHextile  int32 = 5
	EncZRLE     int32 = 16
	EncTight    int32 = 7
	EncTightPNG int32 = -260

	// Pseudo-encodings.
	EncCursor              int32 = -239
	EncDesktopSize         int32 = -223
	EncExtendedDesktopSize int32 = -308
	EncExtendedClipboard   int32 = -1063131698 // 0xC0A1E5CE
)

// Rect describes one rectangle in a FramebufferUpdate message.
type Rect struct {
	X, Y          uint16
	Width, Height uint16
	Encoding      int32

	// DataOffset is the byte offset within the FramebufferUpdate message
	// where this rectangle's pixel data begins (after the 12-byte rect header).
	DataOffset int

	// DataLen is the byte length of this rectangle's pixel payload.
	// For Raw encoding: width * height * bytesPerPixel.
	// For other encodings: determined by parsing.
	// -1 if unknown (encoding not parsed).
	DataLen int
}

// FramebufferUpdateRequest builds a 10-byte FBUpdateRequest message.
func FramebufferUpdateRequest(incremental bool, x, y, w, h uint16) []byte {
	buf := make([]byte, 10)
	buf[0] = MsgFramebufferUpdateReq
	if incremental {
		buf[1] = 1
	}
	binary.BigEndian.PutUint16(buf[2:4], x)
	binary.BigEndian.PutUint16(buf[4:6], y)
	binary.BigEndian.PutUint16(buf[6:8], w)
	binary.BigEndian.PutUint16(buf[8:10], h)
	return buf
}

// BuildServerCutText constructs a valid ServerCutText message.
func BuildServerCutText(text []byte) []byte {
	buf := make([]byte, 8+len(text))
	buf[0] = MsgServerCutText
	// bytes 1-3: padding
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(text))) // #nosec G115 -- length bounded by MaxClipboardSize; fits uint32
	copy(buf[8:], text)
	return buf
}

// BuildClientCutText constructs a valid ClientCutText message.
func BuildClientCutText(text []byte) []byte {
	buf := make([]byte, 8+len(text))
	buf[0] = MsgClientCutText
	// bytes 1-3: padding
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(text))) // #nosec G115 -- length bounded by MaxClipboardSize; fits uint32
	copy(buf[8:], text)
	return buf
}

// BuildPointerEvent constructs a 6-byte PointerEvent message.
func BuildPointerEvent(buttonMask uint8, x, y uint16) []byte {
	buf := make([]byte, 6)
	buf[0] = MsgPointerEvent
	buf[1] = buttonMask
	binary.BigEndian.PutUint16(buf[2:4], x)
	binary.BigEndian.PutUint16(buf[4:6], y)
	return buf
}

// BuildKeyEvent constructs an 8-byte KeyEvent message.
func BuildKeyEvent(down bool, keysym uint32) []byte {
	buf := make([]byte, 8)
	buf[0] = MsgKeyEvent
	if down {
		buf[1] = 1
	}
	// bytes 2-3: padding
	binary.BigEndian.PutUint32(buf[4:8], keysym)
	return buf
}
