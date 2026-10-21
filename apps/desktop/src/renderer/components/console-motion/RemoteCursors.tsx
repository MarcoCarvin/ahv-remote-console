/**
 * RemoteCursors (C7) — presence layer for shared sessions. Renders one
 * gliding cursor + name label per peer. Mounts as a full-bleed absolute
 * overlay over the canvas wrapper (pointer-events:none).
 *
 * This is a no-op-friendly component: it's gated by a feature flag in
 * practice (shared sessions). If `peers` is empty it renders nothing,
 * so it's safe to mount unconditionally.
 *
 * Peer coordinates are in canvas-wrapper pixel space (the same space
 * ConsoleView already maps mouse coords into). Movement is a cheap CSS
 * `transform` transition (compositor-only). Reduced motion: CSS
 * hard-gate drops the glide transition (cursors jump) but still shows.
 */

import './consoleMotion.css';

export interface RemoteCursorPeer {
  id: string;
  name: string;
  color: string;
  x: number;
  y: number;
}

export interface RemoteCursorsProps {
  peers: RemoteCursorPeer[];
}

export function RemoteCursors({ peers }: RemoteCursorsProps) {
  if (!peers.length) return null;

  return (
    <div
      className="cm-root"
      aria-hidden
      style={{ position: 'absolute', inset: 0, pointerEvents: 'none' }}
    >
      {peers.map((p) => (
        <div
          key={p.id}
          className="cm-peer"
          style={{ transform: `translate3d(${p.x}px, ${p.y}px, 0)` }}
        >
          <svg width="16" height="16" viewBox="0 0 16 16" style={{ display: 'block' }}>
            <path
              d="M1 1 L1 12 L4.2 9 L6.3 14 L8.2 13.2 L6.1 8.4 L10.5 8.4 Z"
              fill={p.color}
              stroke="#0a0f17"
              strokeWidth="0.75"
            />
          </svg>
          <span className="cm-peer__label" style={{ background: p.color }}>
            {p.name}
          </span>
        </div>
      ))}
    </div>
  );
}

export default RemoteCursors;
