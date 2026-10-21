/**
 * FullscreenToggle — a toolbar button + hook for entering/exiting
 * browser fullscreen, with a transient toast showing the exit shortcut.
 *
 * Exports:
 *   - useFullscreen()   — hook returning { isFullscreen, toggleFullscreen, FullscreenToast }
 *   - FullscreenToggle  — ready-to-render button component
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import { hostOS } from '../input/HostOS';
import { useMotion } from '../motion/MotionContext';

// ── Stage palette ──────────────────────────────────────────────────
const C = {
  stageBg: 'var(--hc-bg-0)',
  border: 'var(--hc-bg-3)',
  muted: 'var(--hc-fg-2)',
  accent: 'var(--hc-accent-500)',
  green: '#34c759',
} as const;

const FONT_MONO = 'var(--mono)';

const IS_MAC = hostOS() === 'macos';

// ── Hook ───────────────────────────────────────────────────────────

export interface UseFullscreenReturn {
  isFullscreen: boolean;
  toggleFullscreen: () => void;
  FullscreenToast: () => React.JSX.Element | null;
}

// eslint-disable-next-line react-refresh/only-export-components -- intentional: shared hook co-located with its toggle component
export function useFullscreen(): UseFullscreenReturn {
  const [isFullscreen, setIsFullscreen] = useState(() => !!document.fullscreenElement);
  const [toastVisible, setToastVisible] = useState(false);
  const toastTimerRef = useRef<number>(0);

  // Track fullscreenchange from any source (user, API, F11, etc.)
  useEffect(() => {
    const onChange = () => {
      const fs = !!document.fullscreenElement;
      setIsFullscreen(fs);
      if (fs) {
        // Show the exit-hint toast for 4 seconds on enter.
        setToastVisible(true);
        window.clearTimeout(toastTimerRef.current);
        toastTimerRef.current = window.setTimeout(() => setToastVisible(false), 4000);
      } else {
        setToastVisible(false);
        window.clearTimeout(toastTimerRef.current);
      }
    };
    document.addEventListener('fullscreenchange', onChange);
    return () => {
      document.removeEventListener('fullscreenchange', onChange);
      window.clearTimeout(toastTimerRef.current);
    };
  }, []);

  // Keyboard shortcut: Ctrl+Cmd+F on Mac (the macOS standard), F11 on others.
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (IS_MAC) {
        if (e.metaKey && e.ctrlKey && !e.altKey && e.code === 'KeyF') {
          e.preventDefault();
          toggle();
        }
      } else {
        if (e.code === 'F11') {
          e.preventDefault();
          toggle();
        }
      }
    };
    window.addEventListener('keydown', onKeyDown, { capture: true });
    return () => window.removeEventListener('keydown', onKeyDown, { capture: true });
    // toggle is stable (useCallback with no deps that change).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const toggle = useCallback(() => {
    void window.ahvRemoteConsole.toggleFullscreen();
  }, []);

  // Under reduced motion the global rule in motion.css shortens every animation
  // to ~0 ms, which would jump this fade straight to its final opacity 0. Skip
  // the animation instead: the hint stays visible until the 4 s timer hides it.
  // The media query is checked directly too, because the console pop-out window
  // has no MotionProvider.
  const motion = useMotion();
  const reduceMotion =
    !motion.enabled || window.matchMedia?.('(prefers-reduced-motion: reduce)').matches === true;

  const FullscreenToast = useCallback(() => {
    if (!toastVisible) return null;
    const shortcutLabel = IS_MAC ? 'Press ⌃⌘F to exit fullscreen' : 'Press F11 to exit fullscreen';
    return (
      <div
        role="status"
        style={{
          position: 'fixed',
          top: 24,
          left: '50%',
          transform: 'translateX(-50%)',
          zIndex: 9999,
          padding: '8px 18px',
          background: C.stageBg,
          color: '#fff',
          border: `1px solid ${C.border}`,
          borderRadius: 8,
          fontSize: 12,
          fontFamily: FONT_MONO,
          boxShadow: '0 4px 20px rgba(0, 0, 0, 0.5)',
          userSelect: 'none',
          // Informational hint only — must NOT intercept clicks. Without this it
          // sat over the guest's top-center for 4s on every fullscreen entry and
          // ate clicks meant for the guest's own top-edge controls. It auto-hides
          // after 4s (toastTimerRef), so it needs no click-to-dismiss.
          pointerEvents: 'none',
          animation: reduceMotion ? 'none' : 'hc-fs-toast-fade 4s ease forwards',
        }}
      >
        {shortcutLabel}
        <style>{`
            @keyframes hc-fs-toast-fade {
              0%, 70% { opacity: 1; }
              100%     { opacity: 0; }
            }
          `}</style>
      </div>
    );
  }, [toastVisible, reduceMotion]);

  return { isFullscreen, toggleFullscreen: toggle, FullscreenToast };
}

// useWindowFullscreen tracks NATIVE window fullscreen — the macOS green
// button / F11 path that toggleFullscreen drives via win.setFullScreen.
// The DOM `fullscreenchange` event (what useFullscreen watches) never
// fires for those, so main forwards the BrowserWindow enter/leave events
// over the contextBridge. ConsoleView combines both signals to decide
// when the guest canvas should go full-bleed.
// eslint-disable-next-line react-refresh/only-export-components -- intentional: shared hook co-located with its toggle component
export function useWindowFullscreen(): boolean {
  const [fs, setFs] = useState(false);
  useEffect(() => {
    // Optional-chained: a stale preload during dev hot-reload may predate
    // the channel; the console then simply keeps its windowed insets.
    const off = window.ahvRemoteConsole.onWindowFullscreen?.(({ fullscreen }) => setFs(fullscreen));
    return () => off?.();
  }, []);
  return fs;
}

// ── Component ──────────────────────────────────────────────────────

export interface FullscreenToggleProps {
  isFullscreen: boolean;
  onToggle: () => void;
}

export function FullscreenToggle({ isFullscreen, onToggle }: FullscreenToggleProps) {
  const title = isFullscreen
    ? `Exit fullscreen (${IS_MAC ? '⌃⌘F' : 'F11'})`
    : `Enter fullscreen (${IS_MAC ? '⌃⌘F' : 'F11'})`;

  return (
    <button
      onClick={onToggle}
      title={title}
      aria-label={title}
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        justifyContent: 'center',
        width: 28,
        height: 28,
        padding: 0,
        background: 'transparent',
        color: C.muted,
        border: `1px solid ${C.border}`,
        borderRadius: 6,
        fontSize: 15,
        lineHeight: 1,
        cursor: 'pointer',
        fontFamily: FONT_MONO,
        transition: 'color 120ms ease, border-color 120ms ease',
      }}
      onMouseEnter={(e) => {
        e.currentTarget.style.color = '#fff';
        e.currentTarget.style.borderColor = C.muted;
      }}
      onMouseLeave={(e) => {
        e.currentTarget.style.color = C.muted;
        e.currentTarget.style.borderColor = C.border;
      }}
    >
      {isFullscreen ? (
        // Collapse icon (exit fullscreen) — inward arrows
        <svg
          width="14"
          height="14"
          viewBox="0 0 14 14"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <polyline points="9,1 9,5 13,5" />
          <polyline points="5,13 5,9 1,9" />
        </svg>
      ) : (
        // Expand icon (enter fullscreen) — outward arrows
        <svg
          width="14"
          height="14"
          viewBox="0 0 14 14"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <polyline points="9,1 13,1 13,5" />
          <polyline points="5,13 1,13 1,9" />
        </svg>
      )}
    </button>
  );
}
