/**
 * useTasks — polls the runtime's long-running-task list for the Tasks tray.
 *
 * De-jank discipline (mirrors the MEMORY note about replacing a tight polling
 * loop with event-driven work): it polls ONLY while something is running, plus
 * once on mount, and STOPS the interval the moment every task is terminal — so
 * an idle app does no network work. refresh() forces an immediate re-poll
 * (e.g. right after firing a power/mount action) so a new task appears without
 * waiting for the next tick. Just-finished terminal tasks linger (the runtime
 * keeps them ~10m) so the tray can show a success/failure result before they
 * age out.
 *
 * The polling LOGIC lives in the framework-agnostic createTaskPoller below so it
 * is unit-testable with a stub client and fake timers (FE-01 idiom); the hook
 * only binds the poller to React state.
 */

import { useEffect, useRef, useState } from 'react';
import type { ListTasksResponse, TaskInfo } from '@ahv-remote-console/protocol';

/** The slice of the SDK client useTasks needs — narrow so tests can stub it. */
export interface TasksClient {
  listTasks(): Promise<ListTasksResponse>;
}

export interface UseTasksOptions {
  /** Poll cadence while any task is running. Kept >= 1.5s to avoid hammering
   *  Prism's tasks GET over a VPN (one upstream poll per running task). */
  pollMs?: number;
}

export interface UseTasksResult {
  tasks: TaskInfo[];
  /** Count of tasks currently running (drives the tray pill). */
  running: number;
  /** Force an immediate re-poll (imperative bump after firing an action). */
  refresh: () => void;
}

const DEFAULT_POLL_MS = 1500;

/** Count the running tasks in a snapshot. */
export function runningCount(tasks: TaskInfo[]): number {
  return tasks.reduce((n, t) => (t.state === 'running' ? n + 1 : n), 0);
}

export interface TaskPoller {
  /** Poll once now (also (re)starts the interval if a task is running). */
  refresh(): void;
  /** Tear down the interval and stop emitting. */
  stop(): void;
}

export interface TaskPollerHooks {
  /** Emitted on every successful poll (the latest snapshot). */
  onTasks(tasks: TaskInfo[]): void;
  setInterval: (fn: () => void, ms: number) => ReturnType<typeof setInterval>;
  clearInterval: (h: ReturnType<typeof setInterval>) => void;
}

/**
 * createTaskPoller drives the poll-while-running loop, decoupled from React so
 * it can be unit-tested directly:
 *   - poll once immediately on creation;
 *   - keep an interval ONLY while a task is running;
 *   - stop the interval as soon as every task is terminal;
 *   - refresh() forces an immediate poll (and restarts the interval if needed);
 *   - a list failure keeps the last snapshot (no UI blank).
 */
export function createTaskPoller(
  client: TasksClient,
  pollMs: number,
  hooks: TaskPollerHooks,
): TaskPoller {
  let interval: ReturnType<typeof setInterval> | undefined;
  let inFlight = false;
  let stopped = false;

  const stopInterval = () => {
    if (interval !== undefined) {
      hooks.clearInterval(interval);
      interval = undefined;
    }
  };

  const poll = async () => {
    if (inFlight || stopped) return; // don't stack polls if one is slow
    inFlight = true;
    try {
      const { tasks } = await client.listTasks();
      if (stopped) return;
      hooks.onTasks(tasks);
      const anyRunning = tasks.some((t) => t.state === 'running');
      if (anyRunning && interval === undefined) {
        interval = hooks.setInterval(() => void poll(), pollMs);
      } else if (!anyRunning) {
        stopInterval();
      }
    } catch {
      // A transient list failure must not blank the tray — keep the last
      // snapshot and let the next poll/refresh catch up. The runtime serving
      // 503 task_unsupported simply yields an empty tray (renders nothing).
    } finally {
      inFlight = false;
    }
  };

  void poll();

  return {
    refresh: () => void poll(),
    stop: () => {
      stopped = true;
      stopInterval();
    },
  };
}

export function useTasks(client: TasksClient, opts: UseTasksOptions = {}): UseTasksResult {
  const pollMs = opts.pollMs ?? DEFAULT_POLL_MS;
  const [tasks, setTasks] = useState<TaskInfo[]>([]);
  const pollerRef = useRef<TaskPoller | null>(null);

  useEffect(() => {
    const poller = createTaskPoller(client, pollMs, {
      onTasks: setTasks,
      setInterval: (fn, ms) => setInterval(fn, ms),
      clearInterval: (h) => clearInterval(h),
    });
    pollerRef.current = poller;
    return () => {
      poller.stop();
      pollerRef.current = null;
    };
  }, [client, pollMs]);

  return {
    tasks,
    running: runningCount(tasks),
    refresh: () => pollerRef.current?.refresh(),
  };
}
