import { m } from 'framer-motion';

export interface SpinnerProps {
  size?: number;
  color?: string;
  label?: string;
}

export function Spinner({ size = 16, color = 'var(--hc-accent-400)', label }: SpinnerProps) {
  return (
    <span
      role="status"
      aria-label={label ?? 'Loading'}
      style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}
    >
      <m.span
        aria-hidden
        style={{
          display: 'inline-block',
          width: size,
          height: size,
          borderRadius: '50%',
          border: `2px solid ${color}33`,
          borderTopColor: color,
        }}
        animate={{ rotate: 360 }}
        transition={{ duration: 0.9, repeat: Infinity, ease: 'linear' }}
      />
      {label ? <span style={{ color: 'var(--hc-fg-2)', fontSize: 12 }}>{label}</span> : null}
    </span>
  );
}
