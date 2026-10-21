//go:build windows

// Per-user single-instance guard for the bridge daemon.
//
// Background: bridge state (Win32 keyboard hook, clipboard
// watcher, in-band transport) assumes exactly one process per
// Windows user session. Two simultaneous bridges on the same user
// would race for the foreground keyboard hook, double-deliver
// clipboard updates, and confuse the renderer's session-keyed
// envelope verification.
//
// Why a named mutex (and not e.g. a PID lockfile):
//
//   - Mutex ownership is released by the OS even if the process
//     crashes — no stale-lock cleanup needed.
//   - The `Local\` namespace is per-logon-session, so each Windows
//     user (and each RDP/RemoteApp session) gets its own scope. A
//     hostile second user on the same VM has its own mutex
//     namespace and won't see ours, so they can't lock us out;
//     simultaneously, a normal "user double-clicked the
//     installer twice" race resolves with the loser detecting
//     ERROR_ALREADY_EXISTS and exiting cleanly.

package main

import (
	"errors"
	"fmt"
	"os/user"
	"strings"
	"syscall"
	"unsafe"
)

// ErrBridgeAlreadyRunning is returned by acquireSingleInstance when
// another bridge process is already holding the per-user mutex.
// run() treats this as a soft exit (no error to the user) so the
// HKCU\Run auto-start path doesn't pop an error dialog when the
// operator manually launches a second copy.
var ErrBridgeAlreadyRunning = errors.New("hcbridge: another instance is already running for this user")

const (
	// errorAlreadyExists is ERROR_ALREADY_EXISTS from winerror.h.
	// CreateMutexW returns a valid handle and sets last-error to this
	// value when the named mutex already exists.
	errorAlreadyExists = 183
)

var (
	modKernel32sg    = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutexW = modKernel32sg.NewProc("CreateMutexW")
	procCloseHandle  = modKernel32sg.NewProc("CloseHandle")
)

// singleInstance holds the Windows mutex handle. The handle is closed
// when release() is called (typically on process exit), at which
// point another bridge can start.
type singleInstance struct {
	handle uintptr
}

// acquireSingleInstance attempts to claim the per-user named mutex.
// On success returns a singleInstance whose Close() releases the
// handle. On collision returns ErrBridgeAlreadyRunning so the caller
// can print a friendly message and exit cleanly.
//
// The mutex name is `Local\AHVRemoteConsoleBridge-<id>` where <id> is
// the user's SID (preferred — uniquely identifies a Windows account
// across machines) or the username (fallback if SID lookup fails).
// `Local\` confines the namespace to the current logon session, which
// is exactly the granularity we want: per-user, per-RDP-session.
func acquireSingleInstance() (*singleInstance, error) {
	name := mutexNameForCurrentUser()
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("hcbridge: utf16 mutex name: %w", err)
	}
	// CreateMutexW(NULL, FALSE, name) — no security attrs, don't claim
	// initial ownership (we only need the existence check; ownership
	// would gate Wait calls we never make).
	handle, _, callErr := procCreateMutexW.Call(
		0,
		0,
		uintptr(unsafe.Pointer(namePtr)),
	)
	// CreateMutexW returns NULL on real failure. callErr is the
	// last-error value regardless (Windows convention).
	if handle == 0 {
		return nil, fmt.Errorf("hcbridge: CreateMutexW failed: %w", callErr)
	}
	if errno, ok := callErr.(syscall.Errno); ok && uintptr(errno) == errorAlreadyExists {
		// Another process owns the mutex. Close our duplicate handle
		// so we don't leak a kernel object, then signal the caller.
		_, _, _ = procCloseHandle.Call(handle)
		return nil, ErrBridgeAlreadyRunning
	}
	return &singleInstance{handle: handle}, nil
}

// release closes the mutex handle so the OS can reclaim it (and so a
// future bridge instance can take ownership). Safe to call multiple
// times; subsequent calls are no-ops.
func (s *singleInstance) release() {
	if s == nil || s.handle == 0 {
		return
	}
	_, _, _ = procCloseHandle.Call(s.handle)
	s.handle = 0
}

// mutexNameForCurrentUser builds the per-user mutex name. Tries the
// SID first (stable, unique); falls back to the username if the user
// lookup fails (e.g. on a stripped-down Server Core where there's no
// USERNAME variable and user.Current errors). The "ahv-remote-console-fallback"
// suffix on the fallback makes the unlikely "no user info at all" case
// fail closed: every such bridge would share the same name and the
// later one would detect the collision.
func mutexNameForCurrentUser() string {
	if u, err := user.Current(); err == nil {
		if u.Uid != "" {
			return `Local\AHVRemoteConsoleBridge-` + sanitizeMutexComponent(u.Uid)
		}
		if u.Username != "" {
			return `Local\AHVRemoteConsoleBridge-` + sanitizeMutexComponent(u.Username)
		}
	}
	return `Local\AHVRemoteConsoleBridge-ahv-remote-console-fallback`
}

// sanitizeMutexComponent replaces characters that are illegal in Win32
// kernel-object names (notably backslash, which would change the
// namespace) with a safe placeholder. Win32 names can otherwise be
// almost any character up to 260 chars, so this is intentionally
// conservative.
func sanitizeMutexComponent(s string) string {
	return strings.NewReplacer(
		`\`, "_",
		"/", "_",
	).Replace(s)
}
