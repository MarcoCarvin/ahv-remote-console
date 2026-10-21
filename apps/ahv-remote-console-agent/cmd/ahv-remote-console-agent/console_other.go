//go:build !windows

package main

// attachParentConsole is a no-op off Windows: those builds are console-subsystem
// already, so stdout/stderr from the CLI modes reach the launching shell.
func attachParentConsole() {}
