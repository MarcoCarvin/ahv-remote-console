import { AnimatePresence, m } from 'framer-motion';
import { type ReactNode, useState, useId } from 'react';

export interface TooltipProps {
  label: ReactNode;
  children: ReactNode;
  side?: 'top' | 'bottom' | 'left' | 'right';
}

export function Tooltip({ label, children, side = 'top' }: TooltipProps) {
  const [open, setOpen] = useState(false);
  const id = useId();

  return (
    <span
      style={{ position: 'relative', display: 'inline-flex' }}
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      onFocus={() => setOpen(true)}
      onBlur={() => setOpen(false)}
      aria-describedby={open ? id : undefined}
    >
      {children}
      <AnimatePresence>
        {open ? (
          <m.span
            id={id}
            role="tooltip"
            initial={{ opacity: 0, y: side === 'top' ? 4 : -4 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: side === 'top' ? 4 : -4 }}
            transition={{ duration: 0.18, ease: 'easeOut' }}
            style={{
              position: 'absolute',
              zIndex: 50,
              ...positionFor(side),
              padding: '4px 8px',
              background: 'var(--hc-bg-3)',
              color: 'var(--hc-fg-0)',
              border: '1px solid var(--hc-border-1)',
              borderRadius: 'var(--hc-radius-sm)',
              fontSize: 11,
              whiteSpace: 'nowrap',
              boxShadow: 'var(--hc-shadow-soft)',
              pointerEvents: 'none',
            }}
          >
            {label}
          </m.span>
        ) : null}
      </AnimatePresence>
    </span>
  );
}

function positionFor(side: 'top' | 'bottom' | 'left' | 'right'): React.CSSProperties {
  switch (side) {
    case 'top':
      return { bottom: 'calc(100% + 6px)', left: '50%', transform: 'translateX(-50%)' };
    case 'bottom':
      return { top: 'calc(100% + 6px)', left: '50%', transform: 'translateX(-50%)' };
    case 'left':
      return { right: 'calc(100% + 6px)', top: '50%', transform: 'translateY(-50%)' };
    case 'right':
      return { left: 'calc(100% + 6px)', top: '50%', transform: 'translateY(-50%)' };
  }
}
