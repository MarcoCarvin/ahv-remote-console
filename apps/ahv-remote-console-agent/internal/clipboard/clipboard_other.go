//go:build !linux && !windows

package clipboard

// Mac and other unsupported guests fall through to this stub. v1 of
// AHV Remote Console Agent does not support Mac guests; the binary
// won't even cross-compile for darwin (the agent-all-platforms
// Makefile target omits it), so this file exists to keep the
// package-level New() callable for `go vet` / `go build` runs on
// unusual platforms.
func newProvider() (Provider, error) {
	return nil, ErrUnsupported
}
