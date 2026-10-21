import { m } from 'framer-motion';
import { type ReactNode, useId } from 'react';

export interface TabDescriptor {
  id: string;
  label: ReactNode;
  badge?: ReactNode;
}

export interface TabsProps {
  tabs: TabDescriptor[];
  activeId: string;
  onSelect: (id: string) => void;
  onClose?: (id: string) => void;
}

export function Tabs({ tabs, activeId, onSelect, onClose }: TabsProps) {
  const layoutId = useId();
  return (
    <div role="tablist" style={{ display: 'flex', gap: 4, alignItems: 'center' }}>
      {tabs.map((t) => {
        const active = t.id === activeId;
        return (
          <div
            key={t.id}
            role="tab"
            aria-selected={active}
            tabIndex={active ? 0 : -1}
            onClick={() => onSelect(t.id)}
            style={{
              position: 'relative',
              display: 'inline-flex',
              alignItems: 'center',
              gap: 8,
              padding: '8px 12px',
              borderRadius: 'var(--hc-radius-md)',
              fontSize: 12,
              color: active ? 'var(--hc-fg-0)' : 'var(--hc-fg-2)',
              cursor: 'pointer',
              userSelect: 'none',
              transition: 'color 120ms ease',
            }}
            onKeyDown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                onSelect(t.id);
              }
            }}
          >
            {active ? (
              <m.span
                layoutId={`hc-tab-pill-${layoutId}`}
                style={{
                  position: 'absolute',
                  inset: 0,
                  background: 'var(--hc-bg-elev)',
                  border: '1px solid var(--hc-border-1)',
                  borderRadius: 'var(--hc-radius-md)',
                  zIndex: 0,
                }}
                transition={{ duration: 0.22, ease: [0.2, 0.8, 0.2, 1] }}
              />
            ) : null}
            <span style={{ position: 'relative', zIndex: 1 }}>{t.label}</span>
            {t.badge ? <span style={{ position: 'relative', zIndex: 1 }}>{t.badge}</span> : null}
            {onClose ? (
              <button
                aria-label="Close tab"
                onClick={(e) => {
                  e.stopPropagation();
                  onClose(t.id);
                }}
                style={{
                  position: 'relative',
                  zIndex: 1,
                  width: 18,
                  height: 18,
                  display: 'inline-flex',
                  alignItems: 'center',
                  justifyContent: 'center',
                  borderRadius: 4,
                  color: 'var(--hc-fg-2)',
                }}
              >
                ×
              </button>
            ) : null}
          </div>
        );
      })}
    </div>
  );
}
