/**
 * DisplayFitMenu — a dropdown selector for console display-fit mode.
 *
 * Four modes:
 *   - fit           Scale canvas to fill container, maintaining aspect ratio.
 *   - pixel         1:1 pixel mapping; container scrolls if larger.
 *   - stretch       Fill container, ignoring aspect ratio.
 *   - resize-guest  Same visual as 'fit'. (A SetDesktopSize RFB request
 *                   is not honored on AHV and is no longer sent.)
 *
 * Exports:
 *   - useFitMode(vmId)  — persists choice per VM in localStorage
 *   - DisplayFitMenu    — dropdown component matching Stage dark palette
 */

import { useCallback, useEffect, useRef, useState } from 'react';

// ── Stage palette ──────────────────────────────────────────────────
const C = {
  stageBg: 'var(--hc-bg-0)',
  border: 'var(--hc-bg-3)',
  muted: 'var(--hc-fg-2)',
  accent: 'var(--hc-accent-500)',
  hoverBg: 'var(--hc-bg-1)',
  activeBg: 'var(--hc-bg-2)',
} as const;

const FONT_MONO = 'var(--mono)';

// ── Types ──────────────────────────────────────────────────────────

export type FitMode = 'fit' | 'pixel' | 'stretch' | 'resize-guest';

interface FitModeOption {
  value: FitMode;
  label: string;
  description: string;
}

const FIT_OPTIONS: FitModeOption[] = [
  {
    value: 'fit',
    label: 'Fit',
    description: 'Scale to fill, maintain aspect ratio',
  },
  {
    value: 'pixel',
    label: '1 : 1',
    description: 'Native pixels, scroll if needed',
  },
  {
    value: 'stretch',
    label: 'Stretch',
    description: 'Fill container, ignore aspect ratio',
  },
  {
    value: 'resize-guest',
    label: 'Resize guest',
    description: 'Same as Fit (guest resize unsupported on AHV)',
  },
];

// ── Hook ───────────────────────────────────────────────────────────

const STORAGE_PREFIX = 'ahv-console-fit-';

export interface UseFitModeReturn {
  fitMode: FitMode;
  setFitMode: (mode: FitMode) => void;
}

/**
 * Persists the operator's display-fit preference per VM in
 * localStorage. Defaults to 'resize-guest' when no preference is
 * stored (behaviorally identical to 'fit' on AHV).
 */
// eslint-disable-next-line react-refresh/only-export-components -- intentional: shared hook co-located with its component
export function useFitMode(vmId: string): UseFitModeReturn {
  const storageKey = STORAGE_PREFIX + vmId;

  const [fitMode, setFitModeState] = useState<FitMode>(() => {
    try {
      const stored = localStorage.getItem(storageKey);
      if (stored && isFitMode(stored)) return stored;
    } catch {
      /* storage unavailable */
    }
    return 'resize-guest';
  });

  const setFitMode = useCallback(
    (mode: FitMode) => {
      setFitModeState(mode);
      try {
        localStorage.setItem(storageKey, mode);
      } catch {
        /* storage full / unavailable */
      }
    },
    [storageKey],
  );

  return { fitMode, setFitMode };
}

function isFitMode(v: string): v is FitMode {
  return v === 'fit' || v === 'pixel' || v === 'stretch' || v === 'resize-guest';
}

// ── Component ──────────────────────────────────────────────────────

export interface DisplayFitMenuProps {
  fitMode: FitMode;
  onChange: (mode: FitMode) => void;
}

export function DisplayFitMenu({ fitMode, onChange }: DisplayFitMenuProps) {
  const [open, setOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);

  // Close on click outside.
  useEffect(() => {
    if (!open) return;
    const onClickOutside = (e: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    // Close on Escape.
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

  const currentOption = FIT_OPTIONS.find((o) => o.value === fitMode) ?? FIT_OPTIONS[0]!;

  return (
    <div ref={menuRef} style={{ position: 'relative' }}>
      {/* Trigger button */}
      <button
        onClick={() => setOpen((v) => !v)}
        title={`Display fit: ${currentOption.label}`}
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
        <svg
          width="12"
          height="12"
          viewBox="0 0 12 12"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.2"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <rect x="1" y="1" width="10" height="10" rx="1.5" />
          <rect x="3" y="3" width="6" height="6" rx="0.5" />
        </svg>
        {currentOption.label}
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

      {/* Dropdown */}
      {open && (
        <div
          style={{
            position: 'absolute',
            top: 'calc(100% + 4px)',
            right: 0,
            zIndex: 100,
            minWidth: 200,
            background: C.stageBg,
            border: `1px solid ${C.border}`,
            borderRadius: 8,
            boxShadow: '0 8px 24px rgba(0, 0, 0, 0.5)',
            padding: '4px 0',
            fontFamily: FONT_MONO,
          }}
        >
          {FIT_OPTIONS.map((opt) => {
            const isActive = opt.value === fitMode;
            return (
              <button
                key={opt.value}
                onClick={() => {
                  onChange(opt.value);
                  setOpen(false);
                }}
                style={{
                  display: 'flex',
                  flexDirection: 'column',
                  alignItems: 'flex-start',
                  gap: 1,
                  width: '100%',
                  padding: '8px 12px',
                  background: isActive ? C.activeBg : 'transparent',
                  color: isActive ? '#fff' : C.muted,
                  border: 'none',
                  borderLeft: isActive ? `2px solid ${C.accent}` : '2px solid transparent',
                  cursor: 'pointer',
                  textAlign: 'left',
                  fontFamily: FONT_MONO,
                  transition: 'background 80ms ease',
                }}
                onMouseEnter={(e) => {
                  if (!isActive) e.currentTarget.style.background = C.hoverBg;
                }}
                onMouseLeave={(e) => {
                  if (!isActive) e.currentTarget.style.background = 'transparent';
                }}
              >
                <span
                  style={{
                    fontSize: 12,
                    fontWeight: isActive ? 600 : 400,
                    display: 'flex',
                    alignItems: 'center',
                    gap: 6,
                  }}
                >
                  {isActive && <span style={{ fontSize: 13 }}>✓</span>}
                  {opt.label}
                </span>
                <span style={{ fontSize: 10, color: C.muted, opacity: 0.7 }}>
                  {opt.description}
                </span>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
