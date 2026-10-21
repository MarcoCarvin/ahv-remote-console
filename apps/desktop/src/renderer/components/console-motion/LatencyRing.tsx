/**
 * LatencyRing (C2) — a small dot with an expanding "ping" ring whose
 * cadence tracks the round-trip time. Mounts inline in the 28px
 * titlebar, next to the millisecond text.
 *
 *   cadence = clamp(rttMs, 800, 3000) ms   (slower ring = higher RTT)
 *   colour  = green  < 50ms
 *             amber  50–150ms
 *             red    > 150ms
 *
 * Reduced motion: CSS hard-gate freezes the ping ring to a static
 * outline (no expand). Colour/dot remain.
 */

import { latencyCadenceMs, latencyColor } from './signal';
import './consoleMotion.css';

export interface LatencyRingProps {
  rttMs: number;
}

export function LatencyRing({ rttMs }: LatencyRingProps) {
  const color = latencyColor(rttMs);
  const cadence = latencyCadenceMs(rttMs);

  return (
    <span
      className="cm-root cm-ring"
      style={
        {
          '--cm-ping': color,
          '--cm-ping-dur': `${cadence}ms`,
        } as React.CSSProperties
      }
      aria-hidden
    >
      <span className="cm-ring__pulse" />
      <span className="cm-ring__dot" />
    </span>
  );
}

export default LatencyRing;
