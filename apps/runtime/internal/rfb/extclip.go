package rfb

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"math"
)

// Extended Clipboard pseudo-encoding (0xC0A1E5CE).
// QEMU 9.1 ONLY sends clipboard via this format — there is no
// legacy ServerCutText for clipboard. The proxy must:
//   1. Inject this encoding into the client's SetEncodings
//   2. Translate Extended Clipboard ServerCutText (negative length,
//      zlib-compressed) to legacy ServerCutText (positive length)
//      for the renderer

// Extended Clipboard message types (encoded in the first 4 bytes
// of the Extended Clipboard payload after decompression).
const (
	ExtClipCaps    uint32 = 1 << 24
	ExtClipRequest uint32 = 1 << 25
	ExtClipPeek    uint32 = 1 << 26
	ExtClipNotify  uint32 = 1 << 27
	ExtClipProvide uint32 = 1 << 28
)

// Extended Clipboard data types
const (
	ExtClipText uint32 = 1 << 0
)

// InjectExtendedClipboard takes a SetEncodings message and adds
// EncExtendedClipboard if not already present. Returns the modified
// message bytes, or nil if no change needed.
func InjectExtendedClipboard(data []byte) []byte {
	if len(data) < 4 {
		return nil
	}
	if data[0] != MsgSetEncodings {
		return nil
	}
	nEnc := int(binary.BigEndian.Uint16(data[2:4]))
	totalLen := 4 + nEnc*4
	if len(data) < totalLen {
		return nil
	}

	if nEnc >= 0xFFFF {
		return nil
	}

	// Check if already present
	extClipBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(extClipBytes, 0xC0A1E5CE)
	for i := 0; i < nEnc; i++ {
		off := 4 + i*4
		if bytes.Equal(data[off:off+4], extClipBytes) {
			return nil // already present
		}
	}

	// Add it
	newNEnc := nEnc + 1
	result := make([]byte, 4+newNEnc*4)
	result[0] = MsgSetEncodings
	result[1] = 0
	binary.BigEndian.PutUint16(result[2:4], uint16(newNEnc)) // #nosec G115 -- nEnc >= 0xFFFF returns above, so newNEnc <= 0xFFFF and fits uint16
	copy(result[4:], data[4:totalLen])
	binary.BigEndian.PutUint32(result[4+nEnc*4:], 0xC0A1E5CE)
	return result
}

// TranslateExtClipServerCutText checks if a ServerCutText has a
// negative length (Extended Clipboard format). If so, it extracts
// the text payload and returns a legacy ServerCutText (positive
// length, raw UTF-8 text). Returns nil if not Extended Clipboard
// or if no text data is available.
func TranslateExtClipServerCutText(data []byte) []byte {
	if len(data) < 8 {
		return nil
	}
	if data[0] != MsgServerCutText {
		return nil
	}

	// Read length as signed 32-bit
	length := int32(binary.BigEndian.Uint32(data[4:8])) // #nosec G115 -- Extended Clipboard uses negative length as format signal
	if length >= 0 {
		return nil // legacy format, no translation needed
	}
	// math.MinInt32 has no positive counterpart: -length overflows back to
	// MinInt32 and int(-length) is negative, so the len-check below passes and
	// data[8:8+absLen] would panic. Reject it explicitly.
	if length == math.MinInt32 {
		return nil
	}

	// Extended Clipboard: abs(length) bytes follow
	absLen := int(-length)
	if len(data) < 8+absLen {
		return nil
	}

	payload := data[8 : 8+absLen]
	if len(payload) < 4 {
		return nil
	}

	// First 4 bytes: message type flags + data type flags
	flags := binary.BigEndian.Uint32(payload[0:4])

	// We only handle PROVIDE messages that include text data.
	// The flags field has message type in high bits and data type
	// in low bits. Reject non-PROVIDE and PROVIDE-without-text.
	if flags&ExtClipProvide == 0 || flags&ExtClipText == 0 {
		return nil
	}

	// PROVIDE payload: after the 4-byte flags, zlib-compressed data
	// Format: [4 flags] [zlib-compressed: [4 length] [text bytes]]
	if len(payload) < 5 {
		return nil
	}

	compressed := payload[4:]
	r, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil
	}
	defer r.Close()

	const maxDecompressed = 16 * 1024 * 1024 // 16 MB
	decompressed, err := io.ReadAll(io.LimitReader(r, maxDecompressed+1))
	if err != nil || len(decompressed) > maxDecompressed {
		return nil
	}

	if len(decompressed) < 4 {
		return nil
	}

	// Decompressed format: [4-byte BE text length] [text bytes]
	textLen := binary.BigEndian.Uint32(decompressed[0:4])
	if int(textLen) > len(decompressed)-4 {
		return nil
	}

	text := decompressed[4 : 4+textLen]

	// Build legacy ServerCutText
	return BuildServerCutText(text)
}

// IsExtClipNotify checks if an Extended Clipboard ServerCutText is a
// NOTIFY message (the server announcing it has new clipboard data).
func IsExtClipNotify(data []byte) bool {
	if len(data) < 8 || data[0] != MsgServerCutText {
		return false
	}
	length := int32(binary.BigEndian.Uint32(data[4:8])) // #nosec G115 -- Extended Clipboard uses negative length as format signal
	if length >= 0 || length == math.MinInt32 {
		return false
	}
	absLen := int(-length)
	if absLen < 4 || len(data) < 8+absLen {
		return false
	}
	flags := binary.BigEndian.Uint32(data[8 : 8+4])
	return flags&ExtClipNotify != 0 && flags&ExtClipText != 0
}

// IsExtClipProvide reports whether the message is an Extended Clipboard
// PROVIDE (the server delivering clipboard data), regardless of whether the
// payload could be decoded. Used to distinguish a PROVIDE that FAILED to
// decode (zlib error / oversize / truncated) — which means a guest copy was
// lost and must be surfaced — from benign CAPS/PEEK control messages.
func IsExtClipProvide(data []byte) bool {
	if len(data) < 12 || data[0] != MsgServerCutText {
		return false
	}
	length := int32(binary.BigEndian.Uint32(data[4:8])) // #nosec G115 -- Extended Clipboard uses negative length as format signal
	if length >= 0 || length == math.MinInt32 {
		return false
	}
	absLen := int(-length)
	if absLen < 4 || len(data) < 8+absLen {
		return false
	}
	flags := binary.BigEndian.Uint32(data[8 : 8+4])
	return flags&ExtClipProvide != 0
}

// BuildExtClipRequest builds an Extended Clipboard ClientCutText
// REQUEST message asking the server to provide text clipboard data.
func BuildExtClipRequest() []byte {
	flags := ExtClipRequest | ExtClipText
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload[0:4], flags)

	msg := make([]byte, 8+len(payload))
	msg[0] = MsgClientCutText
	binary.BigEndian.PutUint32(msg[4:8], uint32(-int32(len(payload)))) // #nosec G115 -- Extended Clipboard encodes length as negative int32
	copy(msg[8:], payload)
	return msg
}

// BuildExtClipCapsResponse builds an Extended Clipboard ClientCutText
// message advertising our clipboard capabilities (text support).
func BuildExtClipCapsResponse() []byte {
	// Extended Clipboard CAPS format:
	// ClientCutText with negative length
	// Payload: [4 flags (CAPS | supported types)] [4 max text size]
	flags := ExtClipCaps | ExtClipText
	payload := make([]byte, 8)
	binary.BigEndian.PutUint32(payload[0:4], flags)
	binary.BigEndian.PutUint32(payload[4:8], 10*1024*1024) // 10MB max

	// ClientCutText with negative length
	msg := make([]byte, 8+len(payload))
	msg[0] = MsgClientCutText
	// bytes 1-3: padding
	binary.BigEndian.PutUint32(msg[4:8], uint32(-int32(len(payload)))) // #nosec G115 -- Extended Clipboard encodes length as negative int32
	copy(msg[8:], payload)
	return msg
}
