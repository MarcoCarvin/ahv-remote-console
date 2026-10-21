/**
 * Stored-endpoint lookup for the SSO login handler (RED-01).
 *
 * The `open-sso-login` IPC used to trust the renderer-supplied prismURL,
 * allowInsecureTLS, and pinnedCertB64 verbatim. A compromised renderer could
 * therefore aim the credential-bearing SSO window at an attacker host and/or
 * disable TLS verification. The fix: look the endpoint up by its stored
 * `endpointId` in the runtime config and derive the authoritative host + TLS
 * settings from there, ignoring the renderer's TLS inputs and requiring the
 * renderer's prismURL host to equal the stored endpoint host.
 *
 * The runtime fetch is injected (`EndpointFetcher`) so the host-matching and
 * selection logic can be unit-tested without Electron / the live runtime.
 */

import type { Endpoint } from '@ahv-remote-console/protocol';

/**
 * Fetches the runtime's endpoint list. In production this is a signed
 * net.request against `${handshake.endpoint}/v1/config/endpoints`; in tests it
 * is a stub. May reject if the runtime is unreachable.
 */
export type EndpointFetcher = () => Promise<Endpoint[]>;

/** The authoritative TLS posture + host for an endpoint, read from the runtime. */
export interface ResolvedEndpointTLS {
  /** Stored endpoint URL host (URL.host — includes port if non-default). */
  host: string;
  /** Stored allow_insecure_tls — NOT the renderer-passed value. */
  allowInsecureTLS: boolean;
  /** Stored pinned cert (base64 DER), or undefined. NOT renderer-passed. */
  pinnedCertB64?: string;
}

/** Parse a URL's host (host:port form). Returns '' if unparseable. */
export function urlHost(u: string): string {
  try {
    return new URL(u).host;
  } catch {
    return '';
  }
}

/**
 * Resolve the stored endpoint's authoritative host + TLS values and assert the
 * renderer-supplied prismURL targets that same host. Throws on:
 *   - unknown endpointId (not in runtime config),
 *   - a stored endpoint whose URL is unparseable,
 *   - a renderer prismURL whose host differs from the stored endpoint host.
 *
 * On success the returned TLS values come ENTIRELY from the stored endpoint;
 * the caller must use these and discard any renderer-passed TLS inputs.
 */
export function resolveStoredEndpointTLS(
  endpoints: Endpoint[],
  endpointId: string,
  rendererPrismURL: string,
): ResolvedEndpointTLS {
  const stored = endpoints.find((e) => e.id === endpointId);
  if (!stored) {
    throw new Error(`open-sso-login: unknown endpointId ${endpointId}`);
  }
  const storedHost = urlHost(stored.url);
  if (!storedHost) {
    throw new Error(`open-sso-login: stored endpoint ${endpointId} has an invalid URL`);
  }
  const rendererHostValue = urlHost(rendererPrismURL);
  if (rendererHostValue !== storedHost) {
    throw new Error(
      `open-sso-login: prismURL host ${rendererHostValue || '<invalid>'} does not match ` +
        `stored endpoint host ${storedHost}`,
    );
  }
  return {
    host: storedHost,
    allowInsecureTLS: Boolean(stored.allow_insecure_tls),
    pinnedCertB64:
      typeof stored.pinned_cert_b64 === 'string' && stored.pinned_cert_b64
        ? stored.pinned_cert_b64
        : undefined,
  };
}
