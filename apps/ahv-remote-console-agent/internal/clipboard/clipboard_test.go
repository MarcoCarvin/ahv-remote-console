package clipboard

import (
	"testing"
)

func TestWipe(t *testing.T) {
	b := []byte("super-secret-token")
	Wipe(b)
	for i, v := range b {
		if v != 0 {
			t.Errorf("byte %d not zeroed: %d", i, v)
		}
	}
}

func TestWipe_emptySafe(t *testing.T) {
	// Must not panic.
	Wipe(nil)
	Wipe([]byte{})
}
