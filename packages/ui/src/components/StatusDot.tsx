import { m } from 'framer-motion';

export type StatusKind = 'ok' | 'warn' | 'error' | 'idle' | 'pending';

const COLORS: Record<StatusKind, string> = {
  ok: 'var(--hc-success)',
  warn: 'var(--hc-warning)',
  error: 'var(--hc-error)',
  idle: 'var(--hc-fg-3)',
  pending: 'var(--hc-info)',
};

export interface StatusDotProps {
  kind: StatusKind;
  pulse?: boolean;
  size?: number;
  label?: string;
}

export function StatusDot({ kind, pulse = false, size = 8, label }: StatusDotProps) {
  const color = COLORS[kind];
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }} aria-label={label}>
      <span style={{ position: 'relative', width: size, height: size }}>
        <span
          style={{
            position: 'absolute',
            inset: 0,
            borderRadius: '50%',
            background: color,
          }}
        />
        {pulse ? (
          <m.span
            aria-hidden
            initial={{ scale: 1, opacity: 0.7 }}
            animate={{ scale: 2.6, opacity: 0 }}
            transition={{ duration: 1.6, repeat: Infinity, ease: 'easeOut' }}
            style={{
              position: 'absolute',
              inset: 0,
              borderRadius: '50%',
              background: color,
            }}
          />
        ) : null}
      </span>
      {label ? <span style={{ color: 'var(--hc-fg-1)', fontSize: 12 }}>{label}</span> : null}
    </span>
  );
}
