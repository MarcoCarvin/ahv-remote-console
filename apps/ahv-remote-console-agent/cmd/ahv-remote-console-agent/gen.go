package main

// Regenerate the Windows resource object (rsrc_windows_amd64.syso) — the
// agent's application icon + version/publisher info — after changing the icon
// or version. The generated .syso is committed and linked automatically by
// `go build`; this only needs re-running when the icon or version changes.
//
//go:generate go run mksyso.go
