/**
 * Embedded Prism SSO login window.
 *
 * The user clicks "Sign in with SSO" in the renderer. The main process opens
 * a sandboxed BrowserWindow that loads `<prism>/console/`. The user
 * authenticates via whatever IdP Prism is configured with (Okta, Authentik,
 * directory, local) — AHV Remote Console stays out of the IdP dance entirely.
 *
 * Once Prism considers the session authenticated it sets cookies named like
 * `NTNX_MERCURY_IAM_SESSION` and `JSESSIONID`. We poll the partition's
 * cookie jar after every top-level navigation; when a Prism session cookie
 * appears, we resolve with the cookies and close the window.
 *
 * The login window uses an *isolated* session partition (per endpoint) so
 * cookies, cache, and storage do not leak between endpoints or into the
 * default partition the renderer uses.
 */

import { BrowserWindow, session, type Cookie } from 'electron';
import log from 'electron-log';
import { X509Certificate, createHash } from 'node:crypto';

export interface SSOLoginResult {
  cookies: { name: string; value: string }[];
  /**
   * When the endpoint had no pin and we trusted the cluster cert on first use
   * (TOFU), this is its base64-DER, for the caller to persist as the
   * endpoint's pinned_cert_b64. Absent when an existing pin was enforced.
   */
  capturedCertB64?: string;
  /** Human-readable SHA-256 fingerprint of the TOFU-captured cert. */
  capturedCertSha256?: string;
}

/** Strip all whitespace (PEM line breaks etc.) so two base64 blobs compare. */
function stripWs(s: string): string {
  return s.replace(/\s+/g, '');
}

/** The base64-DER body of a PEM cert (armor lines + whitespace removed). */
function certToB64Der(pem: string): string {
  return stripWs(
    pem.replace(/-----BEGIN CERTIFICATE-----/g, '').replace(/-----END CERTIFICATE-----/g, ''),
  );
}

/**
 * SHA-256 of a certificate's SubjectPublicKeyInfo. This MATCHES the runtime's
 * pin semantics (plugins/ahv tlsConfigFor hashes RawSubjectPublicKeyInfo), so
 * a same-key certificate rotation does not break either path. Accepts PEM
 * (Electron's `certificate.data`) or DER (Buffer). Returns null on parse
 * failure.
 */
function spkiSha256(cert: string | Buffer): string | null {
  try {
    const x = new X509Certificate(cert);
    const spkiDer = x.publicKey.export({ type: 'spki', format: 'der' }) as Buffer;
    return createHash('sha256').update(spkiDer).digest('hex');
  } catch {
    return null;
  }
}

/** Colon-free lowercase SHA-256 of the whole DER cert, for display. */
function certFingerprint(cert: string): string | undefined {
  try {
    return new X509Certificate(cert).fingerprint256.replace(/:/g, '').toLowerCase();
  } catch {
    return undefined;
  }
}

const PRISM_SESSION_COOKIE_NAMES = [
  // Prism Central v3+ / Mercury IAM.
  'NTNX_MERCURY_IAM_SESSION',
  'NTNX-MERCURY-IAM-SESSION',
  // Older Prism builds.
  'JSESSIONID',
  'PRISMSESSION',
];

export interface OpenSSOLoginArgs {
  endpointId: string;
  prismURL: string;
  /**
   * base64-DER of the cluster's pinned certificate (config.Endpoint
   * .PinnedCertB64). When present, the SSO window accepts ONLY this exact
   * certificate — the secure way to trust a self-signed cluster.
   */
  pinnedCertB64?: string;
  /**
   * Marks a self-signed cluster. With no pin yet, this enables Trust-On-
   * First-Use: the cluster's cert is captured + accepted once, then returned
   * for the caller to persist as the endpoint pin. Ignored when a pin is set
   * (the pin is enforced instead).
   */
  allowInsecureTLS?: boolean;
}

export async function openSSOLogin(args: OpenSSOLoginArgs): Promise<SSOLoginResult> {
  const partitionName = `persist:sso-${args.endpointId}`;
  const ses = session.fromPartition(partitionName, { cache: false });

  const clusterHost = (() => {
    try {
      return new URL(args.prismURL).hostname;
    } catch {
      return '';
    }
  })();
  const pinSpki = args.pinnedCertB64
    ? spkiSha256(Buffer.from(stripWs(args.pinnedCertB64), 'base64'))
    : null;
  // TOFU capture, filled in by the verify proc when there is no pin yet.
  let capturedCertB64: string | undefined;
  let capturedCertSha256: string | undefined;

  // Certificate trust for the credential-bearing login window. We only govern
  // the CLUSTER host; SSO routinely bounces through external IdP hosts
  // (Okta/Authentik/AD FS/…) whose certs are publicly trusted, so for those we
  // defer to Chromium's default verification (cb(-3)) — enforcing the cluster
  // pin on an IdP host would break the most common SSO flow.
  if (pinSpki || args.allowInsecureTLS) {
    ses.setCertificateVerifyProc((req, cb) => {
      if (req.hostname !== clusterHost) {
        cb(-3); // not the cluster — use Chromium's default trust decision
        return;
      }
      const pem = req.certificate?.data ?? '';
      if (pinSpki) {
        // Enforce the pin by SubjectPublicKeyInfo hash — same anchor the
        // runtime uses, so a same-key cert rotation keeps working.
        const presented = spkiSha256(pem);
        if (presented && presented === pinSpki) {
          cb(0);
        } else {
          log.warn('SSO: cluster certificate does not match pinned SPKI — rejecting', {
            endpoint: args.endpointId,
            host: req.hostname,
          });
          cb(-2); // CERT_INVALID
        }
        return;
      }
      // No pin yet (endpoint is marked allow_insecure_tls) → Trust On First
      // Use: capture the cluster's leaf cert so the caller can persist it as
      // the endpoint's pin after a successful sign-in. Thereafter BOTH this
      // window and the runtime verify against it (the blind-trust window
      // shrinks to exactly the first connect — the VMRC / browser model).
      capturedCertB64 = certToB64Der(pem);
      capturedCertSha256 = certFingerprint(pem);
      log.warn('SSO: trusting cluster certificate on first use — it will be pinned', {
        endpoint: args.endpointId,
        host: req.hostname,
        sha256: capturedCertSha256,
      });
      cb(0);
    });
  }
  // No pin and not insecure → no proc installed → Chromium default verification
  // applies everywhere (a self-signed cluster fails until the operator marks it
  // allow_insecure_tls, which then TOFU-pins on first sign-in).

  // Block all permission requests (camera/mic/geolocation/notifications/…) on
  // the SSO partition. We deliberately do NOT inject the app's strict CSP
  // here — that would break Prism's own login scripts — but a login page has
  // no business requesting device permissions.
  ses.setPermissionRequestHandler((_wc, _perm, callback) => callback(false));
  ses.setPermissionCheckHandler(() => false);

  return new Promise<SSOLoginResult>((resolve, reject) => {
    const win = new BrowserWindow({
      width: 980,
      height: 720,
      title: 'Sign in to Prism',
      backgroundColor: '#0a0c12',
      webPreferences: {
        // No preload, no node, no contextBridge — this window only renders
        // Prism. It must NOT have access to our IPC secret.
        contextIsolation: true,
        sandbox: true,
        nodeIntegration: false,
        webSecurity: true,
        webviewTag: false,
        spellcheck: false,
        partition: partitionName,
      },
    });

    let settled = false;
    const finish = (action: () => void) => {
      if (settled) return;
      settled = true;
      action();
      if (!win.isDestroyed()) win.close();
    };

    // Deny any window.open from Prism / the IdP — every IdP we support uses
    // top-level redirects, no popups. This prevents a malicious or
    // misconfigured page from spawning a second window we cannot supervise.
    win.webContents.setWindowOpenHandler(() => ({ action: 'deny' }));

    win.on('closed', () => {
      if (!settled) {
        settled = true;
        reject(new Error('SSO login window was closed before authentication completed'));
      }
    });

    void win.loadURL(`${args.prismURL.replace(/\/$/, '')}/console/`).catch((err: unknown) => {
      finish(() => reject(err instanceof Error ? err : new Error(String(err))));
    });

    // Check the cookie jar after every meaningful navigation. The login flow
    // bounces through IdPs (which can be multiple hops), so a single check
    // is not enough. We bail out the moment a Prism session cookie lands.
    const probe = async () => {
      try {
        const cookies = await ses.cookies.get({ url: args.prismURL });
        const sessionCookie = cookies.find((c) => isSessionCookie(c));
        if (sessionCookie) {
          finish(() =>
            resolve({
              cookies: cookies.map((c) => ({ name: c.name, value: c.value })),
              capturedCertB64,
              capturedCertSha256,
            }),
          );
        }
      } catch {
        /* transient errors during navigation are fine */
      }
    };

    win.webContents.on('did-navigate', () => void probe());
    win.webContents.on('did-navigate-in-page', () => void probe());
    win.webContents.on('did-frame-finish-load', () => void probe());
    // First pass after the load attempt — covers IdPs that complete on the
    // initial Prism URL.
    win.webContents.once('did-finish-load', () => void probe());
  });
}

function isSessionCookie(c: Cookie): boolean {
  for (const name of PRISM_SESSION_COOKIE_NAMES) {
    if (c.name === name) return true;
    if (c.name.toUpperCase().includes(name)) return true;
  }
  return false;
}
