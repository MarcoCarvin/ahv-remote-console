/**
 * Constants for the RFB / VNC wire protocol (RFC 6143 + RealVNC extensions
 * relevant to Nutanix AHV's noVNC-based stream).
 *
 * Only the bits AHV Remote Console actually emits or consumes are listed; the rest
 * are intentionally elided to keep this file readable.
 */

// Client → server messages.
export const CLI_MSG_SET_PIXEL_FORMAT = 0;
export const CLI_MSG_SET_ENCODINGS = 2;
export const CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST = 3;
export const CLI_MSG_KEY_EVENT = 4;
export const CLI_MSG_POINTER_EVENT = 5;
export const CLI_MSG_CLIENT_CUT_TEXT = 6;
/**
 * Client→server SetDesktopSize message (RFC 6143 §7.5.5 / RealVNC
 * extended-desktop-size protocol). The renderer emits this when the
 * operator resizes the console window so the guest's display driver
 * can switch resolutions in-band. Wire format (big-endian):
 *
 *   u8  type      = 251
 *   u8  padding   = 0
 *   u16 width
 *   u16 height
 *   u8  screens   = 1
 *   u8  padding   = 0
 *   // screen #0
 *   u32 id        = 0
 *   u16 x_pos     = 0
 *   u16 y_pos     = 0
 *   u16 width     (matches outer)
 *   u16 height    (matches outer)
 *   u32 flags     = 0
 *
 * Total: 24 bytes. Servers that don't support the extension reply
 * with an ExtendedDesktopSize FBU carrying status != 0; we treat
 * that as best-effort and rely on the bridge daemon's
 * ChangeDisplaySettingsExW path as the actual resize mechanism.
 */
export const CLI_MSG_SET_DESKTOP_SIZE = 251;
export const CLI_MSG_ENABLE_CONTINUOUS_UPDATES = 150;

// QEMU Extended Key Event message (client→server). Sent as a
// composite frame: type=255 (QEMU msg) + subtype=0. Pre-conditions:
// client must have announced ENC_QEMU_EXT_KEY_EVENT (-258) in
// SetEncodings; server compliance is best-effort — there is no
// negotiation ack. If the server doesn't support it, the bytes
// are silently dropped which is why we keep the legacy KeyEvent
// path as fallback. See docs/INTERACTION-ARCH.md §3 Phase 3.
export const CLI_MSG_QEMU = 255;
export const QEMU_SUBMSG_EXT_KEY = 0;

// Server → client messages.
export const SRV_MSG_FRAMEBUFFER_UPDATE = 0;
export const SRV_MSG_SET_COLOUR_MAP_ENTRIES = 1;
export const SRV_MSG_BELL = 2;
export const SRV_MSG_SERVER_CUT_TEXT = 3;
export const SRV_MSG_END_OF_CONTINUOUS_UPDATES = 150;

// Encodings.
export const ENC_RAW = 0;
export const ENC_COPY_RECT = 1;
export const ENC_RRE = 2;
export const ENC_HEXTILE = 5;
export const ENC_TIGHT = 7;
export const ENC_ZRLE = 16;
export const ENC_CURSOR = -239;
export const ENC_DESKTOP_SIZE = -223;
export const ENC_EXTENDED_DESKTOP_SIZE = -308;
export const ENC_CONTINUOUS_UPDATES = -313;
// QEMU Extended Key Event pseudo-encoding. Announcing it in
// SetEncodings tells QEMU's VNC server to accept CLI_MSG_QEMU
// frames; servers that don't recognize it ignore the announcement
// and we fall back to the plain CLI_MSG_KEY_EVENT path.
export const ENC_QEMU_EXT_KEY_EVENT = -258;

// Tight quality / compression pseudo-encodings (TightVNC extension).
// Quality:     -32 (quality 0) … -23 (quality 9)
// Compression: -256 (compress 0) … -247 (compress 9)
// QEMU's Tight encoder interprets these as JPEG quality and zlib
// compression level respectively. Appended to SetEncodings.
export const ENC_QUALITY_BASE = -32; // -32 = quality 0, -23 = quality 9
export const ENC_COMPRESS_BASE = -256; // -256 = compress 0, -247 = compress 9

// Security types.
export const SEC_NONE = 1;
export const SEC_VNC_AUTH = 2;
export const SEC_TIGHT = 16;

// Pointer button bits (RFB uses a bitmask).
export const BTN_LEFT = 1 << 0;
export const BTN_MIDDLE = 1 << 1;
export const BTN_RIGHT = 1 << 2;
export const BTN_WHEEL_UP = 1 << 3;
export const BTN_WHEEL_DOWN = 1 << 4;
