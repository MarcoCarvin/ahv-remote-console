//go:build windows

package main

import (
	"os"
)

// procAttachConsole (kernel32!AttachConsole). modKernel32 is declared in
// gui_windows.go and reused here.
var procAttachConsole = modKernel32.NewProc("AttachConsole")

// attachParentConsole attaches this GUI-subsystem process to the console of the
// process that launched it (cmd.exe / PowerShell) and re-points os.Stdout and
// os.Stderr at it, so the documented CLI modes' fmt output is actually visible.
//
// A binary linked with -H windowsgui starts with NO console and invalid std
// handles, so --version/--install/--uninstall would otherwise print into the
// void and look like a silent no-op. We only call this for those CLI modes.
//
// No-op when there is no parent console to attach to (Explorer double-click of
// the installer; the detached HKCU\Run daemon): AttachConsole returns 0 and we
// leave the existing handles untouched. fmt.Println reads os.Stdout at call
// time, so reassigning the package vars here is sufficient.
func attachParentConsole() {
	const attachParentProcess = uintptr(0xffffffff) // ATTACH_PARENT_PROCESS = (DWORD)-1
	r, _, _ := procAttachConsole.Call(attachParentProcess)
	if r == 0 {
		return // no parent console — nothing to redirect onto
	}
	if out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = out
		os.Stderr = out
	}
}
