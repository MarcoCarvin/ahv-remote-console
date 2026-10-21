//go:build !windows && !linux

package vdagent

import (
	"fmt"
	"io"
)

// OpenSerial is a stub for non-Windows platforms.
func OpenSerial(name string) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("serial port not supported on this platform")
}
