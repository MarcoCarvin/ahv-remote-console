import { useCallback, useEffect, useRef, useState } from 'react';
import type { Endpoint } from '@ahv-remote-console/protocol';
import { useClient } from '../app/ClientContext';
import { BrandHeader, BrandDecor } from './clusterShared';
import { T } from './clusterTokens';

interface AuthStartResp {
  authenticated?: boolean;
}

export interface SignInPanelProps {
  endpoint: Endpoint;
  onSignedIn: () => void;
  /** Return to the cluster screen, where switching/adding endpoints lives. */
  onBack: () => void;
  /**
   * Auto-open the SSO window on mount. Set ONLY when sign-in was reached via an
   * explicit Connect/Add gesture — never from a background session-expiry, where
   * an unsolicited Prism popup would surprise the operator.
   */
  autoStart?: boolean;
}

/**
 * A cluster certificate captured on first use, waiting for the operator to
 * confirm its fingerprint. The session cookies are held here too: they are not
 * handed to the runtime until the certificate is trusted.
 */
interface PendingPin {
  capturedCertB64: string;
  capturedCertSha256?: string;
  cookies: { name: string; value: string }[];
}

export function SignInPanel({ endpoint, onSignedIn, onBack, autoStart = false }: SignInPanelProps) {
  const client = useClient();
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A certificate captured on first use is never trusted silently. It is held
  // here, with the session cookies, while the operator verifies the SHA-256
  // fingerprint out-of-band.
  const [pendingPin, setPendingPin] = useState<PendingPin | null>(null);

  const clusterHost = (() => {
    try {
      return new URL(endpoint.url).host;
    } catch {
      return endpoint.url;
    }
  })();

  // Store a confirmed first-use pin and clear allow_insecure_tls. The runtime
  // refuses an unpinned insecure endpoint, so sign-in cannot continue without
  // this; errors propagate to the caller.
  const persistPin = useCallback(
    async (capturedCertB64: string) => {
      await client.upsertEndpoint({
        id: endpoint.id,
        label: endpoint.label,
        plugin_id: endpoint.plugin_id,
        url: endpoint.url,
        auth_methods: endpoint.auth_methods,
        pinned_cert_b64: capturedCertB64,
        allow_insecure_tls: false,
        tags: endpoint.tags,
      });
    },
    [
      client,
      endpoint.id,
      endpoint.label,
      endpoint.plugin_id,
      endpoint.url,
      endpoint.auth_methods,
      endpoint.tags,
    ],
  );

  // Hand the captured session to the runtime. Prism validates it there.
  const authenticate = useCallback(
    async (cookies: { name: string; value: string }[]) => {
      const res = (await client.startAuth('nutanix-ahv', {
        endpoint_id: endpoint.id,
        method: 'cookie',
        cookies,
      })) as AuthStartResp;
      if (!res.authenticated) throw new Error('Prism rejected the captured session.');
    },
    [client, endpoint.id],
  );

  // The operator confirmed the fingerprint: store the pin first, then send the
  // session. The runtime only accepts the session over the pinned certificate.
  const confirmPin = useCallback(async () => {
    const pin = pendingPin;
    if (!pin) return;
    setPendingPin(null);
    setSubmitting(true);
    setError(null);
    try {
      await persistPin(pin.capturedCertB64);
      await authenticate(pin.cookies);
      onSignedIn();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setSubmitting(false);
    }
  }, [pendingPin, persistPin, authenticate, onSignedIn]);

  // The operator did not trust the certificate: discard the captured session
  // and stop. Nothing was sent to the runtime.
  const declinePin = useCallback(() => {
    setPendingPin(null);
    setError('Sign-in cancelled: the cluster certificate was not trusted.');
  }, []);

  const startSSO = useCallback(async () => {
    setSubmitting(true);
    setError(null);
    try {
      const result = await window.ahvRemoteConsole.openSSOLogin({
        endpointId: endpoint.id,
        prismURL: endpoint.url,
      });
      if (!result.cookies || result.cookies.length === 0) {
        throw new Error('No session cookies captured from Prism.');
      }

      // Trust on first use: the endpoint has no pin and the SSO window captured
      // the cluster's certificate. Show the fingerprint for out-of-band
      // verification before anything reaches the runtime. The pin is stored and
      // the session sent only after the operator confirms (confirmPin).
      if (!endpoint.pinned_cert_b64 && result.capturedCertB64) {
        setPendingPin({
          capturedCertB64: result.capturedCertB64,
          capturedCertSha256: result.capturedCertSha256,
          cookies: result.cookies,
        });
        return;
      }
      await authenticate(result.cookies);
      onSignedIn();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setSubmitting(false);
    }
  }, [endpoint.id, endpoint.url, endpoint.pinned_cert_b64, authenticate, onSignedIn]);

  // SSO auto-opens ONLY when sign-in was reached via an explicit Connect/Add
  // gesture (`autoStart`) — clicking Connect IS the intent, so jumping straight
  // to the Prism popup saves a click. When sign-in is reached from a background
  // session-expiry (the lobby's 30s poll getting a 401), `autoStart` is false
  // and the operator clicks the button — never an unsolicited popup. Fires once.
  const autoFired = useRef(false);
  useEffect(() => {
    if (autoStart && !autoFired.current) {
      autoFired.current = true;
      void startSSO();
    }
  }, [autoStart, startSSO]);

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
          width: 440,
          background: T.bg,
          borderRadius: 16,
          padding: 36,
          border: `1px solid ${T.line2}`,
          boxShadow: '0 4px 12px rgba(15,34,53,.08)',
          display: 'flex',
          flexDirection: 'column',
          gap: 24,
          position: 'relative',
          zIndex: 1,
        }}
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
            Sign in
          </h1>
          <div style={{ marginTop: 10 }}>
            <span style={{ fontSize: 12, color: T.ink3, fontFamily: T.sans }}>Connecting to </span>
            <span style={{ fontSize: 13, fontWeight: 600, color: T.ink, fontFamily: T.sans }}>
              {endpoint.label}
            </span>
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.ink3, marginTop: 3 }}>
            {endpoint.url}
          </div>
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          {pendingPin ? (
            <CertConfirm
              host={clusterHost}
              sha256={pendingPin.capturedCertSha256}
              onConfirm={() => void confirmPin()}
              onCancel={declinePin}
            />
          ) : submitting ? (
            <div
              style={{
                display: 'flex',
                flexDirection: 'column',
                alignItems: 'center',
                gap: 14,
                padding: '12px 0',
              }}
            >
              <PulseDot />
              <div
                style={{
                  fontSize: 13,
                  color: T.ink,
                  fontFamily: T.sans,
                  fontWeight: 500,
                }}
              >
                Waiting for sign-in…
              </div>
              <div
                style={{
                  fontSize: 12,
                  color: T.ink3,
                  lineHeight: 1.6,
                  textAlign: 'center',
                  fontFamily: T.sans,
                }}
              >
                A Prism sign-in window has opened. Complete authentication there with whichever
                method your cluster supports.
              </div>
            </div>
          ) : error ? (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
              <div
                style={{
                  fontSize: 13,
                  color: T.red,
                  fontWeight: 600,
                  fontFamily: T.sans,
                }}
              >
                Sign-in did not complete
              </div>
              <div style={{ fontSize: 12, color: T.ink, lineHeight: 1.5, fontFamily: T.sans }}>
                {error}
              </div>
              <button
                onClick={() => void startSSO()}
                style={{
                  alignSelf: 'flex-end',
                  padding: '8px 20px',
                  background: T.blue,
                  color: '#fff',
                  border: 'none',
                  borderRadius: 8,
                  fontSize: 13,
                  fontWeight: 500,
                  fontFamily: T.sans,
                  cursor: 'pointer',
                }}
              >
                Open Prism sign-in again
              </button>
            </div>
          ) : (
            <div
              style={{
                display: 'flex',
                flexDirection: 'column',
                alignItems: 'center',
                gap: 12,
                padding: '12px 0',
              }}
            >
              <div
                style={{
                  fontSize: 12,
                  color: T.ink3,
                  lineHeight: 1.6,
                  textAlign: 'center',
                  fontFamily: T.sans,
                }}
              >
                A Prism sign-in window will open. Complete authentication there with whichever
                method your cluster supports.
              </div>
              <button
                onClick={() => void startSSO()}
                style={{
                  padding: '9px 22px',
                  background: T.blue,
                  color: '#fff',
                  border: 'none',
                  borderRadius: 8,
                  fontSize: 13,
                  fontWeight: 600,
                  fontFamily: T.sans,
                  cursor: 'pointer',
                }}
              >
                Sign in with SSO
              </button>
            </div>
          )}
        </div>

        <div
          style={{
            borderTop: `1px solid ${T.line2}`,
            paddingTop: 14,
            display: 'flex',
            justifyContent: 'center',
          }}
        >
          <button
            onClick={onBack}
            type="button"
            style={{
              background: 'transparent',
              border: 'none',
              color: T.ink3,
              fontSize: 12,
              fontFamily: T.sans,
              cursor: 'pointer',
              padding: 0,
            }}
          >
            ← Back to endpoints
          </button>
        </div>
      </div>
    </div>
  );
}

function PulseDot() {
  return (
    <>
      <style>{`@keyframes ahvPulseDot { 0%,100% { opacity:1; transform:scale(1); } 50% { opacity:.4; transform:scale(1.3); } }`}</style>
      <span
        style={{
          width: 10,
          height: 10,
          borderRadius: '50%',
          background: T.blue,
          display: 'inline-block',
          animation: 'ahvPulseDot 1.4s ease-in-out infinite',
        }}
      />
    </>
  );
}

/** Group a hex fingerprint into colon-separated byte pairs for readability. */
function formatFingerprint(sha256: string): string {
  return (sha256.match(/.{1,2}/g) ?? [sha256]).join(':').toUpperCase();
}

/**
 * First-connection certificate fingerprint confirmation. The operator verifies
 * this SHA-256 out-of-band (for example against the cluster's known
 * certificate). Trusting it stores the pin and continues sign-in; cancelling
 * discards the captured session.
 */
function CertConfirm({
  host,
  sha256,
  onConfirm,
  onCancel,
}: {
  host: string;
  sha256?: string;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div style={{ fontSize: 13, color: T.ink, fontWeight: 600, fontFamily: T.sans }}>
        First connection to {host}
      </div>
      <div style={{ fontSize: 12, color: T.ink, lineHeight: 1.5, fontFamily: T.sans }}>
        This cluster presented a certificate that is not trusted yet. Check this SHA-256 fingerprint
        with your cluster administrator. Sign-in continues only if you trust it:
      </div>
      <div
        style={{
          fontFamily: T.mono,
          fontSize: 11,
          color: T.ink,
          background: T.soft,
          border: `1px solid ${T.line2}`,
          borderRadius: 8,
          padding: '10px 12px',
          wordBreak: 'break-all',
          lineHeight: 1.6,
        }}
      >
        {sha256 ? formatFingerprint(sha256) : 'fingerprint unavailable'}
      </div>
      <div style={{ display: 'flex', gap: 10, justifyContent: 'flex-end' }}>
        <button
          onClick={onCancel}
          type="button"
          style={{
            padding: '8px 16px',
            background: 'transparent',
            color: T.ink3,
            border: `1px solid ${T.line2}`,
            borderRadius: 8,
            fontSize: 13,
            fontWeight: 500,
            fontFamily: T.sans,
            cursor: 'pointer',
          }}
        >
          Cancel sign-in
        </button>
        <button
          onClick={onConfirm}
          type="button"
          disabled={!sha256}
          style={{
            padding: '8px 20px',
            background: sha256 ? T.blue : T.line2,
            color: '#fff',
            border: 'none',
            borderRadius: 8,
            fontSize: 13,
            fontWeight: 600,
            fontFamily: T.sans,
            cursor: sha256 ? 'pointer' : 'not-allowed',
          }}
        >
          Trust and sign in
        </button>
      </div>
    </div>
  );
}
