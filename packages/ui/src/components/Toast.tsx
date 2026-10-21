import { AnimatePresence, m } from 'framer-motion';
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react';

export type ToastKind = 'info' | 'success' | 'warn' | 'error';

export type ToastPlacement =
  | 'top-left'
  | 'top-center'
  | 'top-right'
  | 'bottom-left'
  | 'bottom-center'
  | 'bottom-right';

interface ToastEntry {
  id: string;
  kind: ToastKind;
  title: string;
  body?: string;
}

interface ToastCtx {
  push(kind: ToastKind, title: string, body?: string): void;
}

const ToastContext = createContext<ToastCtx | null>(null);

const AUTO_DISMISS_MS = 6000;

export function useToast(): ToastCtx {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error('useToast must be used inside <ToastProvider>');
  return ctx;
}

// One-shot keyframe for the auto-dismiss progress bar. CSS (not framer) drives
// it so hover can freeze it with animation-play-state in perfect step with the
// dismiss timer. Disabled under reduced-motion.
const STYLE_ID = 'hc-toast-css';
if (typeof document !== 'undefined' && !document.getElementById(STYLE_ID)) {
  const s = document.createElement('style');
  s.id = STYLE_ID;
  s.textContent = `
@keyframes hc-toast-progress { from { transform: scaleX(1); } to { transform: scaleX(0); } }
@media (prefers-reduced-motion: reduce) { .hc-toast-progress { animation: none !important; opacity: 0 !important; } }
`;
  document.head.appendChild(s);
}

export function ToastProvider({
  children,
  placement = 'top-center',
}: {
  children: ReactNode;
  /**
   * Where toasts dock. Each surface declares the corner/edge that's free in
   * ITS layout instead of a single position forced app-wide — the lobby has a
   * clear bottom-right, the VM console's bottom is occupied by its rail +
   * guest taskbar so it uses top-center.
   */
  placement?: ToastPlacement;
}) {
  const [entries, setEntries] = useState<ToastEntry[]>([]);
  const layout = placementLayout(placement);

  const dismiss = useCallback((id: string) => {
    setEntries((cur) => cur.filter((e) => e.id !== id));
  }, []);

  const push = useCallback((kind: ToastKind, title: string, body?: string) => {
    setEntries((prev) => {
      // Coalesce: if the most recent toast has the same kind/title/body, drop
      // the duplicate. Prevents a stream of identical entries when a noisy
      // event keeps re-firing in the renderer. (Each toast self-dismisses; see
      // ToastItem.)
      const last = prev[prev.length - 1];
      if (last?.kind === kind && last.title === title && last.body === body) {
        return prev;
      }
      const id = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
      return [...prev, { id, kind, title, body }];
    });
  }, []);

  // Stabilize the context value so consumers that depend on `toast` in
  // useEffect deps do not trigger render loops every time we push.
  const value = useMemo<ToastCtx>(() => ({ push }), [push]);

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div
        role="region"
        aria-label="Notifications"
        aria-live="polite"
        style={{
          position: 'fixed',
          display: 'flex',
          flexDirection: 'column',
          gap: 10,
          zIndex: 200,
          pointerEvents: 'none',
          maxWidth: 'calc(100vw - 40px)',
          ...layout.container,
        }}
      >
        <AnimatePresence initial={false}>
          {entries.map((e) => (
            <ToastItem key={e.id} entry={e} enterY={layout.enterY} onDismiss={dismiss} />
          ))}
        </AnimatePresence>
      </div>
    </ToastContext.Provider>
  );
}

function ToastItem({
  entry,
  enterY,
  onDismiss,
}: {
  entry: ToastEntry;
  enterY: number;
  onDismiss: (id: string) => void;
}) {
  const meta = KIND_META[entry.kind];
  const [paused, setPaused] = useState(false);
  // Self-managed dismiss timer that pauses on hover and resumes with the time
  // that was left — so reading a toast never races the clock out from under you.
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const remaining = useRef(AUTO_DISMISS_MS);
  const startedAt = useRef(0);

  const clear = () => {
    if (timer.current) {
      clearTimeout(timer.current);
      timer.current = null;
    }
  };
  const arm = (ms: number) => {
    clear();
    startedAt.current = Date.now();
    timer.current = setTimeout(() => onDismiss(entry.id), ms);
  };

  useEffect(() => {
    arm(AUTO_DISMISS_MS);
    return clear;
  }, []);

  const pause = () => {
    if (timer.current) {
      remaining.current = Math.max(0, remaining.current - (Date.now() - startedAt.current));
      clear();
    }
    setPaused(true);
  };
  const resume = () => {
    setPaused(false);
    arm(remaining.current);
  };

  return (
    <m.div
      role={entry.kind === 'error' ? 'alert' : 'status'}
      onMouseEnter={pause}
      onMouseLeave={resume}
      initial={{ opacity: 0, y: enterY, scale: 0.96 }}
      animate={{ opacity: 1, y: 0, scale: 1 }}
      exit={{ opacity: 0, scale: 0.96, transition: { duration: 0.16, ease: 'easeIn' } }}
      transition={{ type: 'spring', stiffness: 380, damping: 30, mass: 0.8 }}
      style={{
        position: 'relative',
        overflow: 'hidden',
        pointerEvents: 'auto',
        display: 'flex',
        gap: 11,
        minWidth: 300,
        maxWidth: 380,
        padding: '13px 32px 13px 14px',
        background: 'var(--hc-bg-glass)',
        backdropFilter: 'blur(14px)',
        WebkitBackdropFilter: 'blur(14px)',
        border: `1px solid ${paused ? 'var(--hc-border-strong)' : 'var(--hc-border-1)'}`,
        borderRadius: 'var(--hc-radius-lg)',
        boxShadow: 'var(--hc-shadow-deep)',
        fontFamily: 'var(--hc-font-sans)',
        transition: 'border-color 160ms ease',
      }}
    >
      {/* Tinted icon chip carries the severity color — no loud full-card tint. */}
      <span
        aria-hidden
        style={{
          flexShrink: 0,
          width: 26,
          height: 26,
          marginTop: 1,
          borderRadius: 8,
          display: 'grid',
          placeItems: 'center',
          fontSize: 13,
          fontWeight: 700,
          lineHeight: 1,
          color: meta.color,
          background: `color-mix(in srgb, ${meta.color} 18%, transparent)`,
        }}
      >
        {meta.glyph}
      </span>
      {/* Text. */}
      <div style={{ flex: 1, minWidth: 0, paddingTop: 1 }}>
        <div style={{ fontWeight: 600, fontSize: 13, lineHeight: 1.3, color: 'var(--hc-fg-0)' }}>
          {entry.title}
        </div>
        {entry.body ? (
          <div
            style={{
              marginTop: 3,
              fontSize: 12,
              lineHeight: 1.5,
              color: 'var(--hc-fg-1)',
              wordBreak: 'break-word',
            }}
          >
            {entry.body}
          </div>
        ) : null}
      </div>
      {/* Manual dismiss. */}
      <button
        type="button"
        aria-label="Dismiss notification"
        onClick={() => onDismiss(entry.id)}
        style={{
          position: 'absolute',
          top: 9,
          right: 9,
          width: 18,
          height: 18,
          display: 'grid',
          placeItems: 'center',
          border: 'none',
          background: 'transparent',
          color: 'var(--hc-fg-3)',
          cursor: 'pointer',
          borderRadius: 6,
          fontSize: 13,
          lineHeight: 1,
          padding: 0,
          transition: 'color 120ms ease, background 120ms ease',
        }}
        onMouseEnter={(ev) => {
          ev.currentTarget.style.color = 'var(--hc-fg-0)';
          ev.currentTarget.style.background = 'var(--hc-bg-3)';
        }}
        onMouseLeave={(ev) => {
          ev.currentTarget.style.color = 'var(--hc-fg-3)';
          ev.currentTarget.style.background = 'transparent';
        }}
      >
        ✕
      </button>
      {/* Auto-dismiss progress — CSS-driven so hover freezes it in step with
          the timer (animation-play-state). Shrinks left→right. */}
      <span
        aria-hidden
        className="hc-toast-progress"
        style={{
          position: 'absolute',
          left: 0,
          right: 0,
          bottom: 0,
          height: 2,
          background: meta.color,
          opacity: 0.55,
          transformOrigin: 'left',
          animation: `hc-toast-progress ${AUTO_DISMISS_MS}ms linear forwards`,
          animationPlayState: paused ? 'paused' : 'running',
        }}
      />
    </m.div>
  );
}

interface PlacementLayout {
  /** Positioning/anchor styles for the toast container. */
  container: React.CSSProperties;
  /** Vertical slide-in offset (negative for top-docked, positive for bottom). */
  enterY: number;
}

// Derive container anchoring + slide direction from the declared placement, so
// each surface positions notifications in its own free space rather than a
// single corner forced everywhere. Top edges clear chrome with a 52px inset
// (console top rail / lobby header); bottom edges sit 20px from the edge.
function placementLayout(p: ToastPlacement): PlacementLayout {
  const top = p.startsWith('top');
  const container: React.CSSProperties = top ? { top: 52 } : { bottom: 20 };
  if (p.endsWith('center')) {
    container.left = '50%';
    container.transform = 'translateX(-50%)';
    container.alignItems = 'center';
  } else if (p.endsWith('left')) {
    container.left = 20;
    container.alignItems = 'flex-start';
  } else {
    container.right = 20;
    container.alignItems = 'flex-end';
  }
  return { container, enterY: top ? -16 : 16 };
}

interface KindMeta {
  color: string;
  glyph: string;
}

const KIND_META: Record<ToastKind, KindMeta> = {
  success: { color: 'var(--hc-success)', glyph: '✓' },
  warn: { color: 'var(--hc-warning)', glyph: '!' },
  error: { color: 'var(--hc-error)', glyph: '✕' },
  info: { color: 'var(--hc-info)', glyph: 'i' },
};
