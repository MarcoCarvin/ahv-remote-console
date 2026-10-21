import { useEffect, useState } from 'react';
import type { Endpoint } from '@ahv-remote-console/protocol';
import { getEndpointMeta, relativeTime, type ClusterLink } from '../../app/recentStore';
import { T, statusMeta, hostPort, type DotState } from '../clusterTokens';
import { linkHealthMeta, type LinkHealth } from './linkHealth';

/**
 * Inspector body for a selected cluster node. Moved verbatim from
 * ClusterMap (FE-06 decomposition) — props, state, effects, and markup
 * are unchanged.
 */
export function NodeInspect({
  ep,
  state,
  peers,
  nameOf,
  linkHealth,
  onConnect,
  onRetry,
  onEdit,
  onDiagnostics,
  onDelete,
  onUnlink,
  onClose,
}: {
  ep: Endpoint;
  state: DotState;
  peers: ClusterLink[];
  nameOf: (id: string) => string;
  linkHealth: (l: ClusterLink) => LinkHealth;
  onConnect: () => void;
  onRetry: () => void;
  onEdit: () => void;
  onDiagnostics: () => void;
  onDelete: () => void;
  onUnlink: (linkId: string) => void;
  onClose: () => void;
}) {
  const meta = statusMeta(state);
  const offline = state === 'offline';
  const lastConn = getEndpointMeta(ep.id)?.lastConnAt ?? null;
  const [confirmDelete, setConfirmDelete] = useState(false);
  useEffect(() => setConfirmDelete(false), [ep.id]);
  return (
    <div style={{ padding: '20px 22px' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <span
          style={{ width: 10, height: 10, borderRadius: '50%', background: meta.color }}
          role="img"
          aria-label={`Status: ${meta.label}`}
        />
        <h2
          style={{
            margin: 0,
            fontFamily: T.sans,
            fontSize: 19,
            fontWeight: 700,
            color: T.ink,
            flex: 1,
          }}
        >
          {ep.label}
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
      <div style={{ fontFamily: T.mono, fontSize: 12, color: T.ink3, marginTop: 6 }}>
        {hostPort(ep.url)}
      </div>
      <div
        style={{
          marginTop: 4,
          fontFamily: T.sans,
          fontSize: 12,
          fontWeight: 600,
          color: meta.color,
        }}
      >
        {meta.label}
      </div>

      {lastConn && (
        <div style={{ fontFamily: T.sans, fontSize: 12, color: T.ink3, marginTop: 14 }}>
          Last connected {relativeTime(lastConn)}.
        </div>
      )}

      {/* Links subsection */}
      <div style={{ marginTop: 20 }}>
        <div
          style={{
            fontFamily: T.sans,
            fontSize: 10,
            fontWeight: 600,
            letterSpacing: '.08em',
            color: T.ink3,
            textTransform: 'uppercase',
            marginBottom: 6,
          }}
        >
          Links
        </div>
        {peers.length === 0 ? (
          <div style={{ fontFamily: T.sans, fontSize: 12, color: T.ink3 }}>
            No links. Hover the node and drag its handle to another cluster.
          </div>
        ) : (
          peers.map((l) => {
            const peerId = l.aEndpointId === ep.id ? l.bEndpointId : l.aEndpointId;
            const lh = linkHealthMeta(linkHealth(l));
            return (
              <div
                key={l.id}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: 8,
                  padding: '7px 0',
                  borderBottom: `1px solid ${T.line}`,
                }}
              >
                <span
                  style={{ width: 7, height: 7, borderRadius: '50%', background: lh.color }}
                  role="img"
                  aria-label={`Link ${lh.label}`}
                />
                <span
                  style={{
                    fontFamily: T.sans,
                    fontSize: 13,
                    color: T.ink,
                    flex: 1,
                    minWidth: 0,
                    overflow: 'hidden',
                    textOverflow: 'ellipsis',
                    whiteSpace: 'nowrap',
                  }}
                >
                  {nameOf(peerId)}
                </span>
                <span style={{ fontFamily: T.mono, fontSize: 10, color: T.ink3 }}>{l.label}</span>
                <button
                  className="ec-focus"
                  onClick={() => onUnlink(l.id)}
                  style={{
                    background: 'transparent',
                    border: 'none',
                    color: T.blue,
                    cursor: 'pointer',
                    fontFamily: T.sans,
                    fontSize: 12,
                    fontWeight: 600,
                  }}
                >
                  Unlink
                </button>
              </div>
            );
          })
        )}
      </div>

      {/* Actions */}
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginTop: 22 }}>
        <button
          className="ec-focus"
          onClick={onConnect}
          style={{
            fontFamily: T.sans,
            fontSize: 13,
            fontWeight: 600,
            color: '#fff',
            background: T.blue,
            border: 'none',
            borderRadius: 8,
            padding: '9px 18px',
            cursor: 'pointer',
            minHeight: 40,
          }}
        >
          Connect
        </button>
        {offline && <SecBtn onClick={onRetry}>Retry</SecBtn>}
        <SecBtn onClick={onEdit}>Edit</SecBtn>
        <SecBtn onClick={onDiagnostics}>Diagnostics</SecBtn>
      </div>
      {offline && (
        <div style={{ fontFamily: T.sans, fontSize: 12, color: T.ink3, marginTop: 10 }}>
          This cluster isn’t reachable. Check the network or VPN, then retry.
        </div>
      )}

      {/* Delete endpoint */}
      <div style={{ marginTop: 22, paddingTop: 14, borderTop: `1px solid ${T.line}` }}>
        {confirmDelete ? (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
            <span style={{ fontFamily: T.sans, fontSize: 12, color: T.ink2 }}>
              Remove “{ep.label}”? Its saved sign-in and links are cleared.
            </span>
            <div style={{ display: 'flex', gap: 8 }}>
              <button
                className="ec-focus"
                onClick={onDelete}
                style={{
                  fontFamily: T.sans,
                  fontSize: 13,
                  fontWeight: 600,
                  color: '#fff',
                  background: T.red,
                  border: 'none',
                  borderRadius: 8,
                  padding: '8px 14px',
                  cursor: 'pointer',
                }}
              >
                Delete
              </button>
              <button
                className="ec-focus"
                onClick={() => setConfirmDelete(false)}
                style={{
                  fontFamily: T.sans,
                  fontSize: 13,
                  color: T.ink3,
                  background: 'transparent',
                  border: 'none',
                  cursor: 'pointer',
                }}
              >
                Cancel
              </button>
            </div>
          </div>
        ) : (
          <button
            className="ec-focus"
            onClick={() => setConfirmDelete(true)}
            style={{
              fontFamily: T.sans,
              fontSize: 13,
              fontWeight: 500,
              color: T.red,
              background: 'transparent',
              border: 'none',
              cursor: 'pointer',
              padding: 0,
            }}
          >
            Delete endpoint
          </button>
        )}
      </div>
    </div>
  );
}

function SecBtn({ onClick, children }: { onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      className="ec-focus"
      onClick={onClick}
      style={{
        fontFamily: T.sans,
        fontSize: 13,
        fontWeight: 500,
        color: T.ink2,
        background: T.bg,
        border: `1px solid ${T.line2}`,
        borderRadius: 8,
        padding: '9px 14px',
        cursor: 'pointer',
        minHeight: 40,
      }}
    >
      {children}
    </button>
  );
}
