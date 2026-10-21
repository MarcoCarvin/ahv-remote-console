//go:build windows

package clipboard

import (
	"context"
	"log/slog"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	procAddClipboardFormatListener    = user32.NewProc("AddClipboardFormatListener")
	procRemoveClipboardFormatListener = user32.NewProc("RemoveClipboardFormatListener")
	procCreateWindowExW               = user32.NewProc("CreateWindowExW")
	procGetMessageW                   = user32.NewProc("GetMessageW")
	procDispatchMessageW              = user32.NewProc("DispatchMessageW")
	procPostMessageW                  = user32.NewProc("PostMessageW")
	procDestroyWindow                 = user32.NewProc("DestroyWindow")
)

const (
	wmClipboardUpdate = 0x031D
	wmQuit            = 0x0012
	hwndMessage       = ^uintptr(2) // HWND_MESSAGE = -3
)

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

func (p *windowsProvider) Watch(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		classMessage, _ := syscall.UTF16PtrFromString("Message")
		windowName, _ := syscall.UTF16PtrFromString("ClipWatch")

		hwnd, _, err := procCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(classMessage)),
			uintptr(unsafe.Pointer(windowName)),
			0, 0, 0, 0, 0,
			hwndMessage, 0, 0, 0,
		)

		if hwnd == 0 {
			slog.Warn("clipboard.watch: CreateWindowExW failed, falling back to polling", "err", err)
			fallbackPoll(ctx, p, ch)
			return
		}
		defer procDestroyWindow.Call(hwnd)

		r, _, err := procAddClipboardFormatListener.Call(hwnd)
		if r == 0 {
			slog.Warn("clipboard.watch: AddClipboardFormatListener failed, falling back to polling", "err", err)
			fallbackPoll(ctx, p, ch)
			return
		}
		defer procRemoveClipboardFormatListener.Call(hwnd)

		// Wake up GetMessageW when context is done
		go func() {
			<-ctx.Done()
			procPostMessageW.Call(hwnd, wmQuit, 0, 0)
		}()

		var m msg
		for {
			ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), hwnd, 0, 0)
			if ret == 0 || ret == ^uintptr(0) { // 0 = WM_QUIT, -1 = Error
				return
			}
			if m.Message == wmClipboardUpdate {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()

	return ch
}
