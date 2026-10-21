package rfb

// StreamProcessor wraps the low-level parser with the framebuffer state the
// proxy needs: the negotiated bytes-per-pixel (so variable-length encodings can
// be walked) and a buffer for messages split across WebSocket frames.
type StreamProcessor struct {
	bpp int // bytes per pixel, set after SetPixelFormat
}

// NewStreamProcessor creates a processor with default 4 bpp (32-bit truecolour).
func NewStreamProcessor() *StreamProcessor {
	return &StreamProcessor{
		bpp: 4,
	}
}

// SetBPP updates the bytes-per-pixel after a SetPixelFormat message.
func (s *StreamProcessor) SetBPP(bpp int) {
	s.bpp = bpp
}

// BPP returns the current bytes-per-pixel.
func (s *StreamProcessor) BPP() int {
	return s.bpp
}

// ProcessServerData parses a complete WebSocket binary frame from the
// server (QEMU). Returns all parsed messages. If parsing fails partway,
// returns what was parsed plus the remaining unparsed bytes.
//
// The caller should buffer any remaining bytes and prepend them to the
// next frame. In practice, QEMU sends complete RFB messages per WS frame,
// so remainders are rare.
func (s *StreamProcessor) ProcessServerData(data []byte) (msgs []ParsedMessage, remainder []byte, err error) {
	offset := 0
	for offset < len(data) {
		msg, n, parseErr := ParseServerMessage(data[offset:], s.bpp)
		if parseErr != nil {
			if parseErr == ErrIncomplete {
				return msgs, data[offset:], nil
			}
			// Unknown encoding — return remainder for opaque forwarding.
			return msgs, data[offset:], parseErr
		}
		msgs = append(msgs, msg)
		offset += n
	}
	return msgs, nil, nil
}

// ProcessClientData parses a complete WebSocket binary frame from the
// client (renderer).
func (s *StreamProcessor) ProcessClientData(data []byte) (msgs []ParsedMessage, remainder []byte, err error) {
	offset := 0
	for offset < len(data) {
		msg, n, parseErr := ParseClientMessage(data[offset:])
		if parseErr != nil {
			if parseErr == ErrIncomplete {
				return msgs, data[offset:], nil
			}
			return msgs, data[offset:], parseErr
		}

		// Track pixel format changes.
		if msg.Type == MsgSetPixelFormat && len(msg.Raw) >= 20 {
			bitsPerPixel := int(msg.Raw[4])
			s.bpp = bitsPerPixel / 8
			if s.bpp < 1 {
				s.bpp = 1
			}
		}

		msgs = append(msgs, msg)
		offset += n
	}
	return msgs, nil, nil
}
