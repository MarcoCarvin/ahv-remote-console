package ipc

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestSecretMACMatchesStdlib(t *testing.T) {
	raw := []byte("test-secret-of-reasonable-length-32B")
	s := NewSecret(raw)
	defer s.Wipe()

	msg := []byte("GET\n/v1/plugins\n12345\nabcd\nfedcba98")
	got := s.MAC(msg)

	h := hmac.New(sha256.New, raw)
	_, _ = h.Write(msg)
	want := h.Sum(nil)

	if !ConstantTimeEqual(got, want) {
		t.Fatalf("HMAC mismatch:\n got=%x\nwant=%x", got, want)
	}
}

func TestSecretHashB64(t *testing.T) {
	raw := []byte("hello-world-secret")
	s := NewSecret(raw)
	defer s.Wipe()
	got := s.HashB64()
	sum := sha256.Sum256(raw)
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("HashB64 mismatch: got %q want %q", got, want)
	}
}

func TestSecretWipeZeroes(t *testing.T) {
	raw := []byte("secret-to-wipe")
	s := NewSecret(raw)
	s.Wipe()
	if s.Len() != 0 {
		t.Fatalf("Wipe did not reset length: %d", s.Len())
	}
}

func TestConstantTimeEqualLengthMismatch(t *testing.T) {
	if ConstantTimeEqual([]byte{1, 2, 3}, []byte{1, 2}) {
		t.Fatal("expected false for length-mismatched buffers")
	}
}
