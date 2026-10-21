/**
 * KeyboardLayoutMenu — dropdown to show and select the keyboard layout
 * used for the console session. Persists per VM in localStorage.
 */

import { useCallback, useEffect, useRef, useState } from 'react';

const C = {
  stageBg: 'var(--hc-bg-0)',
  border: 'var(--hc-bg-3)',
  muted: 'var(--hc-fg-2)',
  accent: 'var(--hc-accent-500)',
  hoverBg: 'var(--hc-bg-1)',
  activeBg: 'var(--hc-bg-2)',
} as const;

const FONT_MONO = 'var(--mono)';

export interface KeyboardLayout {
  id: string;
  label: string;
}

// eslint-disable-next-line react-refresh/only-export-components -- intentional: shared layout data co-located with the menu component
export const KEYBOARD_LAYOUTS: KeyboardLayout[] = [
  { id: 'us', label: 'US' },
  { id: 'uk', label: 'UK' },
  { id: 'it', label: 'Italian' },
  { id: 'de', label: 'German' },
  { id: 'fr', label: 'French' },
  { id: 'es', label: 'Spanish' },
  { id: 'pt-br', label: 'Portuguese BR' },
  { id: 'ch-de', label: 'Swiss German' },
  { id: 'ch-fr', label: 'Swiss French' },
  { id: 'jp', label: 'Japanese' },
];

const STORAGE_PREFIX = 'ahv-console-kbd-';

export interface UseKeyboardLayoutReturn {
  layout: KeyboardLayout;
  setLayoutId: (id: string) => void;
}

// eslint-disable-next-line react-refresh/only-export-components -- intentional: shared hook co-located with the menu component
export function useKeyboardLayout(vmId: string): UseKeyboardLayoutReturn {
  const storageKey = STORAGE_PREFIX + vmId;

  const [layoutId, setLayoutIdState] = useState<string>(() => {
    // Default to Italian: this is an Italian-operator deployment, and the
    // InputEngine likewise assumes an Italian physical layout by default
    // (see HostOS.detectHostKeyboardLayout) to recover symbol keys against
    // the guest's fixed en-us keymap. Keeping the visible menu default in
    // sync avoids the menu saying "US" while the engine recovers as Italian.
    try {
      return localStorage.getItem(storageKey) ?? 'it';
    } catch {
      return 'it';
    }
  });

  const setLayoutId = useCallback(
    (id: string) => {
      setLayoutIdState(id);
      try {
        localStorage.setItem(storageKey, id);
      } catch {
        /* */
      }
    },
    [storageKey],
  );

  const layout = KEYBOARD_LAYOUTS.find((l) => l.id === layoutId) ?? KEYBOARD_LAYOUTS[0]!;
  return { layout, setLayoutId };
}

export interface KeyboardLayoutMenuProps {
  layout: KeyboardLayout;
  onChange: (id: string) => void;
}

export function KeyboardLayoutMenu({ layout, onChange }: KeyboardLayoutMenuProps) {
  const [open, setOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onClickOutside = (e: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false);
    };
    document.addEventListener('mousedown', onClickOutside);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onClickOutside);
      document.removeEventListener('keydown', onKey);
    };
  }, [open]);

  return (
    <div ref={menuRef} style={{ position: 'relative' }}>
      <button
        onClick={() => setOpen((v) => !v)}
        title={`Keyboard layout: ${layout.label}`}
        style={{
          display: 'inline-flex',
          alignItems: 'center',
          gap: 5,
          padding: '4px 10px',
          background: 'transparent',
          color: C.muted,
          border: `1px solid ${C.border}`,
          borderRadius: 6,
          fontSize: 11,
          fontFamily: FONT_MONO,
          cursor: 'pointer',
          transition: 'color 120ms ease, border-color 120ms ease',
          whiteSpace: 'nowrap',
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
        {layout.label}
        <svg
          width="8"
          height="8"
          viewBox="0 0 8 8"
          fill="currentColor"
          style={{ marginLeft: 2, opacity: 0.6 }}
        >
          <path d="M1 3l3 3 3-3z" />
        </svg>
      </button>

      {open && (
        <div
          style={{
            position: 'absolute',
            top: 'calc(100% + 4px)',
            right: 0,
            zIndex: 100,
            minWidth: 180,
            background: C.stageBg,
            border: `1px solid ${C.border}`,
            borderRadius: 8,
            boxShadow: '0 8px 24px rgba(0, 0, 0, 0.5)',
            padding: '4px 0',
            fontFamily: FONT_MONO,
          }}
        >
          {KEYBOARD_LAYOUTS.map((l) => {
            const isActive = l.id === layout.id;
            return (
              <button
                key={l.id}
                onClick={() => {
                  onChange(l.id);
                  setOpen(false);
                }}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: 8,
                  width: '100%',
                  padding: '7px 12px',
                  background: isActive ? C.activeBg : 'transparent',
                  color: isActive ? '#fff' : C.muted,
                  border: 'none',
                  borderLeft: isActive ? `2px solid ${C.accent}` : '2px solid transparent',
                  cursor: 'pointer',
                  textAlign: 'left',
                  fontFamily: FONT_MONO,
                  fontSize: 12,
                  transition: 'background 80ms ease',
                }}
                onMouseEnter={(e) => {
                  if (!isActive) e.currentTarget.style.background = C.hoverBg;
                }}
                onMouseLeave={(e) => {
                  if (!isActive) e.currentTarget.style.background = 'transparent';
                }}
              >
                {isActive && <span>✓</span>}
                <span style={{ fontWeight: isActive ? 600 : 400 }}>{l.label}</span>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
