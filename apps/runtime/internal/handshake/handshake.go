// Package handshake serializes the runtime's loopback endpoint and the
// integrity hash of the IPC secret so the Electron shell can locate the
// runtime after the runtime auto-binds an ephemeral port. The file is created
// with 0600 permissions and removed on graceful shutdown.
package handshake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type Handshake struct {
	Version    string    `json:"version"`
	PID        int       `json:"pid"`
	Endpoint   string    `json:"endpoint"`
	SecretHash string    `json:"secret_hash"` // base64(sha256(secret)) — used by renderer to verify it has the right secret.
	StartedAt  time.Time `json:"started_at"`
}

func Write(path string, h Handshake) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	// Clear any stale temp from a prior crash, then create EXCLUSIVELY.
	// O_CREATE|O_EXCL refuses to open when the path already exists — including
	// a symlink a local attacker may have planted at <path>.tmp to redirect
	// our write. Without this, os.WriteFile would follow such a symlink and
	// write the handshake (which advertises the loopback IPC endpoint) to an
	// attacker-chosen location, or let an attacker pre-seed a fake handshake.
	// This is the portable equivalent of O_NOFOLLOW (syscall.O_NOFOLLOW is not
	// defined on Windows; the runtime is cross-platform).
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, werr := f.Write(data); werr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return werr
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// os.Rename does NOT update the destination's mode if it already existed
	// with looser permissions; explicitly chmod so the handshake is always
	// 0600.
	return os.Chmod(path, 0o600)
}

func Remove(path string) {
	_ = os.Remove(path)
}
