/**
 * reopenSessionStream — the self-heal primitive for a console pop-out's stream.
 *
 * The failure it fixes: the renderer↔runtime WebSocket drops AND the runtime no
 * longer has the session (it restarted and lost its session table, or reaped the
 * session as idle). Streaming the old id can then NEVER connect, so the operator
 * is stranded on "Connecting…" or a Reconnect button that does nothing.
 *
 * Behaviour:
 *  - session still live  → open its stream (the common reconnect).
 *  - session gone + we captured its identity while it was alive → re-mint a fresh
 *    session (openSession) and stream THAT, reporting the new id via onReminted.
 *  - session gone + no identity → throw (we can't recreate it here).
 *
 * Extracted from ConsoleWindowApp so the three branches are unit-testable; the
 * component just wires its refs/state setter to it.
 */

import type { AHVRemoteConsoleClient } from '@ahv-remote-console/sdk';

export interface SessionIdentity {
  pluginId: string;
  resourceId: string;
}

export async function reopenSessionStream(
  c: Pick<AHVRemoteConsoleClient, 'listSessions' | 'openSession' | 'openStream'>,
  currentId: string,
  identity: SessionIdentity | null,
  onReminted: (newId: string) => void,
): Promise<WebSocket> {
  let alive: boolean;
  try {
    const { sessions } = await c.listSessions();
    alive = sessions.some((s) => s.id === currentId);
  } catch {
    // listSessions failed (runtime mid-restart / transient). Don't assume the
    // session is gone — try the cheap stream open; a real gap heals on the next
    // close, eventually catching a clean listSessions that re-mints.
    alive = true;
  }
  if (alive) return c.openStream(currentId);
  if (!identity) {
    throw new Error('This VM session expired and can’t be reopened from this window.');
  }
  const { session } = await c.openSession(identity.pluginId, identity.resourceId);
  onReminted(session.id);
  return c.openStream(session.id);
}
