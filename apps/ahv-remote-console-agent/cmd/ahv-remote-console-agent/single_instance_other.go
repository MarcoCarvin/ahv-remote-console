//go:build !windows && !linux

// Single-instance guard stub for non-Windows platforms.
//
// The bridge daemon is Windows-only in v1 (see install_other.go for
// why), so the guard is a no-op on Linux/macOS. When the Linux build
// lands it will use an abstract-namespace AF_UNIX socket bound to
// `\0AHVRemoteConsoleBridge-<uid>` for the same per-user single-instance
// semantics — same property as the Windows named mutex.

package main

import "errors"

// ErrBridgeAlreadyRunning is the platform-neutral collision sentinel.
// The Windows build returns this from acquireSingleInstance when the
// per-user mutex is already owned; the non-Windows stub never does,
// but the symbol is declared here so the caller in main.go compiles
// on every platform.
var ErrBridgeAlreadyRunning = errors.New("hcbridge: another instance is already running for this user")

// singleInstance is a zero-cost placeholder on non-Windows builds.
type singleInstance struct{}

// acquireSingleInstance is a no-op on non-Windows: there's nothing to
// acquire because the bridge daemon isn't installed on these
// platforms anyway. Returns a zero-value handle so the caller's
// defer s.release() call is harmless.
func acquireSingleInstance() (*singleInstance, error) {
	return &singleInstance{}, nil
}

// release is a no-op on non-Windows builds.
func (s *singleInstance) release() {}
