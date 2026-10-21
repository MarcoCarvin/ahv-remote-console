import { describe, expect, it } from 'vitest';
import type { Endpoint } from '@ahv-remote-console/protocol';
import { resolveStoredEndpointTLS, urlHost } from '../endpointLookup';

/**
 * RED-01: open-sso-login must derive host + TLS from the STORED endpoint, not
 * from renderer-supplied values. These tests prove the renderer prismURL host
 * is required to equal the stored endpoint host, and that the returned TLS
 * posture comes from the stored record (renderer TLS inputs are irrelevant
 * here — this function never receives them).
 */

const ep = (over: Partial<Endpoint>): Endpoint => ({
  id: 'ep-1',
  label: 'Cluster A',
  plugin_id: 'ahv',
  url: 'https://prism.corp.example:9440',
  auth_methods: ['cookie'],
  has_pinned_cert: false,
  allow_insecure_tls: false,
  ...over,
});

describe('urlHost', () => {
  it('includes a non-default port', () => {
    expect(urlHost('https://prism.corp.example:9440')).toBe('prism.corp.example:9440');
  });
  it('omits a default https port', () => {
    expect(urlHost('https://prism.corp.example')).toBe('prism.corp.example');
  });
  it('returns empty for an unparseable URL', () => {
    expect(urlHost('not a url')).toBe('');
  });
});

describe('resolveStoredEndpointTLS', () => {
  it('resolves a known endpoint when the renderer host matches', () => {
    const out = resolveStoredEndpointTLS(
      [ep({})],
      'ep-1',
      'https://prism.corp.example:9440/console/',
    );
    expect(out.host).toBe('prism.corp.example:9440');
    expect(out.allowInsecureTLS).toBe(false);
    expect(out.pinnedCertB64).toBeUndefined();
  });

  it('returns the STORED pin + insecure flag (ignoring whatever the caller might pass elsewhere)', () => {
    const out = resolveStoredEndpointTLS(
      [ep({ allow_insecure_tls: true, pinned_cert_b64: 'AAAA', has_pinned_cert: true })],
      'ep-1',
      'https://prism.corp.example:9440',
    );
    expect(out.allowInsecureTLS).toBe(true);
    expect(out.pinnedCertB64).toBe('AAAA');
  });

  it('throws for an unknown endpointId', () => {
    expect(() =>
      resolveStoredEndpointTLS([ep({})], 'ep-unknown', 'https://prism.corp.example:9440'),
    ).toThrow(/unknown endpointId/);
  });

  it('throws when the renderer prismURL host differs from the stored host (RED-01 core)', () => {
    expect(() =>
      resolveStoredEndpointTLS([ep({})], 'ep-1', 'https://evil.attacker.example:9440/console/'),
    ).toThrow(/does not match stored endpoint host/);
  });

  it('throws when the renderer prismURL only differs by port (host:port must match exactly)', () => {
    expect(() =>
      resolveStoredEndpointTLS([ep({})], 'ep-1', 'https://prism.corp.example:443'),
    ).toThrow(/does not match stored endpoint host/);
  });

  it('throws when the stored endpoint URL is invalid', () => {
    expect(() =>
      resolveStoredEndpointTLS([ep({ url: 'garbage' })], 'ep-1', 'https://prism.corp.example:9440'),
    ).toThrow(/invalid URL/);
  });

  it('throws when the renderer prismURL is unparseable', () => {
    expect(() => resolveStoredEndpointTLS([ep({})], 'ep-1', 'not-a-url')).toThrow(
      /does not match stored endpoint host/,
    );
  });
});
