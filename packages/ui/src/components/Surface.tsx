import type { CSSProperties, ReactNode } from 'react';

export interface SurfaceProps {
  children: ReactNode;
  style?: CSSProperties;
  /** Visual elevation level 0–3. */
  level?: 0 | 1 | 2 | 3;
}

const SURFACES: Record<0 | 1 | 2 | 3, string> = {
  0: 'var(--hc-bg-0)',
  1: 'var(--hc-bg-1)',
  2: 'var(--hc-bg-2)',
  3: 'var(--hc-bg-3)',
};

export function Surface({ level = 1, style, children }: SurfaceProps) {
  return <div style={{ background: SURFACES[level], ...style }}>{children}</div>;
}
