/**
 * Tests for the StatsPanel pure formatters. The renderer test harness runs in
 * node (no DOM / no @testing-library), so — per the FE-01 idiom used across this
 * codebase (see TasksTray.test.tsx) — the pure functions that drive the panel
 * are tested directly:
 *
 *   - ppmToPercent: ppm/10000 conversion, with the -1 sentinel passing through;
 *   - formatPpmPercent: percent string, and the -1 -> 'n/a' rule;
 *   - formatBytesPerSec: 1024-based humanization, real 0 vs the -1 'n/a' rule.
 */

import { describe, it, expect } from 'vitest';
import { ppmToPercent, formatPpmPercent, formatBytesPerSec } from '../StatsPanel';

describe('ppmToPercent', () => {
  it('converts parts-per-million to percent (/10000)', () => {
    expect(ppmToPercent(0)).toBe(0);
    expect(ppmToPercent(1000000)).toBe(100);
    expect(ppmToPercent(250000)).toBe(25);
    expect(ppmToPercent(5000)).toBe(0.5);
  });

  it('passes the -1 unavailable sentinel through unchanged', () => {
    expect(ppmToPercent(-1)).toBe(-1);
  });
});

describe('formatPpmPercent', () => {
  it('renders a one-decimal percent', () => {
    expect(formatPpmPercent(250000)).toBe('25.0%');
    expect(formatPpmPercent(5000)).toBe('0.5%');
    expect(formatPpmPercent(0)).toBe('0.0%');
  });

  it("renders the -1 sentinel as 'n/a', not 0%", () => {
    expect(formatPpmPercent(-1)).toBe('n/a');
  });
});

describe('formatBytesPerSec', () => {
  it('renders sub-KB rates in B/s, including a real 0', () => {
    expect(formatBytesPerSec(0)).toBe('0 B/s');
    expect(formatBytesPerSec(512)).toBe('512 B/s');
  });

  it('humanizes KB / MB / GB (1024-based)', () => {
    expect(formatBytesPerSec(1024)).toBe('1.0 KB/s');
    expect(formatBytesPerSec(1536)).toBe('1.5 KB/s');
    expect(formatBytesPerSec(1048576)).toBe('1.0 MB/s');
    expect(formatBytesPerSec(1073741824)).toBe('1.0 GB/s');
  });

  it("renders the -1 sentinel as 'n/a', distinct from a real 0", () => {
    expect(formatBytesPerSec(-1)).toBe('n/a');
    expect(formatBytesPerSec(0)).not.toBe('n/a');
  });
});
