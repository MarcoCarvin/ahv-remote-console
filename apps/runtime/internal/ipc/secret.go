package ipc

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"sync"
)

// Secret holds an in-memory IPC HMAC secret. The secret is never logged and is
// zeroized via Wipe(). All comparisons must use constant-time semantics.
type Secret struct {
	mu    sync.RWMutex
	bytes []byte
}

func NewSecret(b []byte) *Secret {
	cp := make([]byte, len(b))
	copy(cp, b)
	return &Secret{bytes: cp}
}

func (s *Secret) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.bytes)
}

// MAC computes HMAC-SHA256(secret, msg).
func (s *Secret) MAC(msg []byte) []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := hmac.New(sha256.New, s.bytes)
	_, _ = h.Write(msg)
	return h.Sum(nil)
}

// ConstantTimeEqual compares two HMACs without leaking timing info.
func ConstantTimeEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}

// HashB64 returns base64(sha256(secret)). Used by the handshake file so the
// renderer can verify it has the right secret without ever seeing the secret
// itself.
func (s *Secret) HashB64() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := sha256.Sum256(s.bytes)
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// Wipe zeroizes the secret's backing storage. Safe to call multiple times.
func (s *Secret) Wipe() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.bytes {
		s.bytes[i] = 0
	}
	s.bytes = nil
}
