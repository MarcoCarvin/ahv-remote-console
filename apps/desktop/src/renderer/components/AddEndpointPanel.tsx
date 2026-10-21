import { useState } from 'react';
import type { Endpoint, UpsertEndpointRequest } from '@ahv-remote-console/protocol';
import { useToast } from '@ahv-remote-console/ui';
import { useClient } from '../app/ClientContext';
import { BrandHeader, BrandDecor } from './clusterShared';
import { T } from './clusterTokens';

export interface AddEndpointPanelProps {
  pluginId: string;
  /** When provided, the form edits this endpoint in place instead of creating one. */
  endpoint?: Endpoint;
  onSaved: (endpointId: string) => void;
  onCancel?: () => void;
}

export function AddEndpointPanel({ pluginId, endpoint, onSaved, onCancel }: AddEndpointPanelProps) {
  const client = useClient();
  const toast = useToast();
  const editing = endpoint != null;
  const [label, setLabel] = useState(endpoint?.label ?? '');
  const [url, setUrl] = useState(endpoint?.url ?? '');
  const [allowInsecure, setAllowInsecure] = useState(endpoint?.allow_insecure_tls ?? false);
  const [pinnedCertB64, setPinnedCertB64] = useState(endpoint?.pinned_cert_b64 ?? '');
  const [customer, setCustomer] = useState(endpoint?.tags?.customer ?? '');
  const [environment, setEnvironment] = useState(endpoint?.tags?.environment ?? '');
  const [submitting, setSubmitting] = useState(false);
  // Open the Advanced section up-front when editing an endpoint that already
  // has TLS overrides, so they're visible (and preserved) rather than hidden.
  const [advancedOpen, setAdvancedOpen] = useState(
    // eslint-disable-next-line @typescript-eslint/prefer-nullish-coalescing -- intentional falsy check: an empty-string pin should fall through to the allow_insecure_tls flag
    !!(endpoint?.pinned_cert_b64 || endpoint?.allow_insecure_tls),
  );

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!url.trim() || !/^https?:\/\//.test(url.trim())) {
      toast.push('warn', 'Valid URL required', 'Enter the Prism Central URL (https://...).');
      return;
    }
    const cleanURL = url.trim().replace(/\/$/, '');
    const friendly = label.trim() || hostFromURL(cleanURL);
    const body: UpsertEndpointRequest = {
      label: friendly,
      // Preserve the endpoint's plugin + auth methods when editing; new
      // endpoints default to the cookie (embedded SSO) flow.
      plugin_id: endpoint?.plugin_id ?? pluginId,
      url: cleanURL,
      auth_methods: endpoint?.auth_methods?.length ? endpoint.auth_methods : ['cookie'],
      allow_insecure_tls: allowInsecure,
    };
    // Sending the id makes upsert update in place; omit it to create a new one.
    if (editing) body.id = endpoint.id;
    // Re-send the pin so an edit doesn't silently drop a configured pin; an
    // empty field clears it (intentional).
    if (pinnedCertB64.trim()) body.pinned_cert_b64 = pinnedCertB64.trim();
    // Customer + environment are explicit grouping fields, persisted in the
    // endpoint's tags map (preserve any other tags already set).
    const tags: Record<string, string> = { ...(endpoint?.tags ?? {}) };
    const c = customer.trim();
    const env = environment.trim();
    if (c) tags.customer = c;
    else delete tags.customer;
    if (env) tags.environment = env;
    else delete tags.environment;
    body.tags = tags;

    setSubmitting(true);
    try {
      const res = await client.upsertEndpoint(body);
      // No success toast: onSaved navigates back and the endpoint appears in the
      // connection manager — the visible result is the confirmation.
      onSaved(res.endpoint.id);
    } catch (err) {
      toast.push('error', 'Failed to save endpoint', (err as Error).message);
    } finally {
      setSubmitting(false);
    }
  };

  const inputStyle: React.CSSProperties = {
    width: '100%',
    padding: '10px 12px',
    background: T.bg,
    border: `1px solid ${T.line2}`,
    borderRadius: 8,
    fontSize: 13,
    fontFamily: T.sans,
    color: T.ink,
    outline: 'none',
  };

  return (
    <div
      style={{
        height: '100%',
        display: 'flex',
        flexDirection: 'column',
        background: T.soft,
        alignItems: 'center',
        justifyContent: 'center',
        position: 'relative',
      }}
    >
      {/* Same soft-tinted brand backdrop as the connection-manager hero. */}
      <BrandDecor bars={false} />
      <div style={{ position: 'absolute', top: 0, left: 0, right: 0 }}>
        <BrandHeader />
      </div>

      <div
        style={{
          width: 480,
          background: T.bg,
          borderRadius: 16,
          padding: 36,
          border: `1px solid ${T.line2}`,
          boxShadow: '0 4px 12px rgba(15,34,53,.08)',
          position: 'relative',
          zIndex: 1,
        }}
      >
        <form
          onSubmit={(e) => {
            void handleSubmit(e);
          }}
          style={{ display: 'flex', flexDirection: 'column', gap: 20 }}
        >
          <div>
            <h1
              style={{
                fontSize: 30,
                fontWeight: 400,
                margin: 0,
                fontFamily: T.sans,
                color: T.ink,
              }}
            >
              {editing ? 'Edit endpoint' : 'Connect to Prism'}
            </h1>
            <p
              style={{
                fontSize: 12,
                color: T.ink3,
                margin: '8px 0 0',
                lineHeight: 1.5,
                fontFamily: T.sans,
              }}
            >
              {editing
                ? 'Update this endpoint’s URL, name, or TLS settings. Your saved sign-in is kept; changing the URL may require signing in again.'
                : 'You will sign in with your usual Prism credentials on the next screen — local account, AD/LDAP, or SSO.'}
            </p>
          </div>

          <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            <label style={{ fontSize: 11, fontWeight: 500, color: T.ink3, fontFamily: T.mono }}>
              Prism Central URL
            </label>
            <input
              style={inputStyle}
              placeholder="https://prism.example.com"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              autoFocus
            />
          </div>

          <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            <label style={{ fontSize: 11, fontWeight: 500, color: T.ink3, fontFamily: T.mono }}>
              Friendly name (optional)
            </label>
            <input
              style={inputStyle}
              placeholder='e.g. "Customer A — production"'
              value={label}
              onChange={(e) => setLabel(e.target.value)}
            />
            <span style={{ fontSize: 10, color: T.ink3, marginTop: 2 }}>
              Defaults to the hostname if left empty.
            </span>
          </div>

          <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
            <div style={{ display: 'flex', gap: 12 }}>
              <div style={{ display: 'flex', flexDirection: 'column', gap: 4, flex: 1 }}>
                <label style={{ fontSize: 11, fontWeight: 500, color: T.ink3, fontFamily: T.mono }}>
                  Customer
                </label>
                <input
                  style={inputStyle}
                  placeholder='e.g. "Customer A"'
                  value={customer}
                  onChange={(e) => setCustomer(e.target.value)}
                />
              </div>
              <div style={{ display: 'flex', flexDirection: 'column', gap: 4, flex: 1 }}>
                <label style={{ fontSize: 11, fontWeight: 500, color: T.ink3, fontFamily: T.mono }}>
                  Environment
                </label>
                <input
                  style={inputStyle}
                  placeholder='e.g. "PROD", "DR", "STAGING"'
                  value={environment}
                  onChange={(e) => setEnvironment(e.target.value)}
                />
              </div>
            </div>
            <span style={{ fontSize: 10, color: T.ink3 }}>
              Groups this cluster under the customer in the connection list and map.
            </span>
          </div>

          <button
            type="button"
            onClick={() => setAdvancedOpen((v) => !v)}
            aria-expanded={advancedOpen}
            style={{
              alignSelf: 'flex-start',
              display: 'inline-flex',
              alignItems: 'center',
              gap: 7,
              fontSize: 12,
              fontWeight: 600,
              color: advancedOpen ? T.blue : T.ink2,
              background: advancedOpen ? T.soft : 'transparent',
              border: `1px solid ${T.line2}`,
              borderRadius: 8,
              cursor: 'pointer',
              padding: '7px 11px',
              fontFamily: T.mono,
            }}
          >
            <span
              aria-hidden
              style={{
                display: 'inline-block',
                transform: advancedOpen ? 'rotate(90deg)' : 'none',
                transition: 'transform 120ms ease',
                fontSize: 10,
              }}
            >
              ▸
            </span>
            Advanced TLS
          </button>

          {advancedOpen && (
            <div
              style={{
                display: 'flex',
                flexDirection: 'column',
                gap: 12,
                padding: 14,
                border: `1px solid ${T.line2}`,
                borderRadius: 10,
                background: T.soft,
              }}
            >
              <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
                <label
                  style={{
                    fontSize: 11,
                    fontWeight: 500,
                    color: T.ink3,
                    fontFamily: T.mono,
                  }}
                >
                  Pinned leaf certificate (base64)
                </label>
                <textarea
                  style={{
                    ...inputStyle,
                    minHeight: 88,
                    resize: 'vertical',
                    fontFamily: T.mono,
                    fontSize: 12,
                    lineHeight: 1.45,
                    whiteSpace: 'pre-wrap',
                    wordBreak: 'break-all',
                  }}
                  rows={4}
                  placeholder="MIID..."
                  value={pinnedCertB64}
                  onChange={(e) => setPinnedCertB64(e.target.value)}
                />
              </div>
              <label
                style={{
                  display: 'inline-flex',
                  alignItems: 'center',
                  gap: 8,
                  fontSize: 12,
                  fontFamily: T.sans,
                }}
              >
                <input
                  type="checkbox"
                  checked={allowInsecure}
                  onChange={(e) => setAllowInsecure(e.target.checked)}
                />
                <span style={{ color: allowInsecure ? T.amber : T.ink }}>
                  Trust the certificate at first sign-in (self-signed clusters). You confirm its
                  fingerprint before it is saved.
                </span>
              </label>
            </div>
          )}

          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 10, marginTop: 4 }}>
            {onCancel && (
              <button
                type="button"
                onClick={onCancel}
                style={{
                  padding: '8px 20px',
                  background: 'transparent',
                  color: T.ink3,
                  border: `1px solid ${T.line2}`,
                  borderRadius: 8,
                  fontSize: 13,
                  fontFamily: T.sans,
                  cursor: 'pointer',
                }}
              >
                Cancel
              </button>
            )}
            <button
              type="submit"
              disabled={submitting}
              style={{
                padding: '8px 24px',
                background: T.blue,
                color: '#fff',
                border: 'none',
                borderRadius: 8,
                fontSize: 13,
                fontWeight: 600,
                fontFamily: T.sans,
                cursor: submitting ? 'wait' : 'pointer',
                opacity: submitting ? 0.7 : 1,
              }}
            >
              {submitting ? 'Saving…' : editing ? 'Save' : 'Continue'}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}

function hostFromURL(u: string): string {
  try {
    return new URL(u).host;
  } catch {
    return u;
  }
}
