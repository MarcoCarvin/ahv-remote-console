//go:build windows

package clipboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows clipboard via the Win32 user32.dll APIs. We avoid CGO to
// keep the binary static-CGO-free (matches the build flags in the
// Makefile). Calls are wrapped in runtime.LockOSThread because the
// Windows clipboard is owned per-thread; if Go's scheduler moves us
// between calls we'd be operating on a different thread's lock.
//
// Memory hygiene:
//
//   - ReadText: we copy the wide-char clipboard into a Go []byte then
//     immediately unlock and close. No persistent buffer.
//   - WriteText: we allocate global memory, lock it, copy in, then
//     SetClipboardData transfers ownership to the OS. We zero our
//     stack copy after the OS takes it.
//
// Reference: docs.microsoft.com/en-us/windows/win32/dataxchg/

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procGetClipboardData = user32.NewProc("GetClipboardData")
	procSetClipboardData = user32.NewProc("SetClipboardData")

	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
	procGlobalFree   = kernel32.NewProc("GlobalFree")
	procGlobalSize   = kernel32.NewProc("GlobalSize")
)

type windowsProvider struct{}

func newProvider() (Provider, error) {
	// No probe required; the user32 entry points are always present
	// on supported Windows versions.
	return &windowsProvider{}, nil
}

func (p *windowsProvider) ReadText(ctx context.Context) ([]byte, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := openClipboardWithBackoff(ctx); err != nil {
		return nil, err
	}
	defer closeClipboard()

	h, _, _ := procGetClipboardData.Call(uintptr(cfUnicodeText))
	if h == 0 {
		return nil, ErrEmpty
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return nil, errors.New("clipboard: GlobalLock failed")
	}
	defer procGlobalUnlock.Call(h)

	// The handle points to a null-terminated UTF-16 string. We walk
	// to the first null to compute length, then copy.
	size, _, _ := procGlobalSize.Call(h)
	maxChars := int(size) / 2
	if maxChars == 0 {
		return nil, ErrEmpty
	}
	wide := unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), maxChars)
	// Find null terminator.
	n := 0
	for n < len(wide) && wide[n] != 0 {
		n++
	}
	if n == 0 {
		return nil, ErrEmpty
	}
	// Diagnostic: log the raw Win32 measurements so we can confirm
	// whether truncation is happening at the Win32 layer (GlobalSize
	// too small, or null at offset N) vs the conversion layer below.
	slog.Debug("clipboard.read: win32",
		"global_size_bytes", uint64(size),
		"max_chars", maxChars,
		"first_null_at", n,
	)
	// UTF-16 → UTF-8.
	out := []byte(syscall.UTF16ToString(wide[:n]))
	if len(out) == 0 {
		return nil, ErrEmpty
	}
	slog.Debug("clipboard.read: result",
		"utf8_bytes", len(out),
		"utf16_chars", n,
	)
	return out, nil
}

func (p *windowsProvider) WriteText(ctx context.Context, text []byte) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := openClipboardWithBackoff(ctx); err != nil {
		return err
	}
	defer closeClipboard()

	if r, _, _ := procEmptyClipboard.Call(); r == 0 {
		return errors.New("clipboard: EmptyClipboard failed")
	}

	// Windows apps expect CRLF line endings on the clipboard; the host sends
	// LF-only (the renderer normalizes line endings). Collapse any existing
	// CRLF/lone-CR to LF first (so we don't double up), then expand LF→CRLF
	// so multi-line pastes render correctly in Notepad/Office/etc. instead of
	// as one run with stray control glyphs.
	s := string(text)
	if strings.ContainsRune(s, '\n') || strings.ContainsRune(s, '\r') {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	wide, err := syscall.UTF16FromString(s)
	if err != nil {
		return fmt.Errorf("clipboard: utf16: %w", err)
	}
	// UTF16FromString appends a trailing null which is what
	// SetClipboardData expects for CF_UNICODETEXT.
	byteLen := len(wide) * 2

	hMem, _, _ := procGlobalAlloc.Call(uintptr(gmemMoveable), uintptr(byteLen))
	if hMem == 0 {
		return errors.New("clipboard: GlobalAlloc failed")
	}
	ptr, _, _ := procGlobalLock.Call(hMem)
	if ptr == 0 {
		procGlobalFree.Call(hMem)
		return errors.New("clipboard: GlobalLock failed")
	}
	// Copy into the locked global memory. We zero the wide buffer
	// afterwards so the only cleartext copy is the OS-owned one.
	dst := unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), len(wide))
	copy(dst, wide)
	procGlobalUnlock.Call(hMem)
	for i := range wide {
		wide[i] = 0
	}

	if r, _, _ := procSetClipboardData.Call(uintptr(cfUnicodeText), hMem); r == 0 {
		procGlobalFree.Call(hMem)
		return errors.New("clipboard: SetClipboardData failed")
	}
	// Ownership now belongs to the OS; do NOT GlobalFree.
	return nil
}

func (p *windowsProvider) Close() error { return nil }

// openClipboardWithBackoff retries OpenClipboard for a bounded period
// because another process (e.g. clipboard manager) may briefly hold
// the lock. ctx bounds the total wait.
func openClipboardWithBackoff(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for {
		r, _, _ := procOpenClipboard.Call(0)
		if r != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("clipboard: OpenClipboard timed out")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func closeClipboard() {
	procCloseClipboard.Call()
}
