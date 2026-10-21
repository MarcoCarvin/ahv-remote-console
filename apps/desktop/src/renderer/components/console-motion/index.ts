/**
 * console-motion — the active console motion layer.
 *
 * Barrel re-export so ConsoleView can import the layer from one path.
 * Components are props-based and self-contained; each pulls in
 * consoleMotion.css on its own, so importing any one loads the styles.
 */

export {
  useConsoleMotion,
  deriveMotion,
  DEGRADED_RTT_MS,
  type ConsoleMotionInput,
  type ConsoleMotionStatus,
  type ConsoleMotionStatusKind,
  type ConsoleMotionState,
  type ConsoleMotionFlags,
  type ConsoleMotionResult,
} from './useConsoleMotion';

export { LatencyRing, type LatencyRingProps } from './LatencyRing';
export { SignalBars, type SignalBarsProps } from './SignalBars';
// Pure mapping helpers (side-effect free; usable in NODE/tests).
export {
  latencyColor,
  latencyCadenceMs,
  signalActiveBars,
  signalColor,
  SIGNAL_BAR_COUNT,
} from './signal';
// Shared-session presence layer (multi-viewer): one gliding cursor per peer.
export { RemoteCursors, type RemoteCursorPeer, type RemoteCursorsProps } from './RemoteCursors';
