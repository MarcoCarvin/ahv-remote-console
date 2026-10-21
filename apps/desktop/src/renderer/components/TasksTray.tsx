/**
 * TasksTray — a non-blocking corner tray for long-running power / serial-port /
 * agent-ISO ops. A pill button (running count + spinner) expands to a list of
 * per-task rows: kind icon, label, a thin progress bar driven by the real
 * percent_complete, and a state chip (running spinner / success dot / error dot
 * with the real Prism error inline). Idle (zero tasks) renders nothing.
 *
 * Read-only by design: it DISPLAYS in-flight ops; it never triggers them and
 * carries no retry/cancel controls (cancel would need a Prism task-abort + a
 * confirmation; out of scope). Driven by useTasks, which polls only while a
 * task is running.
 */

import { useState } from 'react';
import { Spinner, StatusDot } from '@ahv-remote-console/ui';
import type { TaskInfo } from '@ahv-remote-console/protocol';
import { Icon, type IconName } from './Icon';
import { useTasks, type TasksClient, type UseTasksResult } from '../app/useTasks';

/** A client whose listTasks never resolves, so the internal poller stays idle
 *  when a parent-supplied controller drives the tray instead. */
const NEVER: Promise<never> = new Promise<never>(() => undefined);
const noopTasksClient: TasksClient = {
  listTasks: () => NEVER,
};

const KIND_ICON: Record<string, IconName> = {
  vm_power: 'power-on',
  serial_port: 'plus',
  agent_iso: 'disc',
};

// eslint-disable-next-line react-refresh/only-export-components -- intentional: testable pure helper co-located for unit tests
export function kindIcon(kind: string): IconName {
  return KIND_ICON[kind] ?? 'info';
}

/** Clamp a percent into [0,100] so a malformed value can't break the bar. */
// eslint-disable-next-line react-refresh/only-export-components -- intentional: testable pure helper co-located for unit tests
export function clampPercent(p: number): number {
  if (!Number.isFinite(p)) return 0;
  return Math.max(0, Math.min(100, Math.round(p)));
}

/** Pure presentational derivation for the tray pill — testable without a DOM. */
export interface TraySummary {
  /** False when the tray should render nothing (no tasks). */
  visible: boolean;
  running: number;
  succeeded: number;
  failed: number;
  pillLabel: string;
}

// eslint-disable-next-line react-refresh/only-export-components -- intentional: testable pure helper co-located for unit tests
export function traySummary(tasks: TaskInfo[]): TraySummary {
  const running = tasks.reduce((n, t) => (t.state === 'running' ? n + 1 : n), 0);
  const succeeded = tasks.filter((t) => t.state === 'succeeded').length;
  const failed = tasks.filter((t) => t.state === 'failed').length;
  const pillLabel = running > 0 ? `${running} running` : failed > 0 ? `${failed} failed` : 'Tasks';
  return { visible: tasks.length > 0, running, succeeded, failed, pillLabel };
}

function TaskRow({ task }: { task: TaskInfo }) {
  const running = task.state === 'running';
  const failed = task.state === 'failed';
  const pct = clampPercent(task.percent_complete);
  return (
    <div
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 6,
        padding: '8px 10px',
        borderRadius: 6,
        background: 'rgba(255,255,255,.04)',
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <Icon
          name={kindIcon(task.kind)}
          size={14}
          style={{ color: 'var(--hc-accent-300, #9ccbe6)' }}
        />
        <span style={{ flex: 1, fontWeight: 600, fontSize: 12 }}>{task.label}</span>
        {running ? (
          <Spinner size={12} />
        ) : failed ? (
          <StatusDot kind="error" label={undefined} />
        ) : (
          <StatusDot kind="ok" label={undefined} />
        )}
      </div>
      {task.detail && (
        <span style={{ color: 'rgba(255,255,255,.6)', fontSize: 11 }}>{task.detail}</span>
      )}
      {running && (
        <div
          aria-hidden
          style={{
            position: 'relative',
            height: 3,
            borderRadius: 2,
            background: 'rgba(255,255,255,.12)',
            overflow: 'hidden',
          }}
        >
          <div
            style={{
              position: 'absolute',
              inset: 0,
              width: `${pct}%`,
              borderRadius: 2,
              background: 'var(--hc-accent-500, #0878c8)',
              transition: 'width .3s ease',
            }}
          />
        </div>
      )}
      {failed && task.error && (
        <span
          title={task.error}
          style={{ color: 'var(--hc-error, #e05656)', fontSize: 11, wordBreak: 'break-word' }}
        >
          {task.error}
        </span>
      )}
    </div>
  );
}

/**
 * TasksTray drives its own polling from `client` by default. A parent that
 * already holds a useTasks instance (e.g. Shell, which calls refresh() after
 * firing a power action) can pass it as `controller` to share the single poller
 * instead of spinning up a second one.
 */
export function TasksTray({
  client,
  controller,
}: {
  client: TasksClient;
  controller?: UseTasksResult;
}) {
  // Hooks must run unconditionally; the internal poller is harmless (and idle)
  // when a controller is supplied, but to avoid a second live poller we only
  // read from it when no controller is provided.
  const ownTasks = useTasks(controller ? noopTasksClient : client);
  const { tasks, running } = controller ?? ownTasks;
  const [open, setOpen] = useState(false);

  const { visible, succeeded, failed, pillLabel } = traySummary(tasks);
  // Idle: nothing to show, the tray disappears entirely.
  if (!visible) return null;

  return (
    <div
      style={{
        position: 'fixed',
        right: 16,
        bottom: 16,
        zIndex: 200,
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'flex-end',
        gap: 8,
        fontFamily: 'var(--hc-font-sans)',
      }}
    >
      {open && (
        <div
          role="list"
          aria-label="Tasks"
          style={{
            width: 320,
            maxHeight: 360,
            overflowY: 'auto',
            display: 'flex',
            flexDirection: 'column',
            gap: 6,
            padding: 10,
            borderRadius: 10,
            background: 'rgba(8, 22, 38, 0.96)',
            border: '1px solid var(--hc-accent-600, #1668a5)',
            color: '#fff',
            boxShadow: '0 8px 28px rgba(0,0,0,.5)',
          }}
        >
          {tasks.map((t) => (
            <TaskRow key={t.id} task={t} />
          ))}
        </div>
      )}
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        title="Long-running tasks"
        style={{
          display: 'inline-flex',
          alignItems: 'center',
          gap: 8,
          padding: '8px 14px',
          borderRadius: 999,
          border: '1px solid var(--hc-accent-600, #1668a5)',
          background: 'rgba(8, 22, 38, 0.96)',
          color: '#fff',
          fontSize: 12,
          fontWeight: 600,
          cursor: 'pointer',
          boxShadow: '0 6px 20px rgba(0,0,0,.4)',
        }}
      >
        {running > 0 ? (
          <Spinner size={14} />
        ) : failed > 0 ? (
          <StatusDot kind="error" />
        ) : (
          <StatusDot kind="ok" />
        )}
        <span>{pillLabel}</span>
        {running === 0 && succeeded > 0 && (
          <span style={{ color: 'rgba(255,255,255,.55)', fontWeight: 400 }}>{succeeded} done</span>
        )}
      </button>
    </div>
  );
}
