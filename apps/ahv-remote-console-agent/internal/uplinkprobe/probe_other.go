//go:build !windows

// uplinkprobe — 5-variant uplink viability experiment.
//
// On non-Windows builds this is a stub that fails loudly so the
// caller (main.go's --uplink-probe flag) doesn't silently do
// nothing when run on the operator's host. The probe only has
// meaning inside the guest VM.

package uplinkprobe

import (
	"context"
	"errors"
	"log/slog"
)

// ErrNotImplemented is returned by Run on platforms where the
// probe has no meaning (i.e., any host that isn't the Windows
// guest VM the bridge daemon installs into).
var ErrNotImplemented = errors.New("uplinkprobe: only implemented on Windows")

// Run is the cross-platform entry point. Calls into the Windows
// implementation when present; returns ErrNotImplemented otherwise.
func Run(_ context.Context, _ *slog.Logger) error {
	return ErrNotImplemented
}
