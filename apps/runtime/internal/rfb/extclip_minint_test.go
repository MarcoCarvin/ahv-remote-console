package rfb

import (
	"encoding/binary"
	"math"
	"testing"
)

// A ServerCutText whose length field is exactly math.MinInt32 must not panic
// any of the Extended-Clipboard helpers: -length overflows back to MinInt32
// and int(-length) is negative, so an unguarded len/slice check would index
// out of bounds and crash the proxy goroutine (= clipboard channel dead).
func TestExtClipMinInt32LengthIsRejectedWithoutPanic(t *testing.T) {
	data := make([]byte, 16)
	data[0] = MsgServerCutText
	var minLen int32 = math.MinInt32 // via a var so the uint32 cast is a runtime conversion
	binary.BigEndian.PutUint32(data[4:8], uint32(minLen))
	// Fill a plausible flags word so the helpers reach the length math.
	binary.BigEndian.PutUint32(data[8:12], ExtClipProvide|ExtClipText)

	if got := TranslateExtClipServerCutText(data); got != nil {
		t.Fatalf("TranslateExtClipServerCutText: want nil for MinInt32 length, got %d bytes", len(got))
	}
	if IsExtClipNotify(data) {
		t.Fatal("IsExtClipNotify: want false for MinInt32 length")
	}
	if IsExtClipProvide(data) {
		t.Fatal("IsExtClipProvide: want false for MinInt32 length")
	}
}
