//go:build windows

package main

import "os"

func logFilePath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return ""
	}
	return base + `\AHV Remote Console\hcbridge.log`
}
