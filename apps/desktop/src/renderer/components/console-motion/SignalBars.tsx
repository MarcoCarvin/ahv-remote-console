/**
 * SignalBars (C4) — 4 wifi-style bars with a staggered wave. Mounts in
 * the status strip.
 *
 *   active-bar count by health (when connected):
 *     rtt < 50ms   → 4 bars (green)
 *     50–150ms     → 3 bars (amber)
 *     150–300ms    → 2 bars (amber)
 *     > 300ms      → 1 bar  (red)
 *   not connected  → 0 active bars, neutral colour, no wave.
 *
 * Reduced motion: CSS hard-gate shows the active bars statically (no
 * wave). The active COUNT/colour still convey health.
 */

import { SIGNAL_BAR_COUNT, signalActiveBars, signalColor } from './signal';
import './consoleMotion.css';

export interface SignalBarsProps {
  rttMs: number;
  connected: boolean;
}

export function SignalBars({ rttMs, connected }: SignalBarsProps) {
  const active = signalActiveBars(rttMs, connected);
  const color = signalColor(rttMs, connected);

  return (
    <span
      className="cm-root cm-bars"
      style={{ '--cm-bar': color } as React.CSSProperties}
      aria-hidden
    >
      {Array.from({ length: SIGNAL_BAR_COUNT }, (_, i) => (
        <span
          key={i}
          className="cm-bars__b"
          data-on={i < active ? 'true' : 'false'}
          style={{ '--cm-bar-i': i } as React.CSSProperties}
        />
      ))}
    </span>
  );
}

export default SignalBars;
