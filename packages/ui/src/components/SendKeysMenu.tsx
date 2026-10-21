import { m, AnimatePresence } from 'framer-motion';
import { useState, type ReactNode } from 'react';

export interface SendKeyCombo {
  id: string;
  label: string;
  /** Pretty-printed binding shown to the operator. */
  hint?: string;
  /** Whether the key sequence is dangerous in some sense. */
  destructive?: boolean;
  onSend: () => void;
}

export interface SendKeysMenuProps {
  combos: SendKeyCombo[];
  trigger?: ReactNode;
}

/**
 * Drop-down used by the console toolbar to send OS-intercepted combinations
 * to the guest VM. Each combo is a synthetic RFB keyEvent burst.
 *
 * The list of well-known combos lives next to this component in
 * `defaultSendKeys()` so consumers can pre-populate it without re-typing
 * the X11 keysyms.
 */
export function SendKeysMenu({ combos, trigger }: SendKeysMenuProps) {
  const [open, setOpen] = useState(false);
  return (
    <span style={{ position: 'relative', display: 'inline-flex' }}>
      <button
        onClick={() => setOpen((v) => !v)}
        style={{
          padding: '6px 10px',
          background: open ? 'var(--hc-bg-elev)' : 'transparent',
          color: 'var(--hc-fg-1)',
          border: '1px solid var(--hc-border-1)',
          borderRadius: 'var(--hc-radius-md)',
          fontSize: 12,
          cursor: 'pointer',
        }}
      >
        {trigger ?? 'Send Keys ▾'}
      </button>
      <AnimatePresence>
        {open ? (
          <>
            <span
              onClick={() => setOpen(false)}
              style={{ position: 'fixed', inset: 0, zIndex: 60 }}
            />
            <m.div
              role="menu"
              initial={{ opacity: 0, y: -4, scale: 0.98 }}
              animate={{ opacity: 1, y: 0, scale: 1 }}
              exit={{ opacity: 0, y: -4, scale: 0.98 }}
              transition={{ duration: 0.14, ease: 'easeOut' }}
              style={{
                position: 'absolute',
                top: 'calc(100% + 6px)',
                right: 0,
                minWidth: 240,
                background: 'var(--hc-bg-2)',
                border: '1px solid var(--hc-border-1)',
                borderRadius: 'var(--hc-radius-md)',
                boxShadow: 'var(--hc-shadow-soft)',
                padding: 4,
                zIndex: 70,
              }}
            >
              {combos.map((c) => (
                <button
                  key={c.id}
                  role="menuitem"
                  onClick={() => {
                    c.onSend();
                    setOpen(false);
                  }}
                  style={{
                    width: '100%',
                    display: 'flex',
                    alignItems: 'center',
                    justifyContent: 'space-between',
                    gap: 12,
                    padding: '8px 10px',
                    color: c.destructive ? 'var(--hc-error)' : 'var(--hc-fg-0)',
                    background: 'transparent',
                    borderRadius: 'var(--hc-radius-sm)',
                    textAlign: 'left',
                    fontSize: 12,
                    cursor: 'pointer',
                  }}
                  onMouseEnter={(e) => {
                    (e.currentTarget as HTMLElement).style.background = 'var(--hc-bg-elev)';
                  }}
                  onMouseLeave={(e) => {
                    (e.currentTarget as HTMLElement).style.background = 'transparent';
                  }}
                >
                  <span>{c.label}</span>
                  {c.hint ? (
                    <span style={{ fontSize: 11, color: 'var(--hc-fg-3)' }}>{c.hint}</span>
                  ) : null}
                </button>
              ))}
            </m.div>
          </>
        ) : null}
      </AnimatePresence>
    </span>
  );
}
