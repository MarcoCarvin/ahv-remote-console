import { describe, it, expect, vi } from 'vitest';
import { reopenSessionStream } from '../reopenStream';

// Minimal client double — only the three methods reopenSessionStream touches.
function makeClient(over: {
  sessions?: { id: string }[];
  listThrows?: boolean;
  openSessionId?: string;
}) {
  const sentinelWs = (tag: string) => ({ __tag: tag }) as unknown as WebSocket;
  const listSessions = vi.fn(() =>
    over.listThrows
      ? Promise.reject(new Error('runtime restarting'))
      : Promise.resolve({ sessions: (over.sessions ?? []) as never }),
  );
  const openSession = vi.fn((pluginId: string, resourceId: string) =>
    Promise.resolve({
      session: {
        id: over.openSessionId ?? 'minted',
        plugin_id: pluginId,
        resource_id: resourceId,
        opened_at: '',
      },
    }),
  );
  const openStream = vi.fn((id: string) => Promise.resolve(sentinelWs(id)));
  return {
    client: { listSessions, openSession, openStream },
    listSessions,
    openSession,
    openStream,
  };
}

describe('reopenSessionStream', () => {
  it('streams the same id when the session is still live (no re-mint)', async () => {
    const { client, openSession, openStream } = makeClient({ sessions: [{ id: 'live-1' }] });
    const onReminted = vi.fn();

    const ws = await reopenSessionStream(client, 'live-1', null, onReminted);

    expect(openSession).not.toHaveBeenCalled();
    expect(onReminted).not.toHaveBeenCalled();
    expect(openStream).toHaveBeenCalledWith('live-1');
    expect((ws as unknown as { __tag: string }).__tag).toBe('live-1');
  });

  it('re-mints from captured identity when the session is gone', async () => {
    const { client, openSession, openStream } = makeClient({
      sessions: [{ id: 'someone-else' }], // our id is absent → gone
      openSessionId: 'fresh-9',
    });
    const onReminted = vi.fn();

    const ws = await reopenSessionStream(
      client,
      'dead-1',
      { pluginId: 'ahv', resourceId: 'ep/vm-uuid' },
      onReminted,
    );

    expect(openSession).toHaveBeenCalledWith('ahv', 'ep/vm-uuid');
    expect(onReminted).toHaveBeenCalledWith('fresh-9');
    expect(openStream).toHaveBeenCalledWith('fresh-9');
    expect((ws as unknown as { __tag: string }).__tag).toBe('fresh-9');
  });

  it('throws when the session is gone and no identity was captured', async () => {
    const { client, openStream } = makeClient({ sessions: [] });
    await expect(reopenSessionStream(client, 'dead-1', null, vi.fn())).rejects.toThrow(/reopened/);
    expect(openStream).not.toHaveBeenCalled();
  });

  it('treats a failed listSessions as "assume live" and streams the current id', async () => {
    const { client, openSession, openStream } = makeClient({ listThrows: true });
    const onReminted = vi.fn();

    const ws = await reopenSessionStream(
      client,
      'live-1',
      { pluginId: 'ahv', resourceId: 'ep/vm' },
      onReminted,
    );

    // A transient listSessions failure must NOT trigger a spurious re-mint.
    expect(openSession).not.toHaveBeenCalled();
    expect(onReminted).not.toHaveBeenCalled();
    expect(openStream).toHaveBeenCalledWith('live-1');
    expect((ws as unknown as { __tag: string }).__tag).toBe('live-1');
  });
});
