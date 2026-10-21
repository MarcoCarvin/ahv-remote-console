//go:build linux

package main

import (
	"os"
	"path/filepath"
)

func logFilePath() string {
	// XDG_DATA_HOME, defaulting to ~/.local/share per the XDG spec.
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return ""
		}
		dir = filepath.Join(home, ".local", "share")
	}
	dir = filepath.Join(dir, "ahv-remote-console")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "hcbridge.log")
}
