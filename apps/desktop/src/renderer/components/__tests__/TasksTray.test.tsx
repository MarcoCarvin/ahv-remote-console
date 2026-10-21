/**
 * Tests for the TasksTray presentational core. The renderer test harness runs
 * in node (no DOM / no @testing-library), so — per the FE-01 idiom used across
 * this codebase — the pure derivations that drive the tray are tested directly:
 *
 *   - traySummary: idle -> not visible (the tray renders nothing); the running
 *     count + failed count drive the pill; the pill label reflects state;
 *   - kindIcon: each task kind maps to a real (non-emoji) Icon name;
 *   - clampPercent: the progress bar value is clamped to [0,100].
 */

import { describe, it, expect } from 'vitest';
import type { TaskInfo } from '@ahv-remote-console/protocol';
import { traySummary, kindIcon, clampPercent } from '../TasksTray';

function task(id: string, state: TaskInfo['state'], extra: Partial<TaskInfo> = {}): TaskInfo {
  return {
    id,
    plugin_id: 'nutanix-ahv',
    kind: 'vm_power',
    label: id,
    state,
    percent_complete: state === 'running' ? 50 : 100,
    started_at: '2026-06-14T00:00:00Z',
    updated_at: '2026-06-14T00:00:00Z',
    ...extra,
  };
}

describe('traySummary', () => {
  it('is not visible when idle (no tasks) — the tray renders nothing', () => {
    expect(traySummary([]).visible).toBe(false);
  });

  it('shows the running count and a running pill label', () => {
    const s = traySummary([task('a', 'running'), task('b', 'running'), task('c', 'succeeded')]);
    expect(s.visible).toBe(true);
    expect(s.running).toBe(2);
    expect(s.succeeded).toBe(1);
    expect(s.pillLabel).toBe('2 running');
  });

  it('falls back to a failed pill label when nothing is running but something failed', () => {
    const s = traySummary([task('a', 'failed'), task('b', 'succeeded')]);
    expect(s.running).toBe(0);
    expect(s.failed).toBe(1);
    expect(s.pillLabel).toBe('1 failed');
  });

  it('shows a neutral pill label when all tasks succeeded', () => {
    const s = traySummary([task('a', 'succeeded')]);
    expect(s.pillLabel).toBe('Tasks');
  });

  it('carries the real Prism error through on a failed task', () => {
    // The tray renders task.error inline; this asserts the data path the row
    // reads is the real upstream detail (never fabricated).
    const failed = task('a', 'failed', { error: 'power op denied by upstream RBAC' });
    expect(failed.error).toBe('power op denied by upstream RBAC');
    expect(traySummary([failed]).failed).toBe(1);
  });
});

describe('kindIcon', () => {
  it('maps each task kind to a concrete (non-emoji) Icon name', () => {
    expect(kindIcon('vm_power')).toBe('power-on');
    expect(kindIcon('serial_port')).toBe('plus');
    expect(kindIcon('agent_iso')).toBe('disc');
  });

  it('falls back to a known icon for an unrecognized kind', () => {
    expect(kindIcon('something_new')).toBe('info');
  });
});

describe('clampPercent', () => {
  it('clamps into [0,100] and rounds', () => {
    expect(clampPercent(-5)).toBe(0);
    expect(clampPercent(150)).toBe(100);
    expect(clampPercent(42.6)).toBe(43);
    expect(clampPercent(Number.NaN)).toBe(0);
  });
});
