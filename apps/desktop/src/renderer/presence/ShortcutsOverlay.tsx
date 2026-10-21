/**
 * ShortcutsOverlay — a Raycast/Linear-style keyboard shortcuts modal.
 *
 * Opens when the operator presses `?` while the console canvas is
 * NOT focused (i.e. not captured). Closes on Escape or click-outside.
 *
 * Sections: Console, Navigation, Stage.
 */

import { useCallback, useEffect, useState } from 'react';
import { hostOS } from '../input/HostOS';

// ── Stage palette ──────────────────────────────────────────────────
const C = {
  stageBg: 'var(--hc-bg-0)',
  panelBg: '#252219',
  border: 'var(--hc-bg-3)',
  muted: 'var(--hc-fg-2)',
  accent: 'var(--hc-accent-500)',
  fg: '#e8e0d0',
  fgDim: '#b5ad9e',
  kbdBg: '#2e2a23',
  kbdBorder: '#4a4538',
  sectionFg: '#c8c0b0',
  overlayBg: 'rgba(10, 9, 7, 0.7)',
} as const;

const FONT_MONO = 'var(--mono)';

const IS_MAC = hostOS() === 'macos';

// ── Shortcut data ──────────────────────────────────────────────────

interface Shortcut {
  keys: string[];
  label: string;
}

interface ShortcutSection {
  title: string;
  shortcuts: Shortcut[];
}

function getSections(): ShortcutSection[] {
  return [
    {
      title: 'Console',
      shortcuts: [
        {
          keys: ['Ctrl', 'Alt'],
          label: 'Release keyboard / mouse capture',
        },
        {
          keys: IS_MAC ? ['⌃', '⌘', 'F'] : ['F11'],
          label: 'Toggle fullscreen',
        },
        {
          keys: ['?'],
          label: 'Show this shortcut reference',
        },
      ],
    },
    {
      title: 'Navigation',
      shortcuts: [
        {
          // UX-03: Cmd+K / Ctrl+K is reserved app-wide for the command palette
          // (App.tsx). It is no longer the cluster-map search trigger.
          keys: IS_MAC ? ['⌘', 'K'] : ['Ctrl', 'K'],
          label: 'Open command palette',
        },
      ],
    },
    {
      title: 'Cluster map',
      shortcuts: [
        {
          // Cluster search uses the platform Find shortcut, so it does not
          // collide with the command palette on Cmd/Ctrl+K.
          keys: IS_MAC ? ['⌘', 'F'] : ['Ctrl', 'F'],
          label: 'Search clusters',
        },
      ],
    },
  ];
}

// ── Hook ───────────────────────────────────────────────────────────

export interface UseShortcutsOverlayReturn {
  isOpen: boolean;
  open: () => void;
  close: () => void;
}

/**
 * Listens for `?` at the document level (capture phase) and opens
 * the overlay ONLY when the console canvas does not have focus.
 * The `captured` flag from ConsoleView tracks focus state.
 */
// eslint-disable-next-line react-refresh/only-export-components -- intentional: shared hook co-located with the overlay component
export function useShortcutsOverlay(captured: boolean): UseShortcutsOverlayReturn {
  const [isOpen, setIsOpen] = useState(false);

  const open = useCallback(() => setIsOpen(true), []);
  const close = useCallback(() => setIsOpen(false), []);

  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      // Only trigger when NOT captured (canvas not focused).
      if (captured) return;
      // Ignore if any modifier is held — we only want bare `?`.
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      // `?` is Shift+/ on US layout; check the actual key value.
      if (e.key === '?') {
        e.preventDefault();
        setIsOpen((v) => !v);
      }
    };
    document.addEventListener('keydown', onKeyDown, { capture: true });
    return () => document.removeEventListener('keydown', onKeyDown, { capture: true });
  }, [captured]);

  return { isOpen, open, close };
}

// ── Component ──────────────────────────────────────────────────────

export interface ShortcutsOverlayProps {
  isOpen: boolean;
  onClose: () => void;
}

export function ShortcutsOverlay({ isOpen, onClose }: ShortcutsOverlayProps) {
  // Close on Escape.
  useEffect(() => {
    if (!isOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        onClose();
      }
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [isOpen, onClose]);

  if (!isOpen) return null;

  const sections = getSections();

  return (
    // Backdrop
    <div
      onClick={(e) => {
        // Close on click outside the panel.
        if (e.target === e.currentTarget) onClose();
      }}
      style={{
        position: 'fixed',
        inset: 0,
        zIndex: 9998,
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        background: C.overlayBg,
        backdropFilter: 'blur(4px)',
        WebkitBackdropFilter: 'blur(4px)',
      }}
    >
      {/* Panel */}
      <div
        role="dialog"
        aria-label="Keyboard shortcuts"
        style={{
          width: 480,
          maxWidth: '90vw',
          maxHeight: '80vh',
          overflowY: 'auto',
          background: C.panelBg,
          border: `1px solid ${C.border}`,
          borderRadius: 12,
          boxShadow: '0 16px 48px rgba(0, 0, 0, 0.6)',
          fontFamily: FONT_MONO,
          padding: 0,
        }}
      >
        {/* Header */}
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            padding: '16px 20px 12px',
            borderBottom: `1px solid ${C.border}`,
          }}
        >
          <span
            style={{
              fontSize: 13,
              fontWeight: 700,
              color: C.fg,
              letterSpacing: '0.02em',
            }}
          >
            Keyboard shortcuts
          </span>
          <button
            onClick={onClose}
            aria-label="Close"
            style={{
              display: 'inline-flex',
              alignItems: 'center',
              justifyContent: 'center',
              width: 24,
              height: 24,
              padding: 0,
              background: 'transparent',
              color: C.muted,
              border: 'none',
              borderRadius: 4,
              fontSize: 16,
              cursor: 'pointer',
              lineHeight: 1,
              transition: 'color 100ms ease',
            }}
            onMouseEnter={(e) => {
              e.currentTarget.style.color = C.fg;
            }}
            onMouseLeave={(e) => {
              e.currentTarget.style.color = C.muted;
            }}
          >
            ×
          </button>
        </div>

        {/* Sections */}
        <div style={{ padding: '8px 0' }}>
          {sections.map((section, si) => (
            <div key={section.title}>
              {/* Section header */}
              <div
                style={{
                  padding: '12px 20px 6px',
                  fontSize: 10,
                  fontWeight: 600,
                  color: C.accent,
                  textTransform: 'uppercase',
                  letterSpacing: '0.08em',
                }}
              >
                {section.title}
              </div>

              {/* Rows */}
              {section.shortcuts.map((shortcut, ri) => (
                <div
                  key={ri}
                  style={{
                    display: 'flex',
                    alignItems: 'center',
                    justifyContent: 'space-between',
                    padding: '7px 20px',
                    transition: 'background 80ms ease',
                  }}
                  onMouseEnter={(e) => {
                    e.currentTarget.style.background = 'rgba(255,255,255,0.03)';
                  }}
                  onMouseLeave={(e) => {
                    e.currentTarget.style.background = 'transparent';
                  }}
                >
                  <span
                    style={{
                      fontSize: 12,
                      color: C.fgDim,
                      flex: 1,
                      marginRight: 16,
                    }}
                  >
                    {shortcut.label}
                  </span>
                  <span
                    style={{
                      display: 'inline-flex',
                      alignItems: 'center',
                      gap: 4,
                      flexShrink: 0,
                    }}
                  >
                    {shortcut.keys.map((key, ki) => (
                      <kbd
                        key={ki}
                        style={{
                          display: 'inline-flex',
                          alignItems: 'center',
                          justifyContent: 'center',
                          minWidth: 24,
                          height: 22,
                          padding: '0 6px',
                          background: C.kbdBg,
                          border: `1px solid ${C.kbdBorder}`,
                          borderRadius: 4,
                          fontSize: 11,
                          fontFamily: FONT_MONO,
                          color: C.fg,
                          boxShadow: '0 1px 0 rgba(0,0,0,0.3)',
                          lineHeight: 1,
                        }}
                      >
                        {key}
                      </kbd>
                    ))}
                  </span>
                </div>
              ))}

              {/* Divider between sections (except the last) */}
              {si < sections.length - 1 && (
                <div
                  style={{
                    height: 1,
                    margin: '4px 20px',
                    background: C.border,
                  }}
                />
              )}
            </div>
          ))}
        </div>

        {/* Footer hint */}
        <div
          style={{
            padding: '10px 20px 14px',
            borderTop: `1px solid ${C.border}`,
            fontSize: 10,
            color: C.muted,
            textAlign: 'center',
            opacity: 0.7,
          }}
        >
          Press{' '}
          <kbd
            style={{
              padding: '1px 5px',
              background: C.kbdBg,
              border: `1px solid ${C.kbdBorder}`,
              borderRadius: 3,
              fontSize: 10,
              fontFamily: FONT_MONO,
              color: C.fg,
            }}
          >
            ?
          </kbd>{' '}
          or{' '}
          <kbd
            style={{
              padding: '1px 5px',
              background: C.kbdBg,
              border: `1px solid ${C.kbdBorder}`,
              borderRadius: 3,
              fontSize: 10,
              fontFamily: FONT_MONO,
              color: C.fg,
            }}
          >
            Esc
          </kbd>{' '}
          to close
        </div>
      </div>
    </div>
  );
}
