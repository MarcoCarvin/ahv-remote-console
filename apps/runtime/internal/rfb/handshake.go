package rfb

import "encoding/binary"

// HandshakeState tracks the RFB handshake progress so the proxy knows
// when the stream transitions from handshake to normal message flow.
//
// RFB 3.8 handshake sequence (relevant subset for VNC-over-WebSocket
// with security type "None" as used by AHV/Prism):
//
//	Server → Client: ProtocolVersion  "RFB 003.008\n" (12 bytes)
//	Client → Server: ProtocolVersion  "RFB 003.008\n" (12 bytes)
//	Server → Client: SecurityTypes    1 byte count + N bytes types
//	Client → Server: SecurityType     1 byte (chosen type)
//	Server → Client: SecurityResult   4 bytes (0 = OK)
//	Client → Server: ClientInit       1 byte (shared flag)
//	Server → Client: ServerInit       24 + nameLen bytes
//	         ↓ normal RFB messages start here ↓
type HandshakeState int

const (
	HsServerVersion HandshakeState = iota
	HsClientVersion
	HsSecurityTypes
	HsSecurityChoice
	HsSecurityResult
	HsClientInit
	HsServerInit
	HsDone
	// HsError is a terminal state entered on an auth/security failure path
	// (SecurityResult != 0, SecurityTypes count==0, or an unsupported security
	// type chosen). Unlike HsDone it does NOT mean the stream is ready for
	// normal RFB messages — it means the handshake aborted and the connection
	// must be torn down. Failing() reports it; Done() stays false so the proxy
	// never advances a desynced stream into the message-flow phase.
	HsError
)

// ServerInit holds the parsed ServerInit message fields.
type ServerInit struct {
	Width, Height uint16
	BPP           uint8
	Depth         uint8
	BigEndian     bool
	TrueColour    bool
	RedMax        uint16
	GreenMax      uint16
	BlueMax       uint16
	RedShift      uint8
	GreenShift    uint8
	BlueShift     uint8
	Name          string
}

// HandshakeTracker follows the handshake and extracts ServerInit.
type HandshakeTracker struct {
	State      HandshakeState
	ServerInfo *ServerInit

	// ErrReason carries the server-supplied failure text when State == HsError
	// (RFB 3.8 sends a reason string on SecurityTypes count==0 and on a
	// non-zero SecurityResult). Empty if the failure carried no reason.
	ErrReason string

	securityTypeCount int
	// offeredTypes records the security types the server advertised in
	// SecurityTypes, so HsSecurityChoice can validate the client's chosen type
	// against what was actually offered (PROT-04).
	offeredTypes []byte
}

// Reset returns the tracker to its initial state so a fresh RFB handshake can
// be tracked from the start. The proxy calls this on upstream reconnect (a new
// VNC connection always begins with the handshake); without it, Done() stays
// true from the first connect and the post-reconnect handshake bytes are
// mis-handled as normal RFB messages, desyncing the whole stream.
func (h *HandshakeTracker) Reset() {
	*h = HandshakeTracker{}
}

// FeedServer processes bytes from server→client during handshake.
// Returns number of bytes consumed. When State reaches HsDone,
// ServerInfo is populated and all subsequent bytes are normal messages.
func (h *HandshakeTracker) FeedServer(data []byte) int {
	consumed := 0

	for consumed < len(data) {
		remaining := data[consumed:]
		switch h.State {
		case HsServerVersion:
			if len(remaining) < 12 {
				return consumed
			}
			consumed += 12
			h.State = HsClientVersion

		case HsSecurityTypes:
			if len(remaining) < 1 {
				return consumed
			}
			count := int(remaining[0])
			if count == 0 {
				// Count==0 is the RFB 3.8 connection-failed path: the server
				// follows it with a (4-byte length + string) reason and then
				// closes. Consume the WHOLE reason before transitioning so a
				// caller that keeps reading doesn't mis-frame the trailing bytes
				// as normal messages, and enter the terminal HsError state rather
				// than HsDone (the stream is dead, not ready). If the reason isn't
				// fully buffered yet, leave the count byte unconsumed and wait for
				// more bytes (re-enter this case next call).
				if len(remaining) < 5 {
					return consumed
				}
				// int64 math, then a single bound against len(remaining): a u32
				// reasonLen near 0xFFFFFFFF would overflow `5 + int(reasonLen)`
				// negative on 32-bit int (windows/386) and bypass the guard,
				// panicking on the slice below — the same class of overflow the
				// parser was hardened against.
				reasonLen := int64(binary.BigEndian.Uint32(remaining[1:5]))
				total := 5 + reasonLen
				if int64(len(remaining)) < total {
					return consumed
				}
				if reasonLen > 0 {
					h.ErrReason = string(remaining[5:total])
				}
				consumed += int(total)
				h.State = HsError
				return consumed
			}
			consumed++
			h.securityTypeCount = count
			if len(remaining)-1 < count {
				return consumed
			}
			h.offeredTypes = append(h.offeredTypes[:0], remaining[1:1+count]...)
			consumed += count
			h.State = HsSecurityChoice

		case HsSecurityResult:
			if len(remaining) < 4 {
				return consumed
			}
			result := binary.BigEndian.Uint32(remaining[0:4])
			if result != 0 {
				// Authentication FAILED. RFB 3.8 follows a non-zero result with a
				// (4-byte length + string) reason, then closes. Consume the whole
				// reason and enter the terminal HsError state — the previous code
				// silently advanced to HsClientInit and desynced the stream (the
				// 4-byte length + reason bytes were then mis-parsed as a ClientInit
				// + ServerInit). Wait for the full reason before transitioning.
				if len(remaining) < 8 {
					return consumed
				}
				// int64 math + single len-bound — see the count==0 path: guards
				// against a u32 reasonLen overflowing 32-bit int.
				reasonLen := int64(binary.BigEndian.Uint32(remaining[4:8]))
				total := 8 + reasonLen
				if int64(len(remaining)) < total {
					return consumed
				}
				if reasonLen > 0 {
					h.ErrReason = string(remaining[8:total])
				}
				consumed += int(total)
				h.State = HsError
				return consumed
			}
			consumed += 4
			h.State = HsClientInit

		case HsServerInit:
			if len(remaining) < 24 {
				return consumed
			}
			nameLen := int(binary.BigEndian.Uint32(remaining[20:24]))
			total := 24 + nameLen
			if len(remaining) < total {
				return consumed
			}

			h.ServerInfo = &ServerInit{
				Width:      binary.BigEndian.Uint16(remaining[0:2]),
				Height:     binary.BigEndian.Uint16(remaining[2:4]),
				BPP:        remaining[4],
				Depth:      remaining[5],
				BigEndian:  remaining[6] != 0,
				TrueColour: remaining[7] != 0,
				RedMax:     binary.BigEndian.Uint16(remaining[8:10]),
				GreenMax:   binary.BigEndian.Uint16(remaining[10:12]),
				BlueMax:    binary.BigEndian.Uint16(remaining[12:14]),
				RedShift:   remaining[14],
				GreenShift: remaining[15],
				BlueShift:  remaining[16],
			}
			if nameLen > 0 {
				h.ServerInfo.Name = string(remaining[24 : 24+nameLen])
			}

			consumed += total
			h.State = HsDone
			return consumed

		default:
			// HsClientVersion, HsClientInit, HsSecurityChoice are client→server;
			// they shouldn't appear in server data. HsDone means we're past handshake.
			return consumed
		}
	}

	return consumed
}

// FeedClient processes bytes from client→server during handshake.
// Returns number of bytes consumed.
func (h *HandshakeTracker) FeedClient(data []byte) int {
	consumed := 0

	for consumed < len(data) {
		remaining := data[consumed:]
		switch h.State {
		case HsClientVersion:
			if len(remaining) < 12 {
				return consumed
			}
			consumed += 12
			h.State = HsSecurityTypes

		case HsSecurityChoice:
			if len(remaining) < 1 {
				return consumed
			}
			secType := remaining[0]
			consumed++
			// AHV/Prism always authenticates via the WS token and offers ONLY
			// security type 1 (None). Hard-restrict to it (Simplicity-First):
			// anything else (e.g. VNC challenge-response, type 2) is unexpected
			// and we have no handler for the extra auth bytes it would inject —
			// forwarding it blindly desyncs the stream. Reject by entering the
			// terminal HsError state. When the offered list is tracked, also
			// reject a chosen type the server never advertised.
			if secType != 1 || !h.offeredType(secType) {
				h.ErrReason = "unsupported security type"
				h.State = HsError
				return consumed
			}
			h.State = HsSecurityResult

		case HsClientInit:
			if len(remaining) < 1 {
				return consumed
			}
			consumed++
			h.State = HsServerInit

		default:
			return consumed
		}
	}

	return consumed
}

// offeredType reports whether t was among the security types the server
// advertised in SecurityTypes. If we never tracked an offered list (e.g. the
// SecurityChoice was fed without a preceding SecurityTypes in a test), it is
// permissive and returns true so only the type-1 restriction applies.
func (h *HandshakeTracker) offeredType(t byte) bool {
	if len(h.offeredTypes) == 0 {
		return true
	}
	for _, o := range h.offeredTypes {
		if o == t {
			return true
		}
	}
	return false
}

// Done returns true when the handshake is complete.
func (h *HandshakeTracker) Done() bool {
	return h.State == HsDone
}

// Failing returns true when the handshake reached the terminal HsError state
// (auth/security failure). The proxy treats this distinctly from Done(): the
// stream is dead and must be torn down, not advanced into normal message flow.
func (h *HandshakeTracker) Failing() bool {
	return h.State == HsError
}
