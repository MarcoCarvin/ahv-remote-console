/**
 * Tests for the Tasks-tray polling core (createTaskPoller) and runningCount.
 *
 * FE-01 idiom: the React hook (useTasks) is a thin binding, so the testable
 * logic — poll-while-running, stop-when-terminal, refresh-forces-a-poll — lives
 * in the framework-agnostic createTaskPoller. The interval primitives are
 * injected so the test drives them deterministically without real timers.
 */

import { describe, it, expect, vi } from 'vitest';
import type { TaskInfo } from '@ahv-remote-console/protocol';
import { createTaskPoller, runningCount, type TasksClient } from '../useTasks';

function task(id: string, state: TaskInfo['state']): TaskInfo {
  return {
    id,
    plugin_id: 'nutanix-ahv',
    kind: 'vm_power',
    label: id,
    state,
    percent_complete: state === 'running' ? 30 : 100,
    started_at: '2026-06-14T00:00:00Z',
    updated_at: '2026-06-14T00:00:00Z',
  };
}

/** A controllable interval shim: the test fires ticks by hand. */
function fakeIntervals() {
  let fn: (() => void) | undefined;
  let active = false;
  return {
    hooks: {
      setInterval: (cb: () => void) => {
        fn = cb;
        active = true;
        return 1 as unknown as ReturnType<typeof setInterval>;
      },
      clearInterval: () => {
        active = false;
        fn = undefined;
      },
    },
    isActive: () => active,
    tick: () => fn?.(),
  };
}

/** Stub client that resolves listTasks with a queue of snapshots (last sticks). */
function queueClient(snapshots: TaskInfo[][]): { client: TasksClient; calls: () => number } {
  let i = 0;
  let calls = 0;
  return {
    client: {
      listTasks: () => {
        calls++;
        const snap = snapshots[Math.min(i, snapshots.length - 1)] ?? [];
        i++;
        return Promise.resolve({ tasks: snap });
      },
    },
    calls: () => calls,
  };
}

describe('runningCount', () => {
  it('counts only running tasks', () => {
    expect(runningCount([task('a', 'running'), task('b', 'succeeded'), task('c', 'running')])).toBe(
      2,
    );
    expect(runningCount([task('a', 'succeeded')])).toBe(0);
    expect(runningCount([])).toBe(0);
  });
});

describe('createTaskPoller', () => {
  it('polls once on creation and starts the interval while a task is running', async () => {
    const fi = fakeIntervals();
    const { client, calls } = queueClient([[task('a', 'running')]]);
    const seen: TaskInfo[][] = [];
    createTaskPoller(client, 1500, { onTasks: (t) => seen.push(t), ...fi.hooks });

    await vi.waitFor(() => expect(calls()).toBe(1));
    expect(seen.at(-1)).toEqual([task('a', 'running')]);
    expect(fi.isActive()).toBe(true);
  });

  it('stops the interval once every task is terminal', async () => {
    const fi = fakeIntervals();
    // First poll: running (interval starts). Second poll (via tick): terminal.
    const { client } = queueClient([[task('a', 'running')], [task('a', 'succeeded')]]);
    const seen: TaskInfo[][] = [];
    createTaskPoller(client, 1500, { onTasks: (t) => seen.push(t), ...fi.hooks });

    await vi.waitFor(() => expect(fi.isActive()).toBe(true));
    fi.tick(); // drive the second poll
    await vi.waitFor(() => expect(fi.isActive()).toBe(false));
    expect(seen.at(-1)).toEqual([task('a', 'succeeded')]);
  });

  it('refresh() forces an immediate extra listTasks', async () => {
    const fi = fakeIntervals();
    const { client, calls } = queueClient([[]]); // nothing running
    const poller = createTaskPoller(client, 1500, {
      onTasks: () => {
        /* noop */
      },
      ...fi.hooks,
    });

    await vi.waitFor(() => expect(calls()).toBe(1));
    // Idle: no interval running.
    expect(fi.isActive()).toBe(false);
    poller.refresh();
    await vi.waitFor(() => expect(calls()).toBe(2));
  });

  it('keeps the last snapshot when a poll fails (no blank)', async () => {
    const fi = fakeIntervals();
    let n = 0;
    const client: TasksClient = {
      listTasks: () => {
        n++;
        if (n === 1) return Promise.resolve({ tasks: [task('a', 'running')] });
        return Promise.reject(new Error('transient list failure'));
      },
    };
    const seen: TaskInfo[][] = [];
    const poller = createTaskPoller(client, 1500, { onTasks: (t) => seen.push(t), ...fi.hooks });

    await vi.waitFor(() => expect(seen.length).toBe(1));
    poller.refresh(); // this one rejects
    await vi.waitFor(() => expect(n).toBe(2));
    // No new (blank) snapshot emitted from the failed poll.
    expect(seen).toEqual([[task('a', 'running')]]);
  });

  it('stop() tears down the interval and halts further emits', async () => {
    const fi = fakeIntervals();
    const { client } = queueClient([[task('a', 'running')]]);
    const seen: TaskInfo[][] = [];
    const poller = createTaskPoller(client, 1500, { onTasks: (t) => seen.push(t), ...fi.hooks });

    await vi.waitFor(() => expect(fi.isActive()).toBe(true));
    poller.stop();
    expect(fi.isActive()).toBe(false);
    const before = seen.length;
    poller.refresh(); // stopped — must be a no-op emit
    await Promise.resolve();
    expect(seen.length).toBe(before);
  });
});
