/**
 * StatsPanel — a read-only live VM utilization panel for the console.
 *
 * Pure presentational: it renders the `VMStats` sample handed to it (CPU% /
 * Mem% from ppm, plus disk read/write and net rx/tx as bytes/sec) and never
 * touches the SDK. Polling + fetching live in ConsoleView. A metric equal to
 * the `-1` UNAVAILABLE sentinel renders as a muted 'n/a', not a misleading 0.
 *
 * The formatting helpers (ppmToPercent / formatBytesPerSec / formatPpmPercent)
 * are exported pure functions so they can be unit-tested without rendering —
 * matching the serialCopy.ts / capabilityGate.ts pure-function idiom.
 */

import type { VMStats } from '@ahv-remote-console/sdk';
import { Icon } from '../Icon';

/** The runtime sentinel for an unavailable metric (see protocol VMStats). */
const UNAVAILABLE = -1;

/** ppm (parts-per-million) → percent. -1 (unavailable) passes through as -1. */
// eslint-disable-next-line react-refresh/only-export-components -- intentional: testable pure helper co-located for unit tests
export function ppmToPercent(ppm: number): number {
  if (ppm === UNAVAILABLE) return UNAVAILABLE;
  return ppm / 10000;
}

/**
 * Format a ppm metric as a percentage string, or 'n/a' when unavailable.
 * One decimal place keeps a 0.x% sliver visible without noise.
 */
// eslint-disable-next-line react-refresh/only-export-components -- intentional: testable pure helper co-located for unit tests
export function formatPpmPercent(ppm: number): string {
  const pct = ppmToPercent(ppm);
  if (pct === UNAVAILABLE) return 'n/a';
  return `${pct.toFixed(1)}%`;
}

/**
 * Humanize a bytes-per-second rate (1024-based) as e.g. '1.5 MB/s', or 'n/a'
 * when the metric is the unavailable sentinel. A real 0 renders as '0 B/s'.
 */
// eslint-disable-next-line react-refresh/only-export-components -- intentional: testable pure helper co-located for unit tests
export function formatBytesPerSec(bps: number): string {
  if (bps === UNAVAILABLE) return 'n/a';
  if (bps < 1024) return `${bps} B/s`;
  const units = ['KB/s', 'MB/s', 'GB/s', 'TB/s'];
  let value = bps / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(1)} ${units[unit]}`;
}

const C = {
  panelBg: 'var(--hc-bg-0, #0c1622)',
  border: 'var(--hc-bg-3, #29405a)',
  fg: 'var(--hc-fg-0, #e8eef6)',
  muted: 'var(--hc-fg-2, #7d93ab)',
  accent: 'var(--hc-accent-500, #4ea3e0)',
  barTrack: 'rgba(255,255,255,0.08)',
} as const;

const FONT_MONO = 'var(--hc-font-mono, var(--mono, monospace))';

/** A single labeled metric row; `na` mutes the value when unavailable. */
function MetricRow({ label, value, na }: { label: string; value: string; na: boolean }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', gap: 16, lineHeight: '20px' }}>
      <span style={{ color: C.muted }}>{label}</span>
      <span style={{ color: na ? C.muted : C.fg, fontVariantNumeric: 'tabular-nums' }}>
        {value}
      </span>
    </div>
  );
}

/** A small percent bar (CPU / Mem). Hidden track + fill; n/a draws no fill. */
function PercentBar({ ppm }: { ppm: number }) {
  const pct = ppmToPercent(ppm);
  const na = pct === UNAVAILABLE;
  const width = na ? 0 : Math.max(0, Math.min(100, pct));
  return (
    <div
      style={{
        height: 4,
        marginTop: 3,
        borderRadius: 2,
        background: C.barTrack,
        overflow: 'hidden',
      }}
    >
      <div style={{ height: '100%', width: `${width}%`, background: C.accent }} />
    </div>
  );
}

export interface StatsPanelProps {
  stats: VMStats | null;
  loading: boolean;
  error: string | null;
  /** Dismiss the panel. Rendered as a header close button when provided
   *  (the panel is shown as a centered overlay; Esc / backdrop also dismiss). */
  onClose?: () => void;
}

export function StatsPanel({ stats, loading, error, onClose }: StatsPanelProps) {
  return (
    <div
      style={{
        minWidth: 260,
        maxWidth: 360,
        padding: '12px 14px',
        borderRadius: 12,
        background: C.panelBg,
        border: `1px solid ${C.border}`,
        boxShadow: '0 16px 48px rgba(0,0,0,0.55)',
        fontFamily: FONT_MONO,
        fontSize: 12,
        color: C.fg,
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', marginBottom: 8 }}>
        <span
          style={{
            flex: 1,
            fontSize: 11,
            letterSpacing: '0.06em',
            textTransform: 'uppercase',
            color: C.muted,
          }}
        >
          Live VM stats
        </span>
        {onClose && (
          <button
            type="button"
            aria-label="Close live VM stats"
            onClick={onClose}
            style={{
              border: 'none',
              background: 'transparent',
              color: C.muted,
              cursor: 'pointer',
              padding: 2,
              lineHeight: 0,
            }}
          >
            <Icon name="close" size={14} />
          </button>
        )}
      </div>

      {error ? (
        <div style={{ color: C.muted }}>Couldn&apos;t load stats: {error}</div>
      ) : !stats ? (
        <div style={{ color: C.muted }}>{loading ? 'Loading…' : 'No data'}</div>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
          <div>
            <MetricRow
              label="CPU"
              value={formatPpmPercent(stats.cpu_usage_ppm)}
              na={stats.cpu_usage_ppm === UNAVAILABLE}
            />
            <PercentBar ppm={stats.cpu_usage_ppm} />
          </div>
          <div>
            <MetricRow
              label="Memory"
              value={formatPpmPercent(stats.memory_usage_ppm)}
              na={stats.memory_usage_ppm === UNAVAILABLE}
            />
            <PercentBar ppm={stats.memory_usage_ppm} />
          </div>
          <MetricRow
            label="Disk read"
            value={formatBytesPerSec(stats.disk_read_bps)}
            na={stats.disk_read_bps === UNAVAILABLE}
          />
          <MetricRow
            label="Disk write"
            value={formatBytesPerSec(stats.disk_write_bps)}
            na={stats.disk_write_bps === UNAVAILABLE}
          />
          <MetricRow
            label="Net Rx"
            value={formatBytesPerSec(stats.net_rx_bps)}
            na={stats.net_rx_bps === UNAVAILABLE}
          />
          <MetricRow
            label="Net Tx"
            value={formatBytesPerSec(stats.net_tx_bps)}
            na={stats.net_tx_bps === UNAVAILABLE}
          />
        </div>
      )}
    </div>
  );
}
