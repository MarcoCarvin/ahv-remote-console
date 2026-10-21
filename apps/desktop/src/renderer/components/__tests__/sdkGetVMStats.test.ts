/**
 * Wire-contract round-trip test for AHVRemoteConsoleClient.getVMStats —
 * the renderer-visible side of the GET /v1/vm/stats contract:
 *
 *   - plugin_id + resource_id ride the query string (a GET), both
 *     URL-encoded — the resource_id slash is percent-escaped;
 *   - the { stats: VMStats } reply round-trips untouched, including the
 *     -1 unavailable sentinel.
 *
 * Lives in the desktop suite because packages/* have no test runner — the
 * desktop app is the SDK's consumer and its vitest host exercises the built
 * @ahv-remote-console/sdk package (build packages first).
 */

import { describe, it, expect, vi, afterEach } from 'vitest';
import { AHVRemoteConsoleClient, type Signer, type VMStats } from '@ahv-remote-console/sdk';

const signer: Signer = {
  signRequest: () => ({ 'X-HC-Sig': 'test' }),
  signWebSocket: () => 'test',
};

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

function makeClient(): AHVRemoteConsoleClient {
  return new AHVRemoteConsoleClient({ endpoint: 'http://127.0.0.1:7', signer });
}

const STATS: VMStats = {
  cpu_usage_ppm: 250000,
  memory_usage_ppm: 500000,
  disk_read_bps: 1024,
  disk_write_bps: 2048,
  net_rx_bps: -1,
  net_tx_bps: 4096,
  sampled_at: '2026-06-14T00:00:00Z',
};

describe('getVMStats wire contract', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('GETs /v1/vm/stats with both query params URL-encoded', async () => {
    const fetchMock = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(jsonResponse({ stats: STATS }));
    await makeClient().getVMStats('nutanix-ahv', 'ep-1/vm-1');
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(
      'http://127.0.0.1:7/v1/vm/stats?plugin_id=nutanix-ahv&resource_id=ep-1%2Fvm-1',
    );
    expect(init?.method).toBe('GET');
    expect(init?.body ?? null).toBeNull();
  });

  it('round-trips the { stats } reply incl. the -1 unavailable sentinel', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ stats: STATS }));
    const result = await makeClient().getVMStats('nutanix-ahv', 'ep-1/vm-1');
    expect(result.stats).toEqual(STATS);
    expect(result.stats.net_rx_bps).toBe(-1);
  });
});
