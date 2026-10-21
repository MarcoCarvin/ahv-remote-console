import { AnimatePresence, m } from 'framer-motion';
import { type ReactNode, useEffect, useMemo, useRef, useState } from 'react';
import { useFocusTrap } from '../hooks/useFocusTrap';

export interface CommandItem {
  id: string;
  label: string;
  hint?: string;
  group?: string;
  icon?: ReactNode;
  /** Lowercase keywords to match. */
  keywords?: string[];
  onRun: () => void;
}

export interface CommandPaletteProps {
  open: boolean;
  onClose: () => void;
  items: CommandItem[];
  placeholder?: string;
}

export function CommandPalette({
  open,
  onClose,
  items,
  placeholder = 'Type a command…',
}: CommandPaletteProps) {
  const inputRef = useRef<HTMLInputElement>(null);
  const cardRef = useRef<HTMLDivElement>(null);
  const [query, setQuery] = useState('');
  const [active, setActive] = useState(0);

  // Trap focus inside the palette card while open + restore on close (a11y:
  // the modal owns keyboard focus, no tabbing out to the page behind it).
  useFocusTrap(cardRef, open);

  useEffect(() => {
    if (!open) return;
    setQuery('');
    setActive(0);
    const t = setTimeout(() => inputRef.current?.focus(), 20);
    return () => clearTimeout(t);
  }, [open]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return items;
    return items.filter((i) => {
      const haystack = [
        i.label.toLowerCase(),
        ...(i.keywords ?? []),
        i.hint?.toLowerCase() ?? '',
        i.group?.toLowerCase() ?? '',
      ].join(' ');
      return q.split(/\s+/).every((tok) => haystack.includes(tok));
    });
  }, [items, query]);

  useEffect(() => {
    if (active >= filtered.length) setActive(0);
  }, [filtered.length, active]);

  if (!open) return null;

  return (
    <AnimatePresence>
      <m.div
        key="palette-backdrop"
        initial={{ opacity: 0 }}
        animate={{ opacity: 1 }}
        exit={{ opacity: 0 }}
        onClick={onClose}
        style={{
          position: 'fixed',
          inset: 0,
          background: 'rgba(0,0,0,0.4)',
          backdropFilter: 'blur(2px)',
          zIndex: 80,
          display: 'flex',
          justifyContent: 'center',
          paddingTop: 96,
        }}
      >
        <m.div
          key="palette-card"
          ref={cardRef}
          role="dialog"
          aria-modal="true"
          aria-label="Command palette"
          initial={{ y: 8, opacity: 0, scale: 0.98 }}
          animate={{ y: 0, opacity: 1, scale: 1 }}
          exit={{ y: -4, opacity: 0, scale: 0.98 }}
          transition={{ duration: 0.16, ease: 'easeOut' }}
          onClick={(e) => e.stopPropagation()}
          style={{
            width: 520,
            maxWidth: 'calc(100vw - 48px)',
            background: 'var(--hc-bg-glass)',
            border: '1px solid var(--hc-border-1)',
            borderRadius: 'var(--hc-radius-lg)',
            boxShadow: 'var(--hc-shadow-deep)',
            backdropFilter: 'blur(24px) saturate(160%)',
            overflow: 'hidden',
          }}
        >
          <input
            ref={inputRef}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'ArrowDown') {
                e.preventDefault();
                setActive((a) => Math.min(filtered.length - 1, a + 1));
              } else if (e.key === 'ArrowUp') {
                e.preventDefault();
                setActive((a) => Math.max(0, a - 1));
              } else if (e.key === 'Enter') {
                e.preventDefault();
                const item = filtered[active];
                if (item) {
                  item.onRun();
                  onClose();
                }
              } else if (e.key === 'Escape') {
                onClose();
              }
            }}
            placeholder={placeholder}
            style={{
              width: '100%',
              padding: '16px 18px',
              background: 'transparent',
              border: 0,
              outline: 'none',
              color: 'var(--hc-fg-0)',
              fontSize: 15,
            }}
          />
          <div
            style={{ maxHeight: 360, overflowY: 'auto', borderTop: '1px solid var(--hc-border-0)' }}
          >
            {filtered.length === 0 ? (
              <div style={{ padding: 20, color: 'var(--hc-fg-2)', fontSize: 13 }}>No matches.</div>
            ) : null}
            {filtered.map((item, idx) => (
              <button
                key={item.id}
                onMouseEnter={() => setActive(idx)}
                onClick={() => {
                  item.onRun();
                  onClose();
                }}
                style={{
                  width: '100%',
                  display: 'flex',
                  alignItems: 'center',
                  gap: 12,
                  padding: '10px 16px',
                  background: idx === active ? 'var(--hc-bg-elev)' : 'transparent',
                  color: 'var(--hc-fg-0)',
                  textAlign: 'left',
                }}
              >
                {item.icon ? <span style={{ color: 'var(--hc-fg-2)' }}>{item.icon}</span> : null}
                <span style={{ flex: 1 }}>{item.label}</span>
                {item.hint ? (
                  <span style={{ fontSize: 11, color: 'var(--hc-fg-2)' }}>{item.hint}</span>
                ) : null}
              </button>
            ))}
          </div>
        </m.div>
      </m.div>
    </AnimatePresence>
  );
}
