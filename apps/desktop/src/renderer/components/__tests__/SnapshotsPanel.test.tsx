/**
 * SnapshotsPanel safety tests.
 *
 * The renderer test harness runs in node (no DOM / no @testing-library), so —
 * per the FE-01 idiom used across this codebase (TasksTray.test, capabilityGate
 * .test, StatsPanel.test) — the load-bearing SAFETY decisions of the panel are
 * tested as pure functions, and the SDK wire contract the panel relies on
 * (create / list / restore) is tested via a stubbed fetch:
 *
 *   - snapshotControlsDisabled: write controls are disabled when vm_update is a
 *     proven denial (gateCanWrite=false) OR an op is in flight (busy);
 *   - restoreConfirmCopy: the mandatory-modal body NAMES the VM + the snapshot +
 *     its timestamp and states HONESTLY that v3 restore CREATES A NEW VM (it is
 *     NOT an in-place revert of this VM);
 *   - snapshotMetaLine: the per-row meta line joins creation time, type, and an
 *     expiry note, omitting absent fields;
 *   - the SDK calls the panel issues: createSnapshot POSTs the typed name;
 *     listSnapshots GETs the per-VM list; restoreSnapshot POSTs the snapshot
 *     uuid to the /restore path (the only call that restores).
 */

import { describe, it, expect, vi, afterEach } from 'vitest';
import { AHVRemoteConsoleClient, type Signer, type SnapshotInfo } from '@ahv-remote-console/sdk';
import {
  snapshotControlsDisabled,
  createConfirmCopy,
  restoreConfirmCopy,
  snapshotMetaLine,
} from '../SnapshotsPanel';

const SNAP: SnapshotInfo = {
  uuid: 'snap-abc',
  name: 'before-upgrade',
  created_at: '2026-06-14T00:00:00Z',
  expires_at: '2026-12-31T00:00:00Z',
  state: 'COMPLETE',
  snapshot_type: 'CRASH_CONSISTENT',
};

describe('snapshotControlsDisabled — capability + busy gating', () => {
  it('is enabled when vm_update is not denied and no op is in flight', () => {
    expect(snapshotControlsDisabled(true, false)).toBe(false);
  });

  it('is DISABLED when vm_update is a proven denial (gateCanWrite=false)', () => {
    expect(snapshotControlsDisabled(false, false)).toBe(true);
  });

  it('is DISABLED while an op is in flight (busy), even if writable', () => {
    expect(snapshotControlsDisabled(true, true)).toBe(true);
  });
});

describe('createConfirmCopy — create confirmation body', () => {
  it('names the VM and the snapshot name', () => {
    const copy = createConfirmCopy('prod-db-01', 'before-upgrade');
    expect(copy).toContain('prod-db-01'); // names the VM
    expect(copy).toContain('before-upgrade'); // names the snapshot
    expect(copy).toMatch(/create/i);
  });
});

describe('restoreConfirmCopy — mandatory confirmation body', () => {
  it('names the VM and the snapshot, and is HONEST that restore creates a new VM', () => {
    const copy = restoreConfirmCopy('prod-db-01', SNAP);
    expect(copy).toContain('prod-db-01'); // names the VM
    expect(copy).toContain('before-upgrade'); // names the snapshot
    expect(copy).toContain('2026-06-14T00:00:00Z'); // names the timestamp
    expect(copy).toMatch(/new vm/i); // honest: a new VM is created
    expect(copy).toMatch(/not revert .* in place|does not revert/i); // not an in-place revert
  });

  it('omits the timestamp clause cleanly when created_at is absent', () => {
    const copy = restoreConfirmCopy('vm-x', { uuid: 'u', name: 'snap' });
    expect(copy).toContain('vm-x');
    expect(copy).toContain('"snap"');
    expect(copy).not.toContain('taken ');
  });
});

describe('snapshotMetaLine — per-row meta', () => {
  it('joins creation time, type, and expiry note', () => {
    expect(snapshotMetaLine(SNAP)).toBe(
      '2026-06-14T00:00:00Z · CRASH_CONSISTENT · expires 2026-12-31T00:00:00Z',
    );
  });

  it('omits absent fields and returns empty when nothing is known', () => {
    expect(snapshotMetaLine({ uuid: 'u', name: 'n', created_at: '2026-06-14T00:00:00Z' })).toBe(
      '2026-06-14T00:00:00Z',
    );
    expect(snapshotMetaLine({ uuid: 'u', name: 'n' })).toBe('');
  });
});

// ---- SDK wire contract the panel depends on ----

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

describe('Snapshots SDK wire contract', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('createSnapshot POSTs the typed name to /v1/vm/snapshots', async () => {
    const fetchMock = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(jsonResponse({ uuid: 'snap-new', name: 'before-upgrade' }));
    const out = await makeClient().createSnapshot('nutanix-ahv', 'ep-1/vm-1', 'before-upgrade');
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe('http://127.0.0.1:7/v1/vm/snapshots');
    expect(init?.method).toBe('POST');
    expect(JSON.parse((init?.body as string) ?? '{}')).toEqual({
      plugin_id: 'nutanix-ahv',
      resource_id: 'ep-1/vm-1',
      name: 'before-upgrade',
    });
    expect(out.uuid).toBe('snap-new');
  });

  it('listSnapshots GETs the per-VM list with query params encoded', async () => {
    const fetchMock = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(jsonResponse({ snapshots: [SNAP] }));
    const res = await makeClient().listSnapshots('nutanix-ahv', 'ep-1/vm-1');
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(
      'http://127.0.0.1:7/v1/vm/snapshots?plugin_id=nutanix-ahv&resource_id=ep-1%2Fvm-1',
    );
    expect(init?.method).toBe('GET');
    expect(res.snapshots).toEqual([SNAP]);
  });

  it('restoreSnapshot POSTs the snapshot uuid to the DESTRUCTIVE /restore path', async () => {
    const fetchMock = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(jsonResponse({ status: 'ok' }));
    await makeClient().restoreSnapshot('nutanix-ahv', 'ep-1/vm-1', 'snap-abc');
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe('http://127.0.0.1:7/v1/vm/snapshots/restore');
    expect(init?.method).toBe('POST');
    expect(JSON.parse((init?.body as string) ?? '{}')).toEqual({
      plugin_id: 'nutanix-ahv',
      resource_id: 'ep-1/vm-1',
      snapshot_id: 'snap-abc',
    });
  });
});
