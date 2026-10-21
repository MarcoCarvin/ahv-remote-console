/**
 * Overlay — the shared "fancy" transient-surface treatment for the console.
 *
 * Every transient surface (confirm dialogs, the Snapshots / Stats panels, and
 * the toolbar menus) renders through this instead of an ad-hoc anchored popup:
 * a blurred dark backdrop fades in over the frozen frame and a centered card
 * fades + rises in — matching the connection-state overlay's aesthetic
 * (ConsoleOverlays, zIndex 50). These sit above it (zIndex >= 120).
 *
 * Dismissal is uniform: click the backdrop, or press Esc. Esc closes only the
 * TOPMOST overlay (a module-level stack), so a confirm layered over a panel
 * closes the confirm first, then the panel.
 *
 * `Overlay` is the bare backdrop+centering+fade+dismissal — its child IS the
 * card (so StatsPanel / SnapshotsPanel / confirm bodies keep their own chrome).
 * `OverlayMenu` is the card form for the toolbar menus (a titled list of items).
 */

import { useEffect, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { Icon } from '../Icon';

const FADE_MS = 180;

// Open overlays, topmost last. Esc acts only on the last entry so stacked
// overlays (confirm over panel) unwind one at a time.
const overlayStack: object[] = [];

const FOCUSABLE =
  'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

function focusableIn(container: HTMLElement | null): HTMLElement[] {
  if (!container) return [];
  return Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE));
}

export function Overlay({
  label,
  onClose,
  children,
  zIndex = 120,
  backdropBlur = true,
}: {
  label: string;
  onClose: () => void;
  children: ReactNode;
  /** Stacking order. Confirms layered over a panel pass a higher value. */
  zIndex?: number;
  /** Blur the backdrop. A confirm layered over an already-blurred panel passes
   *  false so the blur isn't stacked twice (which reads muddy). */
  backdropBlur?: boolean;
}) {
  const [shown, setShown] = useState(false);
  const tokenRef = useRef<object>({});
  const cardRef = useRef<HTMLDivElement>(null);

  // Fade/scale in on mount; register on the stack for topmost-only Esc.
  useEffect(() => {
    const token = tokenRef.current;
    overlayStack.push(token);
    const raf = requestAnimationFrame(() => setShown(true));
    return () => {
      cancelAnimationFrame(raf);
      const i = overlayStack.indexOf(token);
      if (i >= 0) overlayStack.splice(i, 1);
    };
  }, []);

  // Modal focus contract: move focus into the dialog on open (off the VM canvas,
  // so keystrokes stop reaching the guest and Tab is trapped here), and restore
  // focus to whatever was focused before — the trigger — on close.
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null;
    // Focus the card container (tabIndex=-1) rather than a specific control, so
    // no destructive button is pre-selected; Tab then steps through the content.
    cardRef.current?.focus();
    return () => {
      if (prev?.isConnected) prev.focus();
    };
  }, []);

  // Esc closes and Tab is trapped — but only when THIS overlay is the topmost.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (overlayStack[overlayStack.length - 1] !== tokenRef.current) return;
      if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        onClose();
        return;
      }
      if (e.key === 'Tab') {
        const els = focusableIn(cardRef.current);
        const first = els[0];
        const last = els[els.length - 1];
        if (!first || !last) {
          e.preventDefault();
          return;
        }
        const active = document.activeElement;
        if (e.shiftKey && (active === first || active === cardRef.current)) {
          e.preventDefault();
          last.focus();
        } else if (!e.shiftKey && active === last) {
          e.preventDefault();
          first.focus();
        }
      }
    };
    document.addEventListener('keydown', onKey, true);
    return () => document.removeEventListener('keydown', onKey, true);
  }, [onClose]);

  // Portal to <body> with position:fixed so the overlay always covers the whole
  // window and never gets clipped by a transformed ancestor — which matters when
  // one Overlay is nested inside another (a confirm over the Snapshots panel):
  // the parent's fade transform would otherwise become the confirm's containing
  // block. Portaling sidesteps that entirely and keeps stacking by zIndex.
  return createPortal(
    <div
      role="dialog"
      aria-modal="true"
      aria-label={label}
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
      style={{
        position: 'fixed',
        inset: 0,
        zIndex,
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        padding: 24,
        boxSizing: 'border-box',
        background: backdropBlur ? 'rgba(4, 10, 18, 0.55)' : 'rgba(4, 10, 18, 0.62)',
        backdropFilter: backdropBlur ? 'blur(4px)' : undefined,
        WebkitBackdropFilter: backdropBlur ? 'blur(4px)' : undefined,
        opacity: shown ? 1 : 0,
        transition: `opacity ${FADE_MS}ms ease`,
      }}
    >
      <div
        ref={cardRef}
        tabIndex={-1}
        onMouseDown={(e) => e.stopPropagation()}
        style={{
          maxHeight: '100%',
          display: 'flex',
          outline: 'none',
          opacity: shown ? 1 : 0,
          transform: shown ? 'translateY(0) scale(1)' : 'translateY(10px) scale(0.985)',
          transition: `opacity ${FADE_MS}ms ease, transform ${FADE_MS}ms ease`,
        }}
      >
        {children}
      </div>
    </div>,
    document.body,
  );
}

/** One row in an OverlayMenu. Structurally compatible with FloatingCluster's
 *  ClusterMenuItem (kept independent here to avoid an import cycle). */
export interface OverlayMenuItem {
  label: string;
  onClick?: () => void;
  danger?: boolean;
  /** Non-interactive section heading. */
  header?: boolean;
}

const M = {
  panelBg: 'var(--hc-bg-0, #0c1622)',
  border: 'var(--hc-bg-3, #29405a)',
  fg: 'var(--hc-fg-0, #e8eef6)',
  muted: 'var(--hc-fg-2, #7d93ab)',
  danger: 'var(--hc-error, #e05656)',
} as const;
const MONO = 'var(--hc-font-mono, var(--mono, monospace))';

/** A toolbar menu rendered as a centered overlay card: a titled header (with a
 *  close affordance) over a scrollable list of items. Replaces the anchored
 *  dropdown ClusterBtn used to render. */
export function OverlayMenu({
  title,
  items,
  onClose,
}: {
  title: string;
  items: OverlayMenuItem[];
  onClose: () => void;
}) {
  return (
    <Overlay label={title} onClose={onClose} zIndex={130}>
      <div
        style={{
          width: 340,
          maxWidth: '100%',
          maxHeight: '100%',
          display: 'flex',
          flexDirection: 'column',
          borderRadius: 12,
          background: M.panelBg,
          border: `1px solid ${M.border}`,
          boxShadow: '0 16px 48px rgba(0,0,0,0.55)',
          fontFamily: MONO,
          color: M.fg,
          overflow: 'hidden',
        }}
      >
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 8,
            padding: '12px 14px',
            borderBottom: `1px solid ${M.border}`,
            flexShrink: 0,
          }}
        >
          <span
            title={title}
            style={{
              flex: 1,
              minWidth: 0,
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
              fontSize: 11,
              letterSpacing: '0.06em',
              textTransform: 'uppercase',
              color: M.muted,
            }}
          >
            {title}
          </span>
          <button
            type="button"
            aria-label={`Close ${title}`}
            onClick={onClose}
            style={{
              border: 'none',
              background: 'transparent',
              color: M.muted,
              cursor: 'pointer',
              padding: 2,
              lineHeight: 0,
            }}
          >
            <Icon name="close" size={14} />
          </button>
        </div>
        <div style={{ overflowY: 'auto', padding: '6px 0' }}>
          {items.map((item, i) =>
            item.header ? (
              <div
                key={i}
                style={{
                  padding: '8px 14px 3px',
                  marginTop: i === 0 ? 0 : 4,
                  borderTop: i === 0 ? 'none' : '1px solid rgba(255,255,255,0.06)',
                  color: M.muted,
                  fontSize: 9.5,
                  textTransform: 'uppercase',
                  letterSpacing: 0.6,
                }}
              >
                {item.label}
              </div>
            ) : (
              <button
                key={i}
                type="button"
                onClick={() => {
                  item.onClick?.();
                  onClose();
                }}
                style={{
                  display: 'block',
                  width: '100%',
                  padding: '8px 14px',
                  background: 'transparent',
                  border: 'none',
                  textAlign: 'left',
                  color: item.danger ? M.danger : M.fg,
                  fontSize: 12,
                  fontFamily: MONO,
                  cursor: 'pointer',
                }}
                onMouseEnter={(e) => {
                  e.currentTarget.style.background = 'rgba(255,255,255,0.07)';
                }}
                onMouseLeave={(e) => {
                  e.currentTarget.style.background = 'transparent';
                }}
              >
                {item.label}
              </button>
            ),
          )}
        </div>
      </div>
    </Overlay>
  );
}
