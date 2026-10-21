package rfb

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Direction indicates which side of the RFB connection is sending.
type Direction int

const (
	ServerToClient Direction = iota
	ClientToServer
)

// ParsedMessage is the result of parsing one RFB message from a byte buffer.
type ParsedMessage struct {
	Type      uint8
	Direction Direction
	Raw       []byte // complete message bytes including header

	// Populated for FramebufferUpdate only.
	Rects []Rect

	// Populated for ServerCutText / ClientCutText only.
	CutText []byte
}

var (
	ErrIncomplete = errors.New("rfb: incomplete message")
	ErrUnknown    = errors.New("rfb: unknown message type")
)

// ParseServerMessage parses one server→client message from the front of buf.
// Returns the parsed message and the number of bytes consumed.
// Returns ErrIncomplete if buf doesn't contain a complete message.
func ParseServerMessage(buf []byte, bpp int) (ParsedMessage, int, error) {
	if len(buf) < 1 {
		return ParsedMessage{}, 0, ErrIncomplete
	}

	msgType := buf[0]
	switch msgType {
	case MsgFramebufferUpdate:
		return parseFramebufferUpdate(buf, bpp)
	case MsgSetColourMapEntries:
		return parseSetColourMapEntries(buf)
	case MsgBell:
		return ParsedMessage{Type: MsgBell, Direction: ServerToClient, Raw: buf[:1]}, 1, nil
	case MsgServerCutText:
		return parseServerCutText(buf)
	default:
		return ParsedMessage{}, 0, fmt.Errorf("%w: server message type %d", ErrUnknown, msgType)
	}
}

// ParseClientMessage parses one client→server message from the front of buf.
// Returns the parsed message and the number of bytes consumed.
func ParseClientMessage(buf []byte) (ParsedMessage, int, error) {
	if len(buf) < 1 {
		return ParsedMessage{}, 0, ErrIncomplete
	}

	msgType := buf[0]
	switch msgType {
	case MsgSetPixelFormat:
		if len(buf) < 20 {
			return ParsedMessage{}, 0, ErrIncomplete
		}
		return ParsedMessage{Type: MsgSetPixelFormat, Direction: ClientToServer, Raw: buf[:20]}, 20, nil
	case MsgSetEncodings:
		return parseSetEncodings(buf)
	case MsgFramebufferUpdateReq:
		if len(buf) < 10 {
			return ParsedMessage{}, 0, ErrIncomplete
		}
		return ParsedMessage{Type: MsgFramebufferUpdateReq, Direction: ClientToServer, Raw: buf[:10]}, 10, nil
	case MsgKeyEvent:
		if len(buf) < 8 {
			return ParsedMessage{}, 0, ErrIncomplete
		}
		return ParsedMessage{Type: MsgKeyEvent, Direction: ClientToServer, Raw: buf[:8]}, 8, nil
	case MsgPointerEvent:
		if len(buf) < 6 {
			return ParsedMessage{}, 0, ErrIncomplete
		}
		return ParsedMessage{Type: MsgPointerEvent, Direction: ClientToServer, Raw: buf[:6]}, 6, nil
	case MsgClientCutText:
		return parseClientCutText(buf)
	case MsgSetDesktopSize:
		return parseSetDesktopSize(buf)
	case MsgQEMUClientMessage:
		// QEMU client message. Subtype 0 = Extended Key Event, a fixed 12-byte
		// frame. We don't act on it, but we MUST frame it so the rest of the
		// client stream stays parseable (otherwise a frame mixing this with
		// SetEncodings/KeyEvent/PointerEvent would abort parsing and silently
		// skip clipboard injection + viewer input-filtering).
		if len(buf) < 2 {
			return ParsedMessage{}, 0, ErrIncomplete
		}
		if buf[1] != QEMUSubmsgExtKey {
			return ParsedMessage{}, 0, fmt.Errorf("%w: qemu submessage %d", ErrUnknown, buf[1])
		}
		if len(buf) < 12 {
			return ParsedMessage{}, 0, ErrIncomplete
		}
		return ParsedMessage{Type: MsgQEMUClientMessage, Direction: ClientToServer, Raw: buf[:12]}, 12, nil
	default:
		return ParsedMessage{}, 0, fmt.Errorf("%w: client message type %d", ErrUnknown, msgType)
	}
}

func parseSetEncodings(buf []byte) (ParsedMessage, int, error) {
	if len(buf) < 4 {
		return ParsedMessage{}, 0, ErrIncomplete
	}
	nEnc := int(binary.BigEndian.Uint16(buf[2:4]))
	total := 4 + nEnc*4
	if len(buf) < total {
		return ParsedMessage{}, 0, ErrIncomplete
	}
	return ParsedMessage{Type: MsgSetEncodings, Direction: ClientToServer, Raw: buf[:total]}, total, nil
}

func parseServerCutText(buf []byte) (ParsedMessage, int, error) {
	if len(buf) < 8 {
		return ParsedMessage{}, 0, ErrIncomplete
	}
	// RFB Extended Clipboard signals its format with a NEGATIVE length, so the
	// 4-byte length MUST be read as a signed int32: int(int32(...)) wraps the
	// high-bit-set extended value negative on 64-bit, where a bare int(uint32)
	// would be a huge positive and silently miss the ext-clip path. Matches
	// extclip.go.
	length := int(int32(binary.BigEndian.Uint32(buf[4:8]))) // #nosec G115 -- signed reinterpretation is intentional (ext-clip uses negative length)
	if length < 0 {
		// Extended clipboard — negative length means extended format.
		// Consume abs(length) bytes after the header for alignment.
		absLen := -length
		total := 8 + absLen
		if len(buf) < total {
			return ParsedMessage{}, 0, ErrIncomplete
		}
		return ParsedMessage{
			Type:      MsgServerCutText,
			Direction: ServerToClient,
			Raw:       buf[:total],
		}, total, nil
	}
	total := 8 + length
	if len(buf) < total {
		return ParsedMessage{}, 0, ErrIncomplete
	}
	return ParsedMessage{
		Type:      MsgServerCutText,
		Direction: ServerToClient,
		Raw:       buf[:total],
		CutText:   buf[8:total],
	}, total, nil
}

func parseClientCutText(buf []byte) (ParsedMessage, int, error) {
	if len(buf) < 8 {
		return ParsedMessage{}, 0, ErrIncomplete
	}
	// Signed int32 read — see parseServerCutText: ext-clip uses a negative length.
	length := int(int32(binary.BigEndian.Uint32(buf[4:8]))) // #nosec G115 -- intentional signed reinterpretation
	if length < 0 {
		absLen := -length
		total := 8 + absLen
		if len(buf) < total {
			return ParsedMessage{}, 0, ErrIncomplete
		}
		return ParsedMessage{
			Type:      MsgClientCutText,
			Direction: ClientToServer,
			Raw:       buf[:total],
			CutText:   nil,
		}, total, nil
	}
	total := 8 + length
	if len(buf) < total {
		return ParsedMessage{}, 0, ErrIncomplete
	}
	return ParsedMessage{
		Type:      MsgClientCutText,
		Direction: ClientToServer,
		Raw:       buf[:total],
		CutText:   buf[8:total],
	}, total, nil
}

func parseSetColourMapEntries(buf []byte) (ParsedMessage, int, error) {
	if len(buf) < 6 {
		return ParsedMessage{}, 0, ErrIncomplete
	}
	nColours := int(binary.BigEndian.Uint16(buf[4:6]))
	total := 6 + nColours*6 // each colour: R(2) + G(2) + B(2)
	if len(buf) < total {
		return ParsedMessage{}, 0, ErrIncomplete
	}
	return ParsedMessage{Type: MsgSetColourMapEntries, Direction: ServerToClient, Raw: buf[:total]}, total, nil
}

// parseFramebufferUpdate parses a FramebufferUpdate message.
// It extracts rectangle metadata for all rects. For Raw-encoded rects,
// it computes exact data length. For other encodings, DataLen is set to
// the actual consumed bytes.
func parseFramebufferUpdate(buf []byte, bpp int) (ParsedMessage, int, error) {
	if len(buf) < 4 {
		return ParsedMessage{}, 0, ErrIncomplete
	}
	nRects := int(binary.BigEndian.Uint16(buf[2:4]))

	offset := 4
	rects := make([]Rect, 0, nRects)

	for i := 0; i < nRects; i++ {
		if len(buf) < offset+12 {
			return ParsedMessage{}, 0, ErrIncomplete
		}
		r := Rect{
			X:        binary.BigEndian.Uint16(buf[offset : offset+2]),
			Y:        binary.BigEndian.Uint16(buf[offset+2 : offset+4]),
			Width:    binary.BigEndian.Uint16(buf[offset+4 : offset+6]),
			Height:   binary.BigEndian.Uint16(buf[offset+6 : offset+8]),
			Encoding: int32(binary.BigEndian.Uint32(buf[offset+8 : offset+12])), // #nosec G115 -- RFB encoding is a signed 32-bit wire value (negative = pseudo-encoding)
		}
		offset += 12
		r.DataOffset = offset

		dataLen, err := rectDataLen(buf[offset:], r, bpp)
		if err != nil {
			return ParsedMessage{}, 0, err
		}
		// Defensive: every rectDataLen path now computes in int64 and bounds
		// against len(data), so dataLen is in [0, len(buf)-offset]. This guard
		// is the belt to that suspenders — a negative or buffer-overrunning
		// length must refuse rather than panic on buf[:offset] below.
		if dataLen < 0 || offset+dataLen < offset || offset+dataLen > len(buf) {
			return ParsedMessage{}, 0, ErrIncomplete
		}
		r.DataLen = dataLen
		offset += dataLen

		rects = append(rects, r)
	}

	return ParsedMessage{
		Type:      MsgFramebufferUpdate,
		Direction: ServerToClient,
		Raw:       buf[:offset],
		Rects:     rects,
	}, offset, nil
}

// rectDataLen determines the byte length of a rectangle's pixel data.
// For Raw encoding the length is deterministic. For CopyRect it's always 4.
// For variable-length encodings (Hextile, ZRLE, Tight), we must walk the
// data to find the end — but for the initial implementation we return an
// error for unsupported encodings, since the data region should always be
// requested in Raw encoding.
func rectDataLen(data []byte, r Rect, bpp int) (int, error) {
	w := int(r.Width)
	h := int(r.Height)

	switch r.Encoding {
	case EncRaw:
		// int64 math: w,h are u16 and bpp up to 4, so w*h*bpp can reach
		// ~1.7e10 — past int32. On the shipped windows/386 build (32-bit int)
		// the native-int form wraps negative, the `len(data) < n` guard is
		// bypassed, and a later buf[:offset] panics, crashing the whole
		// runtime process (every operator's session).
		n := int64(w) * int64(h) * int64(bpp)
		if n > int64(len(data)) {
			return 0, ErrIncomplete
		}
		return int(n), nil

	case EncCopyRect:
		if len(data) < 4 {
			return 0, ErrIncomplete
		}
		return 4, nil

	case EncDesktopSize, EncExtendedDesktopSize:
		// Pseudo-encoding: no pixel data for DesktopSize.
		if r.Encoding == EncExtendedDesktopSize {
			return parseExtendedDesktopSizeLen(data, r)
		}
		return 0, nil

	case EncCursor:
		// Cursor pseudo-encoding: pixels + bitmask. int64 math — same u16²·bpp
		// overflow as EncRaw on 32-bit int.
		pixels := int64(w) * int64(h) * int64(bpp)
		mask := int64((w+7)/8) * int64(h)
		n := pixels + mask
		if n > int64(len(data)) {
			return 0, ErrIncomplete
		}
		return int(n), nil

	case EncHextile:
		return parseHextileLen(data, r, bpp)

	case EncZRLE:
		return parseCompactLen(data)

	case EncTight, EncTightPNG:
		return parseTightLen(data, r, bpp)

	case EncRRE:
		return parseRRELen(data, bpp)

	default:
		// Unknown encoding — we cannot determine length.
		// Return error so the caller can fall back to opaque forwarding.
		return 0, fmt.Errorf("rfb: unsupported encoding %d for rect %dx%d", r.Encoding, w, h)
	}
}

func parseExtendedDesktopSizeLen(data []byte, r Rect) (int, error) {
	if len(data) < 4 {
		return 0, ErrIncomplete
	}
	// number-of-screens is the FIRST byte of the rect data; bytes 1-3 are padding.
	nScreens := int(data[0])
	n := 4 + nScreens*16
	if len(data) < n {
		return 0, ErrIncomplete
	}
	return n, nil
}

func parseRRELen(data []byte, bpp int) (int, error) {
	if len(data) < 4 {
		return 0, ErrIncomplete
	}
	// int64 math: nSubrects is a full u32 (up to ~4.3e9); nSubrects*(bpp+8)
	// overflows int32 on windows/386 and wraps negative, bypassing the guard.
	nSubrects := int64(binary.BigEndian.Uint32(data[0:4]))
	// header (4 bytes nSubrects) + background pixel (bpp) + subrects * (bpp + 8)
	n := 4 + int64(bpp) + nSubrects*int64(bpp+8)
	if n > int64(len(data)) {
		return 0, ErrIncomplete
	}
	return int(n), nil
}

// parseCompactLen handles ZRLE-style length-prefixed compressed data.
func parseCompactLen(data []byte) (int, error) {
	if len(data) < 4 {
		return 0, ErrIncomplete
	}
	// int64: a u32 zLen with the high bit set (>= 0x80000000) becomes a
	// negative int on 32-bit, bypassing the guard and panicking downstream.
	zLen := int64(binary.BigEndian.Uint32(data[0:4]))
	n := 4 + zLen
	if n > int64(len(data)) {
		return 0, ErrIncomplete
	}
	return int(n), nil
}

func parseHextileLen(data []byte, r Rect, bpp int) (int, error) {
	w := int(r.Width)
	h := int(r.Height)
	offset := 0

	for ty := 0; ty < h; ty += 16 {
		th := 16
		if ty+th > h {
			th = h - ty
		}
		for tx := 0; tx < w; tx += 16 {
			tw := 16
			if tx+tw > w {
				tw = w - tx
			}
			if len(data) < offset+1 {
				return 0, ErrIncomplete
			}
			subencoding := data[offset]
			offset++

			if subencoding&0x01 != 0 { // Raw
				n := tw * th * bpp
				if len(data) < offset+n {
					return 0, ErrIncomplete
				}
				offset += n
				continue
			}
			if subencoding&0x02 != 0 { // BackgroundSpecified
				if len(data) < offset+bpp {
					return 0, ErrIncomplete
				}
				offset += bpp
			}
			if subencoding&0x04 != 0 { // ForegroundSpecified
				if len(data) < offset+bpp {
					return 0, ErrIncomplete
				}
				offset += bpp
			}
			if subencoding&0x08 != 0 { // AnySubrects
				if len(data) < offset+1 {
					return 0, ErrIncomplete
				}
				nSub := int(data[offset])
				offset++
				if subencoding&0x10 != 0 { // SubrectsColoured
					offset += nSub * (bpp + 2)
				} else {
					offset += nSub * 2
				}
				if len(data) < offset {
					return 0, ErrIncomplete
				}
			}
		}
	}
	return offset, nil
}

func parseTightLen(data []byte, r Rect, bpp int) (int, error) {
	if len(data) < 1 {
		return 0, ErrIncomplete
	}
	ctl := data[0]
	offset := 1

	// Reset stream flags (bits 0-3): each pair of bits resets a zlib stream.
	// Skip them — they don't add to data length.

	compType := (ctl >> 4) & 0x0F

	switch {
	case compType == 0x08: // FillCompression
		// tightMinimumBpp: if trueColour and bpp=32 with specific pixel format, use 3 bytes
		n := bpp
		if bpp == 4 {
			n = 3 // TPIXEL
		}
		if len(data) < offset+n {
			return 0, ErrIncomplete
		}
		return offset + n, nil

	case compType == 0x09: // JpegCompression
		compLen, consumed, err := readTightCompactLen(data[offset:])
		if err != nil {
			return 0, err
		}
		offset += consumed
		if len(data) < offset+compLen {
			return 0, ErrIncomplete
		}
		return offset + compLen, nil

	case compType <= 0x07: // BasicCompression
		// Filter ID is present if bit 6 is set (0x40)
		filterID := byte(0) // CopyFilter
		if ctl&0x40 != 0 {
			if len(data) < offset+1 {
				return 0, ErrIncomplete
			}
			filterID = data[offset]
			offset++
		}

		w := int(r.Width)
		h := int(r.Height)
		// int64: rowBytes*h with w=h=65535 overflows int32 (≈1.3e10), wraps
		// negative, makes the `< 12` branch fire on a hostile rect, and
		// returns offset+negative — the windows/386 process-crash path.
		rowBytes := int64(w) * int64(bpp)
		if bpp == 4 {
			rowBytes = int64(w) * 3 // TPIXEL
		}

		switch filterID {
		case 0: // CopyFilter
			// no additional filter data
		case 1: // PaletteFilter
			if len(data) < offset+1 {
				return 0, ErrIncomplete
			}
			nColors := int(data[offset]) + 1
			offset++
			// Palette entries are pixels: 3-byte TPIXELs only at 32bpp
			// (depth-24 compression) — at 16bpp the entry is the full
			// 2-byte pixel. Same rule as the fill and rowBytes paths
			// above; a hardcoded 3 misframes every 16bpp palette rect.
			entrySize := bpp
			if bpp == 4 {
				entrySize = 3 // TPIXEL
			}
			palSize := nColors * entrySize
			if len(data) < offset+palSize {
				return 0, ErrIncomplete
			}
			offset += palSize
			if nColors <= 2 {
				rowBytes = int64((w + 7) / 8) // 1bpp packed
			} else {
				rowBytes = int64(w) // 8bpp indexed
			}
		case 2: // GradientFilter
			// no additional filter data
		}

		uncompressedLen := rowBytes * int64(h)
		if uncompressedLen < 12 {
			// Data sent uncompressed.
			if int64(len(data)) < int64(offset)+uncompressedLen {
				return 0, ErrIncomplete
			}
			return offset + int(uncompressedLen), nil
		}
		// Compressed with zlib. Length uses tight compact encoding.
		compLen, consumed, err := readTightCompactLen(data[offset:])
		if err != nil {
			return 0, err
		}
		offset += consumed
		if len(data) < offset+compLen {
			return 0, ErrIncomplete
		}
		return offset + compLen, nil

	default:
		return 0, fmt.Errorf("rfb: unknown tight compression type %d", compType)
	}
}

// readTightCompactLen reads a 1-3 byte compact length used by Tight encoding.
func readTightCompactLen(data []byte) (length int, consumed int, err error) {
	if len(data) < 1 {
		return 0, 0, ErrIncomplete
	}
	b0 := int(data[0])
	length = b0 & 0x7F
	consumed = 1
	if b0&0x80 != 0 {
		if len(data) < 2 {
			return 0, 0, ErrIncomplete
		}
		b1 := int(data[1])
		length |= (b1 & 0x7F) << 7
		consumed = 2
		if b1&0x80 != 0 {
			if len(data) < 3 {
				return 0, 0, ErrIncomplete
			}
			b2 := int(data[2])
			length |= b2 << 14
			consumed = 3
		}
	}
	return length, consumed, nil
}

// parseSetDesktopSize parses a SetDesktopSize client message (type 251).
// Wire format: type(1) + pad(1) + width(2) + height(2) + numScreens(1) +
// pad(1) + numScreens * screenData(16).  Minimum 8 bytes (header with 0
// screens).
func parseSetDesktopSize(buf []byte) (ParsedMessage, int, error) {
	if len(buf) < 8 {
		return ParsedMessage{}, 0, ErrIncomplete
	}
	nScreens := int(buf[6])
	total := 8 + nScreens*16
	if len(buf) < total {
		return ParsedMessage{}, 0, ErrIncomplete
	}
	return ParsedMessage{Type: MsgSetDesktopSize, Direction: ClientToServer, Raw: buf[:total]}, total, nil
}
