/**
 * Keyboard-LOCK banner. Shown ONLY while keyboard LOCK mode is armed (the native
 * windowed grab routes host-reserved combos to the VM), with OS-honest copy
 * naming the combos that stay host-owned. Renders invisibly (opacity 0) when not
 * locked so the unlock transition fades.
 *
 * The former on-open capture affordances — the "Click to control this VM" idle
 * hint and the "Captured — Ctrl+Alt to release" pill — were removed so opening a
 * console is quiet (no auto-notifications). The release chord lives in the
 * Shortcuts overlay.
 */

import { hostOS } from '../../input/HostOS';

// OS-honest copy for the persistent LOCK banner. Names the combos that the OS
// reserves and therefore stay on the Send Keys menu even while locked — so the
// banner never over-promises (see KEYBOARD_LOCK_DESIGN.md capability matrix).
function lockBannerText(): string {
  switch (hostOS()) {
    case 'windows':
      return 'Keyboard locked — host keys go to the VM · Ctrl+Alt or click away to release · Ctrl+Alt+Del and Win+L stay local (use Send Keys)';
    case 'macos':
      return 'Keyboard locked — Ctrl+Alt or click away to release · Cmd+Tab, Cmd+Space and Mission Control stay on your Mac (use Send Keys / Option+Tab in the guest)';
    default:
      return 'Keyboard locked — host keys go to the VM · Ctrl+Alt or click away to release · Ctrl+Alt+Del and the VT switches stay local (use Send Keys)';
  }
}

export function CaptureIndicator({ locked = false }: { locked?: boolean }) {
  return (
    <div
      aria-hidden={!locked}
      role={locked ? 'status' : undefined}
      style={{
        position: 'absolute',
        top: 52,
        left: '50%',
        transform: 'translateX(-50%)',
        zIndex: 56,
        maxWidth: '92%',
        padding: '6px 14px',
        borderRadius: 999,
        background: 'rgba(40,26,8,.9)',
        border: '1px solid rgba(242,166,90,.5)',
        backdropFilter: 'blur(8px)',
        color: '#fff',
        fontSize: 11.5,
        fontFamily: 'var(--hc-font-sans)',
        display: 'flex',
        alignItems: 'center',
        gap: 7,
        opacity: locked ? 1 : 0,
        pointerEvents: 'none',
        transition: 'opacity 200ms ease',
      }}
    >
      <span
        aria-hidden
        style={{
          width: 7,
          height: 7,
          borderRadius: '50%',
          background: '#F2A65A',
          flexShrink: 0,
        }}
      />
      {lockBannerText()}
    </div>
  );
}
