//go:build tools

// tools.go pins the build-time-only dependency used by mksyso.go (the Windows
// resource generator) so `go mod tidy` keeps it in go.mod, even though
// mksyso.go itself is //go:build ignore. It is never compiled into any shipped
// binary (the `tools` tag is never set by a normal build).
package main

import _ "github.com/tc-hib/winres"
