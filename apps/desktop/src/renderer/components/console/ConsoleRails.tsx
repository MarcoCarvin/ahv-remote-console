/**
 * Console chrome rails (ambient latency line + top/bottom hover rails),
 * extracted verbatim from ConsoleView (FE-01 ConsoleToolbar / StatusFooter).
 * Pure presentational components driven entirely by props.
 */

import { SignalBars } from '../console-motion';
import { titleBarLeft } from '../../platform';
import { ClusterBtn, type ClusterButton } from '../FloatingCluster';
import { Spark } from '../clusterShared';
import { T, noDrag } from '../clusterTokens';
import { type ConsoleStatus, statusLabel } from './status';

// A) Ambient latency line — 3px, full width, driven by live RTT/connection
// state. Healthy = blue→green; degraded (high RTT) shifts the stop to amber;
// connecting/reconnecting/lost = red/blue with an indeterminate sweep.
export function AmbientLine({ status }: { status: ConsoleStatus }) {
  let background: string;
  let sweep = false;
  if (status.kind === 'error' || status.kind === 'reconnecting') {
    background = 'linear-gradient(90deg, var(--red), #e8765f 60%, var(--red))';
    sweep = true;
  } else if (status.kind === 'connecting' || status.kind === 'starting') {
    background = 'linear-gradient(90deg, var(--blue), var(--blueL) 60%, var(--blue))';
    sweep = true;
  } else if (status.kind === 'poweredOff') {
    background = 'linear-gradient(90deg, var(--amber), var(--amber) 60%, var(--amber))';
  } else if (status.kind === 'connected' && status.latencyMs > 150) {
    background = 'linear-gradient(90deg, var(--blue), var(--amber) 60%, var(--blue))';
  } else {
    background = 'linear-gradient(90deg, var(--blue), var(--greenL) 60%, var(--blue))';
  }
  return (
    <div
      aria-hidden
      className={sweep ? 'ec-sweep' : undefined}
      style={{
        position: 'absolute',
        top: 0,
        left: 0,
        right: 0,
        height: 3,
        background,
        opacity: 0.9,
        zIndex: 60,
        pointerEvents: 'none',
      }}
    />
  );
}

const RAIL_DIVIDER: React.CSSProperties = {
  width: 1,
  height: 18,
  background: 'rgba(255,255,255,.16)',
  margin: '0 4px',
  flexShrink: 0,
};

// C) Top chrome rail — hover-reveal, docked flush to the top frame edge.
export function TopRail({
  vmName,
  status,
  buttons,
  extra,
  onClose,
  signalBars,
  revealed,
  hoverProps,
}: {
  vmName: string;
  status: ConsoleStatus;
  buttons: ClusterButton[];
  /** Extra toolbar controls that aren't simple ClusterButtons (e.g. the
   *  display-fit dropdown). Rendered after the buttons, before Close. */
  extra?: React.ReactNode;
  onClose: () => void;
  signalBars: boolean;
  revealed: boolean;
  hoverProps: { onMouseEnter: () => void; onMouseLeave: () => void };
}) {
  const reconnecting =
    status.kind === 'connecting' || status.kind === 'reconnecting' || status.kind === 'starting';
  const dot = statusLabel(status);
  return (
    <div
      {...hoverProps}
      style={
        {
          position: 'absolute',
          top: 7,
          left: 10,
          right: 10,
          height: 34,
          borderRadius: '6px 6px 0 0',
          background: 'rgba(11,18,28,.8)',
          backdropFilter: 'blur(14px)',
          border: '1px solid rgba(74,163,220,.22)',
          borderBottom: 'none',
          display: 'flex',
          alignItems: 'center',
          gap: 12,
          padding: `0 12px 0 ${titleBarLeft(12)}px`,
          zIndex: 100,
          opacity: revealed ? 1 : 0,
          transform: revealed ? 'translateY(0)' : 'translateY(-8px)',
          transition: 'opacity 160ms ease, transform 160ms ease',
          pointerEvents: revealed ? 'auto' : 'none',
          WebkitAppRegion: 'drag',
        } as React.CSSProperties
      }
    >
      <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <Spark
          size={17}
          color="var(--blueL)"
          className={reconnecting ? 'ec-spark-spin' : undefined}
        />
        <span style={{ fontFamily: T.mono, fontWeight: 600, fontSize: 13, color: '#fff' }}>
          {vmName}
        </span>
        <span
          role="img"
          aria-label={`Status: ${dot.text}`}
          style={{ width: 6, height: 6, borderRadius: '50%', background: dot.color, flexShrink: 0 }}
        />
        <span style={{ fontFamily: T.mono, fontSize: 11, color: dot.color }}>{dot.text}</span>
        {signalBars && (
          <SignalBars
            rttMs={status.kind === 'connected' ? status.latencyMs : 0}
            connected={status.kind === 'connected'}
          />
        )}
      </span>
      <span
        style={{ ...noDrag, marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: 4 }}
      >
        {/* Key by title, not icon: icons are reused across buttons (e.g. the
            CD 'disc' on both Prerequisites & media and Snapshots), so an
            icon key collides; titles are unique + stable across this rail. */}
        {buttons.map((b) =>
          b.tone === 'danger' ? (
            [
              <span key={`${b.title}-div`} aria-hidden style={RAIL_DIVIDER} />,
              <ClusterBtn key={b.title} btn={b} />,
            ]
          ) : (
            <ClusterBtn key={b.title} btn={b} />
          ),
        )}
        {extra}
        <span aria-hidden style={RAIL_DIVIDER} />
        <ClusterBtn btn={{ icon: 'close', title: 'Close session', onClick: onClose }} />
      </span>
    </div>
  );
}

// The bottom rail was removed: it duplicated status already shown elsewhere and,
// once the guest went full-bleed, it covered the guest's own taskbar / window
// manager every time the pointer neared the bottom edge. The bottom edge now
// belongs entirely to the guest; all chrome lives in the hover-reveal TopRail.
