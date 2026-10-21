//go:build linux

// Per-user single-instance guard for the bridge daemon on Linux.
//
// Uses flock(2) on a well-known lock file. flock is automatically
// released by the kernel when the process exits (even on crash), so
// there are no stale-lock issues — same property as the Windows named
// mutex. The lock file lives under $XDG_RUNTIME_DIR (tmpfs, per-user,
// per-session on modern systemd distros) or falls back to /tmp with
// a UID suffix.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var ErrBridgeAlreadyRunning = errors.New("hcbridge: another instance is already running for this user")

type singleInstance struct {
	f *os.File
}

func acquireSingleInstance() (*singleInstance, error) {
	path := lockPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("lock dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrBridgeAlreadyRunning
		}
		return nil, fmt.Errorf("flock: %w", err)
	}

	return &singleInstance{f: f}, nil
}

func (s *singleInstance) release() {
	if s == nil || s.f == nil {
		return
	}
	_ = unix.Flock(int(s.f.Fd()), unix.LOCK_UN)
	s.f.Close()
	s.f = nil
}

func lockPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "hcbridge.lock")
	}
	return fmt.Sprintf("/tmp/hcbridge-%d.lock", os.Getuid())
}
