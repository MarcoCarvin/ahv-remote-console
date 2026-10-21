import type { ReactNode } from 'react';

export function Kbd({ children }: { children: ReactNode }) {
  return (
    <kbd
      style={{
        display: 'inline-block',
        padding: '1px 6px',
        background: 'var(--hc-bg-3)',
        border: '1px solid var(--hc-border-1)',
        borderRadius: 4,
        fontFamily: 'var(--hc-font-mono)',
        fontSize: 11,
        color: 'var(--hc-fg-1)',
        boxShadow: '0 1px 0 var(--hc-border-1)',
      }}
    >
      {children}
    </kbd>
  );
}
