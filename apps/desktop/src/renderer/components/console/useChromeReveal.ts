/**
 * Hover-reveal chrome for the console's top rail, extracted verbatim from
 * ConsoleView (FE-01). CODE-06: clearHideTimer/scheduleHide are wrapped in
 * useCallback (they only read refs + stable setState), so the mousemove effect
 * dependency array can list them honestly without re-arming the effect (the
 * callbacks keep a stable identity for the component's life).
 */

import { useCallback, useEffect, useRef, useState } from 'react';

export interface ChromeReveal {
  revealed: boolean;
  /** True while the operator has pinned the rail open (auto-hide suspended). */
  pinned: boolean;
  /** Toggle the pin — wired to the rail's pushpin button. */
  togglePin: () => void;
  hoverProps: { onMouseEnter: () => void; onMouseLeave: () => void };
}

// The console toolbar is SHOWN BY DEFAULT and stays shown (pinned=true initial,
// no pin toggle button). The guest is given a fixed band below it (see
// ConsoleView viewportTop), so it never re-scales on pointer movement — earlier
// hover-reveal/pin-toggle variants caused the guest to resize on every top-edge
// hover, which is why the toolbar is now simply always-on.
//
// The hover-reveal + auto-hide machinery below is retained but is INERT while
// pinned (every path short-circuits on pinnedRef): kept so a future "auto-hide"
// preference could flip the default without re-deriving it. `fullBleed` only
// affects the (currently inert) reveal-zone width.
export function useChromeReveal(fullBleed = false): ChromeReveal {
  const [visible, setVisible] = useState(true);
  const [pinned, setPinned] = useState(true);
  // Mirror of `pinned` for the closures (mousemove / hide timer) so they read
  // the live value without re-arming on every toggle.
  const pinnedRef = useRef(true);
  const hoveringRef = useRef(false);
  const hideTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const clearHideTimer = useCallback(() => {
    if (hideTimer.current) {
      clearTimeout(hideTimer.current);
      hideTimer.current = null;
    }
  }, []);
  const scheduleHide = useCallback(() => {
    clearHideTimer();
    if (pinnedRef.current || hoveringRef.current) return;
    hideTimer.current = setTimeout(() => setVisible(false), 1500);
  }, [clearHideTimer]);

  const togglePin = useCallback(() => {
    const next = !pinnedRef.current;
    pinnedRef.current = next;
    setPinned(next);
    if (next) {
      clearHideTimer();
      setVisible(true);
    } else {
      scheduleHide();
    }
  }, [clearHideTimer, scheduleHide]);

  useEffect(() => {
    // Full-bleed: 3px = a deliberate shove to the clamped top edge. Windowed:
    // 64px = a forgiving reach for chrome that overlays no guest.
    const EDGE = fullBleed ? 3 : 64;
    const onMove = (e: MouseEvent) => {
      if (pinnedRef.current) return;
      const inZone = e.clientY < EDGE;
      if (inZone && !hoveringRef.current) {
        clearHideTimer();
        setVisible(true);
      } else if (!inZone && !hoveringRef.current) {
        scheduleHide();
      }
    };
    window.addEventListener('mousemove', onMove);
    return () => {
      window.removeEventListener('mousemove', onMove);
      clearHideTimer();
    };
  }, [clearHideTimer, scheduleHide, fullBleed]);

  return {
    revealed: visible,
    pinned,
    togglePin,
    hoverProps: {
      onMouseEnter: () => {
        hoveringRef.current = true;
        clearHideTimer();
        setVisible(true);
      },
      onMouseLeave: () => {
        hoveringRef.current = false;
        scheduleHide();
      },
    },
  };
}
