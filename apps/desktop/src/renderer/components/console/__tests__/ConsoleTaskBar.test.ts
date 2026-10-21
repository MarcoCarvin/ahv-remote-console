import { describe, expect, it } from 'vitest';
import type { TaskInfo } from '@ahv-remote-console/protocol';
import { aggregateRunning } from '../ConsoleTaskBar';

function task(p: Partial<TaskInfo>): TaskInfo {
  return {
    id: 'id',
    plugin_id: 'nutanix-ahv',
    kind: 'agent_iso',
    label: 'Install agent on vm',
    state: 'running',
    percent_complete: 0,
    started_at: '',
    updated_at: '',
    ...p,
  };
}

describe('aggregateRunning', () => {
  it('returns null when nothing is running', () => {
    expect(aggregateRunning([])).toBeNull();
    expect(aggregateRunning([task({ state: 'succeeded' }), task({ state: 'failed' })])).toBeNull();
  });

  it('surfaces a single running task with its label, detail and percent', () => {
    const agg = aggregateRunning([
      task({ label: 'Install agent on web01', detail: 'Uploading bytes', percent_complete: 62 }),
    ]);
    expect(agg).toEqual({
      count: 1,
      label: 'Install agent on web01',
      detail: 'Uploading bytes',
      percent: 62,
      indeterminate: false,
    });
  });

  it('is indeterminate while a running task reports no percent yet', () => {
    const agg = aggregateRunning([task({ percent_complete: 0 })]);
    expect(agg?.indeterminate).toBe(true);
    expect(agg?.percent).toBe(0);
  });

  it('averages percent across several running tasks and labels the count', () => {
    const agg = aggregateRunning([
      task({ kind: 'vm_power', label: 'Power OFF vm', percent_complete: 80 }),
      task({ kind: 'agent_iso', label: 'Install agent', percent_complete: 20 }),
    ]);
    expect(agg?.count).toBe(2);
    expect(agg?.label).toBe('2 tasks running');
    expect(agg?.detail).toBeUndefined();
    expect(agg?.percent).toBe(50);
    expect(agg?.indeterminate).toBe(false);
  });

  it('ignores terminal tasks when aggregating the running ones', () => {
    const agg = aggregateRunning([
      task({ state: 'succeeded', percent_complete: 100 }),
      task({ label: 'Mounting ISO', percent_complete: 40 }),
    ]);
    expect(agg?.count).toBe(1);
    expect(agg?.label).toBe('Mounting ISO');
    expect(agg?.percent).toBe(40);
  });

  it('clamps a malformed percent', () => {
    expect(aggregateRunning([task({ percent_complete: 999 })])?.percent).toBe(100);
    expect(aggregateRunning([task({ percent_complete: Number.NaN })])?.percent).toBe(0);
  });
});
