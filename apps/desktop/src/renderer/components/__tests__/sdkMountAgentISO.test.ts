/**
 * Wire-contract round-trip test for AHVRemoteConsoleClient.mountAgentISO —
 * the renderer-visible side of the POST /v1/agent/mount-iso contract:
 *
 *   - the optional `power_cycle` request field is serialized ONLY as an
 *     explicit true (operator consent); a plain mount leaves it absent
 *     (NOT false) per the frozen wire contract;
 *   - the two new SerialNote values (`serial_port_deferred`,
 *     `serial_port_added_cycled`) round-trip untouched through the client.
 *
 * Lives in the desktop suite because packages/* have no test runner —
 * the desktop app is the SDK's consumer and its vitest host exercises the
 * built @ahv-remote-console/sdk package (build packages first).
 */

import { describe, it, expect, vi, afterEach } from 'vitest';
import { AHVRemoteConsoleClient, type Signer } from '@ahv-remote-console/sdk';

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

const REPLY = {
  image_uuid: 'img-uuid',
  drive_letter_hint: 'D:',
  reused: true,
  serial_note: 'serial_port_added_cycled',
  task_id: 'task-abc',
};

describe('mountAgentISO wire contract', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('serializes power_cycle:true on the consent re-call', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse(REPLY));
    await makeClient().mountAgentISO('sess-1', { powerCycle: true });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe('http://127.0.0.1:7/v1/agent/mount-iso');
    expect(init?.method).toBe('POST');
    expect(JSON.parse(init?.body as string)).toEqual({
      session_id: 'sess-1',
      power_cycle: true,
    });
  });

  it('leaves power_cycle absent (not false) on a plain mount', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse(REPLY));
    await makeClient().mountAgentISO('sess-1');
    expect(JSON.parse(fetchMock.mock.calls[0]![1]?.body as string)).toEqual({
      session_id: 'sess-1',
    });
  });

  it('leaves power_cycle absent when the option is explicitly false', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse(REPLY));
    await makeClient().mountAgentISO('sess-1', { powerCycle: false });
    expect(JSON.parse(fetchMock.mock.calls[0]![1]?.body as string)).toEqual({
      session_id: 'sess-1',
    });
  });

  it.each(['serial_port_deferred', 'serial_port_added_cycled'])(
    'round-trips the new serial note %s',
    async (note) => {
      vi.spyOn(globalThis, 'fetch').mockResolvedValue(
        jsonResponse({ ...REPLY, serial_note: note }),
      );
      const result = await makeClient().mountAgentISO('sess-1');
      expect(result).toEqual({
        imageUuid: 'img-uuid',
        driveLetterHint: 'D:',
        reused: true,
        serialNote: note,
        taskId: 'task-abc',
      });
    },
  );

  it('defaults taskId to empty string when the runtime omits task_id', async () => {
    // eslint-disable-next-line @typescript-eslint/no-unused-vars -- destructure to omit task_id
    const { task_id: _omit, ...withoutTaskId } = REPLY;
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse(withoutTaskId));
    const result = await makeClient().mountAgentISO('sess-1');
    expect(result.taskId).toBe('');
  });
});
