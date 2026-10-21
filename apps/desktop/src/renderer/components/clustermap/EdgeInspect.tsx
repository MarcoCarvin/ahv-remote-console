import type { ClusterLink } from '../../app/recentStore';
import { T } from '../clusterTokens';

/**
 * Inspector body for a selected operator link (edge). Moved verbatim
 * from ClusterMap (FE-06 decomposition) — props and markup unchanged.
 */
export function EdgeInspect({
  link,
  health,
  nameOf,
  onRemove,
  onClose,
}: {
  link: ClusterLink;
  health: { color: string; label: string };
  nameOf: (id: string) => string;
  onRemove: () => void;
  onClose: () => void;
}) {
  return (
    <div style={{ padding: '20px 22px' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <span
          style={{ width: 10, height: 10, borderRadius: '50%', background: health.color }}
          role="img"
          aria-label={`Link ${health.label}`}
        />
        <h2
          style={{
            margin: 0,
            fontFamily: T.sans,
            fontSize: 17,
            fontWeight: 700,
            color: T.ink,
            flex: 1,
          }}
        >
          {link.label}
        </h2>
        <button
          className="ec-focus"
          onClick={onClose}
          aria-label="Close"
          title="Close"
          style={{
            background: 'transparent',
            border: 'none',
            color: T.ink3,
            cursor: 'pointer',
            fontSize: 16,
            lineHeight: 1,
          }}
        >
          ×
        </button>
      </div>
      <div
        style={{
          marginTop: 4,
          fontFamily: T.sans,
          fontSize: 12,
          fontWeight: 600,
          color: health.color,
        }}
      >
        {health.label}{' '}
        <span style={{ color: T.ink3, fontWeight: 400 }}>(derived from endpoint status)</span>
      </div>
      <div
        style={{ fontFamily: T.mono, fontSize: 12, color: T.ink2, marginTop: 16, lineHeight: 1.7 }}
      >
        {nameOf(link.aEndpointId)}
        <br />
        <span style={{ color: T.ink3 }}>↕ {link.label}</span>
        <br />
        {nameOf(link.bEndpointId)}
      </div>
      <div style={{ fontFamily: T.sans, fontSize: 11, color: T.ink3, marginTop: 14 }}>
        Operator-defined link (this build exposes no cluster-reported replication topology).
      </div>
      <div style={{ marginTop: 22 }}>
        <button
          className="ec-focus"
          onClick={onRemove}
          style={{
            fontFamily: T.sans,
            fontSize: 13,
            fontWeight: 600,
            color: T.red,
            background: T.bg,
            border: `1px solid ${T.line2}`,
            borderRadius: 8,
            padding: '9px 16px',
            cursor: 'pointer',
            minHeight: 40,
          }}
        >
          Remove link
        </button>
      </div>
    </div>
  );
}
