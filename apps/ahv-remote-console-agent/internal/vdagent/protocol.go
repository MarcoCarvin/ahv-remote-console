// Package vdagent implements a minimal SPICE agent protocol client
// that runs over an ISA serial port (COM2). It bridges the QEMU
// clipboard subsystem to the Windows guest clipboard via the
// qemu-vdagent chardev backend.
//
// The protocol is the same one that spice-vdagent uses over
// virtio-serial, but transported over a standard ISA serial port
// whose chardev backend has been swapped to qemu-vdagent via QMP
// chardev-change. The QEMU vdagent chardev is frontend-agnostic —
// it doesn't care whether bytes arrive from virtio-serial or ISA
// serial.
//
// Wire format (all little-endian):
//
//	VDIChunkHeader (8 bytes):
//	  uint32 port   // VDP_CLIENT_PORT=1 (host→guest), VDP_SERVER_PORT=2 (guest→host)
//	  uint32 size   // payload size following this header
//
//	VDAgentMessage (20+ bytes):
//	  uint32 protocol  // VD_AGENT_PROTOCOL = 1
//	  uint32 type      // message type
//	  uint64 opaque    // unused, set to 0
//	  uint32 size      // variable data length
//	  uint8  data[]    // variable payload
package vdagent

import "encoding/binary"

// Ports
const (
	PortClient uint32 = 1 // host → guest
	PortServer uint32 = 2 // guest → host
)

// Protocol version
const (
	ProtocolVersion uint32 = 1
)

// Message types
const (
	MsgMouseState          uint32 = 1
	MsgMonitorsConfig      uint32 = 2
	MsgReply               uint32 = 3
	MsgClipboard           uint32 = 4
	MsgDisplayConfig       uint32 = 5
	MsgAnnounceCapabilities uint32 = 6
	MsgClipboardGrab       uint32 = 7
	MsgClipboardRequest    uint32 = 8
	MsgClipboardRelease    uint32 = 9
	MsgFileXferStart       uint32 = 11
	MsgFileXferStatus      uint32 = 12
	MsgFileXferData        uint32 = 13
	MsgGraphicsDeviceInfo  uint32 = 20
)

// Clipboard types
const (
	ClipboardNone     uint32 = 0
	ClipboardUTF8Text uint32 = 1
	ClipboardImagePNG uint32 = 2
	ClipboardImageBMP uint32 = 3
)

// Clipboard selection (prepended when CAP_CLIPBOARD_SELECTION is set)
const (
	SelectionClipboard uint8 = 0
	SelectionPrimary   uint8 = 1
	SelectionSecondary uint8 = 2
)

// Capability bits
const (
	CapMouseState         uint32 = 0
	CapMonitorsConfig     uint32 = 1
	CapReply              uint32 = 2
	CapClipboardByDemand   uint32 = 5
	CapClipboardSelection  uint32 = 6
	CapMaxClipboard        uint32 = 10
	CapClipboardGrabSerial uint32 = 17
)

// VDIChunkHeader is the framing header for all messages.
type VDIChunkHeader struct {
	Port uint32
	Size uint32
}

// VDAgentMessage is the message header inside a chunk.
type VDAgentMessage struct {
	Protocol uint32
	Type     uint32
	Opaque   uint64
	Size     uint32
}

const (
	ChunkHeaderSize   = 8
	MessageHeaderSize = 20
	MaxChunkSize      = 1024
	MaxMessageSize    = 16 * 1024 * 1024 // 16 MB hard cap on reassembled messages
	MaxClipboardSize  = 10 * 1024 * 1024 // 10 MB hard cap on clipboard payloads
)

// EncodeChunkedMessage wraps a VDAgentMessage + payload into one or
// more VDI chunks, each at most MaxChunkSize bytes of payload.
func EncodeChunkedMessage(port, msgType uint32, data []byte) []byte {
	msgHeader := VDAgentMessage{
		Protocol: ProtocolVersion,
		Type:     msgType,
		Size:     uint32(len(data)), // #nosec G115 -- outbound payload bounded by MaxMessageSize (16MB), fits uint32
	}

	totalPayload := make([]byte, MessageHeaderSize+len(data))
	binary.LittleEndian.PutUint32(totalPayload[0:4], msgHeader.Protocol)
	binary.LittleEndian.PutUint32(totalPayload[4:8], msgHeader.Type)
	binary.LittleEndian.PutUint64(totalPayload[8:16], msgHeader.Opaque)
	binary.LittleEndian.PutUint32(totalPayload[16:20], msgHeader.Size)
	copy(totalPayload[20:], data)

	var result []byte
	offset := 0
	for offset < len(totalPayload) {
		chunkSize := len(totalPayload) - offset
		if chunkSize > MaxChunkSize {
			chunkSize = MaxChunkSize
		}
		chunk := make([]byte, ChunkHeaderSize+chunkSize)
		binary.LittleEndian.PutUint32(chunk[0:4], port)
		binary.LittleEndian.PutUint32(chunk[4:8], uint32(chunkSize)) // #nosec G115 -- chunkSize <= MaxChunkSize (1024)
		copy(chunk[ChunkHeaderSize:], totalPayload[offset:offset+chunkSize])
		result = append(result, chunk...)
		offset += chunkSize
	}
	return result
}

// BuildCapabilities builds the data payload for an ANNOUNCE_CAPABILITIES
// message. If request is true, the host should respond with its own caps.
func BuildCapabilities(request bool, caps ...uint32) []byte {
	maxBit := uint32(0)
	for _, c := range caps {
		if c > maxBit {
			maxBit = c
		}
	}
	nWords := (maxBit / 32) + 1

	// Payload: uint32 request flag + uint32[] capability bitmask
	data := make([]byte, 4+nWords*4)
	if request {
		binary.LittleEndian.PutUint32(data[0:4], 1)
	}
	for _, c := range caps {
		wordIdx := c / 32
		bitIdx := c % 32
		off := 4 + wordIdx*4
		val := binary.LittleEndian.Uint32(data[off : off+4])
		val |= 1 << bitIdx
		binary.LittleEndian.PutUint32(data[off:off+4], val)
	}
	return data
}

// BuildClipboardGrab builds the data for a CLIPBOARD_GRAB message.
// When CAP_CLIPBOARD_SELECTION is active, the selection byte is prepended.
func BuildClipboardGrab(selection uint8, types []uint32, useSelection bool) []byte {
	offset := 0
	size := len(types) * 4
	if useSelection {
		size += 4 // selection byte padded to 4 bytes
	}
	data := make([]byte, size)
	if useSelection {
		data[0] = selection
		offset = 4
	}
	for i, t := range types {
		binary.LittleEndian.PutUint32(data[offset+i*4:offset+i*4+4], t)
	}
	return data
}

// BuildClipboardData builds the data for a CLIPBOARD (data) message.
func BuildClipboardData(selection uint8, clipType uint32, payload []byte, useSelection bool) []byte {
	offset := 0
	size := 4 + len(payload)
	if useSelection {
		size += 4
	}
	data := make([]byte, size)
	if useSelection {
		data[0] = selection
		offset = 4
	}
	binary.LittleEndian.PutUint32(data[offset:offset+4], clipType)
	copy(data[offset+4:], payload)
	return data
}

// BuildClipboardRequest builds the data for a CLIPBOARD_REQUEST message.
func BuildClipboardRequest(selection uint8, clipType uint32, useSelection bool) []byte {
	offset := 0
	size := 4
	if useSelection {
		size += 4
	}
	data := make([]byte, size)
	if useSelection {
		data[0] = selection
		offset = 4
	}
	binary.LittleEndian.PutUint32(data[offset:offset+4], clipType)
	return data
}
