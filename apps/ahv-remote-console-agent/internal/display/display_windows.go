//go:build windows

// display_windows.go is the Win32 implementation. EnumerateModes
// walks every mode the active display driver advertises via
// EnumDisplaySettingsExW; and ApplyMode runs PickBestMode then commits
// the result via ChangeDisplaySettingsExW(CDS_UPDATEREGISTRY).
//
// We pass lpszDeviceName = NULL to all Win32 calls, which means
// "the primary display device" in Windows lingo — that's exactly what
// the bridge wants to drive, since the renderer's resize gesture is
// always about the console's framebuffer (the primary monitor inside
// the guest VM).
//
// The picker is in display.go so the best-fit logic is testable from
// any developer platform; only this file pulls in syscall + the
// DEVMODEW ABI.

package display

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// Win32 constants. Names match the Microsoft C #defines so cross-
// referencing MSDN / wingdi.h is straightforward.
const (
	enumRegistrySettings uint32 = 0xFFFFFFFE
	// enumCurrentSettings is ENUM_CURRENT_SETTINGS — asks EnumDisplaySettingsExW
	// for the mode the display is using RIGHT NOW (not an enumerable candidate).
	enumCurrentSettings uint32 = 0xFFFFFFFF

	// CDS_* flags for ChangeDisplaySettingsExW.
	cdsUpdateRegistry uint32 = 0x00000001
	cdsGlobal         uint32 = 0x00000008

	// DISP_CHANGE_* return codes from ChangeDisplaySettingsExW.
	dispChangeSuccessful int32 = 0
	dispChangeRestart    int32 = 1

	// DM_* "which fields are valid" mask used in DEVMODEW.dmFields.
	dmBitsPerPel       uint32 = 0x00040000
	dmPelsWidth        uint32 = 0x00080000
	dmPelsHeight       uint32 = 0x00100000
	dmDisplayFrequency uint32 = 0x00400000

	// CCHDEVICENAME is the fixed-length WCHAR buffer for
	// DEVMODEW.dmDeviceName.
	cchDeviceName = 32
	// CCHFORMNAME is the fixed-length WCHAR buffer for
	// DEVMODEW.dmFormName.
	cchFormName = 32
)

// devmodeW mirrors Win32 DEVMODEW. We use the documented post-Vista
// layout where the union slot between dmDriverExtra and dmColor holds
// dmPosition + dmDisplayOrientation + dmDisplayFixedOutput (16 bytes
// total) for DISPLAY devices. Only the fields we read/write
// (dmFields, dmPelsWidth, dmPelsHeight, dmBitsPerPel,
// dmDisplayFrequency) are touched; the rest must be present so the
// struct's size matches the driver's expectation. Total: 220 bytes
// on 64-bit Windows.
//
// Field order MUST match <wingdi.h>; getting the offset of dmFields
// wrong silently corrupts the mode-set call.
type devmodeW struct {
	DmDeviceName    [cchDeviceName]uint16
	DmSpecVersion   uint16
	DmDriverVersion uint16
	DmSize          uint16
	DmDriverExtra   uint16
	DmFields        uint32
	// Union block: in DISPLAY mode this is
	//   POINTL dmPosition;
	//   DWORD  dmDisplayOrientation;
	//   DWORD  dmDisplayFixedOutput;
	// — 16 bytes total. In PRINTER mode it's a different layout, but
	// we never call EnumDisplaySettingsExW on a printer.
	DmPositionX          int32
	DmPositionY          int32
	DmDisplayOrientation uint32
	DmDisplayFixedOutput uint32

	DmColor       int16
	DmDuplex      int16
	DmYResolution int16
	DmTTOption    int16
	DmCollate     int16
	DmFormName    [cchFormName]uint16
	DmLogPixels   uint16
	DmBitsPerPel  uint32
	DmPelsWidth   uint32
	DmPelsHeight  uint32

	// In DISPLAY mode the next union slot is dmDisplayFlags; we don't
	// touch it but it must occupy the right offset so
	// dmDisplayFrequency lines up.
	DmDisplayFlags uint32

	DmDisplayFrequency uint32
	DmICMMethod        uint32
	DmICMIntent        uint32
	DmMediaType        uint32
	DmDitherType       uint32
	DmReserved1        uint32
	DmReserved2        uint32
	DmPanningWidth     uint32
	DmPanningHeight    uint32
}

var (
	modUser32 = syscall.NewLazyDLL("user32.dll")

	procEnumDisplaySettingsExW   = modUser32.NewProc("EnumDisplaySettingsExW")
	procChangeDisplaySettingsExW = modUser32.NewProc("ChangeDisplaySettingsExW")
)

// EnumerateModes returns every display mode the primary monitor
// supports, queried via EnumDisplaySettingsExW. We pass
// lpszDeviceName = NULL which means "the primary display device";
// that's the monitor the bridge wants to drive.
//
// Modes that report zero width or height (a quirk some virtualized
// drivers emit at iModeNum=0) are dropped.
func EnumerateModes() ([]Mode, error) {
	var modes []Mode
	for i := uint32(0); ; i++ {
		var dm devmodeW
		dm.DmSize = uint16(unsafe.Sizeof(dm))
		ret, _, _ := procEnumDisplaySettingsExW.Call(
			0, // lpszDeviceName = NULL → primary display
			uintptr(i),
			uintptr(unsafe.Pointer(&dm)),
			0, // dwFlags
		)
		if ret == 0 {
			break
		}
		w := int(dm.DmPelsWidth)
		h := int(dm.DmPelsHeight)
		if w <= 0 || h <= 0 {
			continue
		}
		modes = append(modes, Mode{
			Width:  w,
			Height: h,
			Bits:   int(dm.DmBitsPerPel),
			Hz:     int(dm.DmDisplayFrequency),
		})
	}
	if len(modes) == 0 {
		return nil, errors.New("display: EnumDisplaySettingsExW returned no modes")
	}
	return modes, nil
}

// CurrentMode returns the resolution the primary display is using right now
// (ENUM_CURRENT_SETTINGS), so callers can tell whether a mode change is even
// needed before forcing one.
func CurrentMode() (Mode, error) {
	var dm devmodeW
	dm.DmSize = uint16(unsafe.Sizeof(dm))
	ret, _, _ := procEnumDisplaySettingsExW.Call(
		0, // lpszDeviceName = NULL → primary display
		uintptr(enumCurrentSettings),
		uintptr(unsafe.Pointer(&dm)),
		0, // dwFlags
	)
	if ret == 0 {
		return Mode{}, errors.New("display: EnumDisplaySettingsExW(ENUM_CURRENT_SETTINGS) failed")
	}
	w := int(dm.DmPelsWidth)
	h := int(dm.DmPelsHeight)
	if w <= 0 || h <= 0 {
		return Mode{}, errors.New("display: current mode reported zero dimensions")
	}
	return Mode{Width: w, Height: h, Bits: int(dm.DmBitsPerPel), Hz: int(dm.DmDisplayFrequency)}, nil
}

// ApplyMode picks the best supported mode for the requested
// (width, height), applies it via ChangeDisplaySettingsExW with
// CDS_UPDATEREGISTRY, and returns the mode that was actually
// applied (which may differ from `requested` — see PickBestMode).
//
// Returns ErrNoMatchingMode if no supported mode fits, or a wrapped
// error describing the ChangeDisplaySettingsExW return code on Win32
// failure.
func ApplyMode(requested Mode) (Mode, error) {
	// Try the exact requested resolution first. The Bochs VGA adapter
	// (QEMU stdvga) supports arbitrary resolutions up to VRAM limits,
	// even if EnumDisplaySettingsExW doesn't list them.
	if m, err := trySetMode(requested.Width, requested.Height); err == nil {
		return m, nil
	}

	// Fall back to the closest enumerated mode.
	modes, err := EnumerateModes()
	if err != nil {
		return Mode{}, err
	}
	best, err := PickBestMode(modes, requested)
	if err != nil {
		return Mode{}, err
	}
	return trySetMode(best.Width, best.Height)
}

func trySetMode(width, height int) (Mode, error) {
	var dm devmodeW
	dm.DmSize = uint16(unsafe.Sizeof(dm))
	dm.DmFields = dmPelsWidth | dmPelsHeight
	dm.DmPelsWidth = uint32(width)
	dm.DmPelsHeight = uint32(height)

	ret, _, _ := procChangeDisplaySettingsExW.Call(
		0,
		uintptr(unsafe.Pointer(&dm)),
		0,
		uintptr(cdsUpdateRegistry),
		0,
	)
	code := int32(ret)
	switch code {
	case dispChangeSuccessful:
		return Mode{Width: width, Height: height}, nil
	case dispChangeRestart:
		// Mode written to registry, but the current session won't see it
		// until the guest reboots — surface as ErrRestartRequired so the
		// daemon can trigger a reboot rather than logging a false "applied".
		return Mode{Width: width, Height: height}, ErrRestartRequired
	default:
		return Mode{}, fmt.Errorf("display: ChangeDisplaySettingsExW returned %d for %dx%d", code, width, height)
	}
}

// ApplyExact sets EXACTLY the requested mode with NO nearest-mode fallback.
// The agent's startup default uses this so it forces 1920x1080 only when the
// adapter actually accepts that mode, and otherwise leaves the current
// resolution untouched — rather than snapping to some other "closest" mode.
func ApplyExact(requested Mode) (Mode, error) {
	return trySetMode(requested.Width, requested.Height)
}
