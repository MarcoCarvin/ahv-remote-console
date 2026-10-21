/**
 * ConsoleTaskBar — the console's running-task indicator. It is deliberately NOT a
 * pop-out alert: a thin progress LINE sits flush at the toolbar seam (chrome, not
 * a card) with a low-chrome label beneath it, and it fades/slides in and out
 * PROGRESSIVELY via AnimatePresence rather than snapping on/off. It aggregates
 * EVERY running task the console's poller sees (power, serial-port, agent-ISO
 * install, …) into this one indicator — replacing the old "look at the Tasks
 * tray" banner and the corner pill. Driven by the useTasks list; shows nothing
 * when nothing runs.
 *
 * `pointer-events: none` — status only; it never swallows a click meant for the
 * VM behind it.
 */

import { m, AnimatePresence } from 'framer-motion';
import { Spinner } from '@ahv-remote-console/ui';
import type { TaskInfo } from '@ahv-remote-console/protocol';
import { clampPercent } from '../TasksTray';
import { duration, ease } from '../../motion/tokens';

export interface RunningAggregate {
  /** How many tasks are running right now. */
  count: number;
  /** The single task's label, or "N tasks running" for several. */
  label: string;
  /** The single running task's current-step text (omitted when several run). */
  detail?: string;
  /** Mean percent_complete across the running tasks, clamped to [0,100]. */
  percent: number;
  /** True when no granular progress is reported yet — show a moving bar. */
  indeterminate: boolean;
}

/**
 * Collapse the running tasks into one progress view. Pure + exported for tests.
 * Returns null when nothing is running.
 */
// eslint-disable-next-line react-refresh/only-export-components -- intentional: testable pure helper co-located with the component (mirrors TasksTray)
export function aggregateRunning(tasks: TaskInfo[]): RunningAggregate | null {
  const running = tasks.filter((t) => t.state === 'running');
  const first = running[0];
  if (!first) return null;
  const sum = running.reduce((n, t) => n + clampPercent(t.percent_complete), 0);
  const percent = Math.round(sum / running.length);
  const single = running.length === 1;
  return {
    count: running.length,
    label: single ? first.label : `${running.length} tasks running`,
    detail: single ? first.detail : undefined,
    percent,
    indeterminate: percent <= 0,
  };
}

const ACCENT = 'var(--hc-accent-500, #0878c8)';

export function ConsoleTaskBar({
  tasks,
  pendingLabel,
}: {
  tasks: TaskInfo[];
  /** Shown as an indeterminate bar BEFORE the first task surfaces (e.g. the
   *  agent install is firing but the runtime hasn't registered its task yet),
   *  so the operator gets instant feedback without a blank gap. */
  pendingLabel?: string;
}) {
  const agg = aggregateRunning(tasks);
  const view: RunningAggregate | null =
    agg ??
    (pendingLabel
      ? { count: 1, label: pendingLabel, detail: undefined, percent: 0, indeterminate: true }
      : null);

  return (
    <AnimatePresence>
      {view && (
        <m.div
          key="console-task-bar"
          role="status"
          aria-live="polite"
          initial={{ opacity: 0, y: -6 }}
          animate={{ opacity: 1, y: 0 }}
          exit={{ opacity: 0, y: -6 }}
          transition={{ duration: duration.short4, ease: ease.standard }}
          style={{
            position: 'absolute',
            // Flush at the toolbar seam (TopRail is top:7 h:34 → bottom ≈ 41).
            top: 41,
            left: 0,
            right: 0,
            zIndex: 101,
            display: 'flex',
            flexDirection: 'column',
            alignItems: 'center',
            gap: 6,
            pointerEvents: 'none',
          }}
        >
          {/* Thin progress line, edge to edge — reads as chrome, not a card. */}
          <div
            aria-hidden
            style={{
              position: 'relative',
              width: '100%',
              height: 2,
              background: 'rgba(255,255,255,.08)',
              overflow: 'hidden',
            }}
          >
            {view.indeterminate ? (
              <div
                className="hc-ctb-indet"
                style={{
                  position: 'absolute',
                  top: 0,
                  bottom: 0,
                  width: '30%',
                  background: ACCENT,
                }}
              />
            ) : (
              <div
                style={{
                  position: 'absolute',
                  inset: 0,
                  width: `${view.percent}%`,
                  background: ACCENT,
                  transition: 'width .3s ease',
                }}
              />
            )}
          </div>
          {/* Low-chrome label — soft translucent scrim for legibility over the
              guest, no hard border or drop shadow. */}
          <div
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: 8,
              maxWidth: '80%',
              padding: '4px 12px',
              borderRadius: 999,
              background: 'rgba(8, 22, 38, 0.55)',
              backdropFilter: 'blur(6px)',
              WebkitBackdropFilter: 'blur(6px)',
              color: '#fff',
              fontFamily: 'var(--hc-font-sans)',
              fontSize: 11.5,
              lineHeight: 1.3,
              whiteSpace: 'nowrap',
              overflow: 'hidden',
              textOverflow: 'ellipsis',
            }}
          >
            <Spinner size={11} />
            <span style={{ fontWeight: 600, flexShrink: 0 }}>{view.label}</span>
            {view.detail && (
              <span
                style={{
                  color: 'rgba(255,255,255,.6)',
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                }}
              >
                · {view.detail}
              </span>
            )}
            {!view.indeterminate && (
              <span
                style={{
                  color: 'rgba(255,255,255,.75)',
                  fontVariantNumeric: 'tabular-nums',
                  flexShrink: 0,
                }}
              >
                · {view.percent}%
              </span>
            )}
          </div>
          <style>{`
            @keyframes hc-ctb-slide { 0% { left: -30%; } 100% { left: 100%; } }
            .hc-ctb-indet { animation: hc-ctb-slide 1.1s ease-in-out infinite; }
          `}</style>
        </m.div>
      )}
    </AnimatePresence>
  );
}
