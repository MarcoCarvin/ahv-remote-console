// Package clipboard reads and writes the guest OS clipboard from the
// AHV Remote Console Agent. It is intentionally minimal — text only, no
// MIME negotiation — because the agent's job is bridging guest
// text to the operator's Mac pasteboard, not implementing a full
// clipboard manager.
//
// All implementations zero in-memory cleartext as soon as the caller
// returns. Persistent storage is forbidden; an operator copying a
// password into the guest should not find that password in any file
// the agent ever wrote to disk.
//
// Platform support
//
//	linux    via xclip/xsel (X11) or wl-copy/wl-paste (Wayland)
//	windows  via user32 GetClipboardData / SetClipboardData
//	darwin   not supported — Mac guests are out of scope for v1
//
// Each platform has its own *_linux.go / *_windows.go file. The
// shared file in this package only declares the Provider interface
// and the constructor New() that selects an implementation based on
// runtime.GOOS.
package clipboard

import (
	"context"
	"errors"
)

// ErrUnsupported is returned by New on platforms we don't ship a
// clipboard implementation for. The caller should refuse to start in
// side-channel mode rather than silently no-op every clipboard
// operation.
var ErrUnsupported = errors.New("clipboard: unsupported platform")

// ErrEmpty is returned by ReadText when the guest's clipboard is
// empty or contains a non-text payload (image, file list, etc.). The
// agent treats this as "nothing to ship" and stays quiet.
var ErrEmpty = errors.New("clipboard: empty or non-text")

// Provider is the interface a platform's clipboard implementation
// satisfies. Methods take a Context so the caller can bound how long
// they're willing to wait for an OS API (some Linux helpers can hang
// when the display server is unresponsive).
type Provider interface {
	// ReadText returns the current clipboard text. Empty or non-text
	// clipboards return ErrEmpty. The returned byte slice is the
	// caller's to wipe — implementations have already zeroed any
	// temporary copies they made.
	ReadText(ctx context.Context) ([]byte, error)

	// WriteText sets the clipboard to the given text. The input slice
	// may contain a password or other sensitive payload; the
	// implementation must not write it to disk and must zero any
	// temporary copies before returning.
	WriteText(ctx context.Context, text []byte) error

	// Watch returns a channel that emits an event whenever the guest
	// OS clipboard changes. On platforms where event-driven
	// notifications aren't natively supported, this falls back to a
	// polling loop.
	Watch(ctx context.Context) <-chan struct{}

	// Close releases any OS handles or background subprocesses the
	// provider opened. After Close, every method returns an error.
	Close() error
}

// Wipe overwrites a credential-bearing slice with zeros. Helper so
// callers don't have to repeat the loop everywhere.
func Wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// New returns the Provider implementation for the current platform.
// Linux requires xclip / xsel / wl-copy + wl-paste; if none are
// present, returns an error and the caller should refuse to run.
func New() (Provider, error) {
	return newProvider()
}
