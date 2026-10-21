package vdagent

import (
	"encoding/binary"
	"testing"
)

func TestEncodeChunkedMessage(t *testing.T) {
	data := []byte("hello clipboard")
	encoded := EncodeChunkedMessage(PortServer, MsgClipboard, data)

	// Should have one chunk: 8 (chunk hdr) + 20 (msg hdr) + 15 (data) = 43 bytes
	if len(encoded) != 43 {
		t.Fatalf("encoded length = %d, want 43", len(encoded))
	}

	// Chunk header
	port := binary.LittleEndian.Uint32(encoded[0:4])
	size := binary.LittleEndian.Uint32(encoded[4:8])
	if port != PortServer {
		t.Fatalf("port = %d, want %d", port, PortServer)
	}
	if size != 35 { // 20 + 15
		t.Fatalf("chunk size = %d, want 35", size)
	}

	// Message header
	protocol := binary.LittleEndian.Uint32(encoded[8:12])
	msgType := binary.LittleEndian.Uint32(encoded[12:16])
	msgSize := binary.LittleEndian.Uint32(encoded[24:28])
	if protocol != ProtocolVersion {
		t.Fatalf("protocol = %d, want %d", protocol, ProtocolVersion)
	}
	if msgType != MsgClipboard {
		t.Fatalf("msgType = %d, want %d", msgType, MsgClipboard)
	}
	if msgSize != 15 {
		t.Fatalf("msgSize = %d, want 15", msgSize)
	}

	// Payload
	payload := encoded[28:]
	if string(payload) != "hello clipboard" {
		t.Fatalf("payload = %q, want %q", payload, "hello clipboard")
	}
}

func TestEncodeChunkedMessageLarge(t *testing.T) {
	data := make([]byte, MaxChunkSize+500)
	for i := range data {
		data[i] = byte(i % 256)
	}
	encoded := EncodeChunkedMessage(PortServer, MsgClipboard, data)

	// Should have 2 chunks.
	// First chunk: 8 + 1024 bytes
	// Remaining payload: 20 + 1524 - 1024 = 520 bytes
	// Second chunk: 8 + 520 bytes
	expectedLen := (8 + MaxChunkSize) + (8 + (MessageHeaderSize + len(data) - MaxChunkSize))
	if len(encoded) != expectedLen {
		t.Fatalf("encoded length = %d, want %d", len(encoded), expectedLen)
	}

	// First chunk header
	size1 := binary.LittleEndian.Uint32(encoded[4:8])
	if size1 != MaxChunkSize {
		t.Fatalf("chunk1 size = %d, want %d", size1, MaxChunkSize)
	}
}

func TestBuildCapabilities(t *testing.T) {
	caps := BuildCapabilities(true, CapClipboardByDemand, CapClipboardSelection)

	// Request flag
	request := binary.LittleEndian.Uint32(caps[0:4])
	if request != 1 {
		t.Fatalf("request = %d, want 1", request)
	}

	// CapClipboardByDemand = bit 5, CapClipboardSelection = bit 6
	word0 := binary.LittleEndian.Uint32(caps[4:8])
	if word0&(1<<5) == 0 {
		t.Fatal("CapClipboardByDemand not set")
	}
	if word0&(1<<6) == 0 {
		t.Fatal("CapClipboardSelection not set")
	}
}

func TestBuildClipboardGrab(t *testing.T) {
	data := BuildClipboardGrab(SelectionClipboard, []uint32{ClipboardUTF8Text}, true)

	// With selection: 4 bytes selection + 4 bytes type = 8 bytes
	if len(data) != 8 {
		t.Fatalf("len = %d, want 8", len(data))
	}
	if data[0] != SelectionClipboard {
		t.Fatalf("selection = %d, want %d", data[0], SelectionClipboard)
	}
	clipType := binary.LittleEndian.Uint32(data[4:8])
	if clipType != ClipboardUTF8Text {
		t.Fatalf("type = %d, want %d", clipType, ClipboardUTF8Text)
	}
}

func TestBuildClipboardData(t *testing.T) {
	payload := []byte("test text")
	data := BuildClipboardData(SelectionClipboard, ClipboardUTF8Text, payload, true)

	// 4 (selection) + 4 (type) + 9 (payload) = 17
	if len(data) != 17 {
		t.Fatalf("len = %d, want 17", len(data))
	}
	if data[0] != SelectionClipboard {
		t.Fatalf("selection = %d, want %d", data[0], SelectionClipboard)
	}
	clipType := binary.LittleEndian.Uint32(data[4:8])
	if clipType != ClipboardUTF8Text {
		t.Fatalf("type = %d, want %d", clipType, ClipboardUTF8Text)
	}
	if string(data[8:]) != "test text" {
		t.Fatalf("payload = %q, want %q", data[8:], "test text")
	}
}

func TestBuildClipboardRequest(t *testing.T) {
	data := BuildClipboardRequest(SelectionClipboard, ClipboardUTF8Text, false)

	// Without selection: just 4 bytes type
	if len(data) != 4 {
		t.Fatalf("len = %d, want 4", len(data))
	}
	clipType := binary.LittleEndian.Uint32(data[0:4])
	if clipType != ClipboardUTF8Text {
		t.Fatalf("type = %d, want %d", clipType, ClipboardUTF8Text)
	}
}
