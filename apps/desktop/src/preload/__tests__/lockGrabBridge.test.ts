import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * The keyboard-grab IPC surface across the contextBridge. We mock `electron` so
 * importing the preload runs `exposeInMainWorld` in plain Node, capturing the
 * exposed API and the ipcRenderer calls. This pins the two channels and proves:
 *   - setLockGrab → invoke('…:set-lock-grab', enabled)   (renderer→main, boolean)
 *   - onLockCombo → on('…:lock-combo', …)                 (main→renderer, closed enum)
 * so a rename on either side of the bridge breaks the test, not production.
 */

const { invoke, on, off, exposed } = vi.hoisted(() => {
  const exposed: Record<string, unknown> = {};
  return {
    invoke: vi.fn(() => Promise.resolve(true)),
    on: vi.fn(),
    off: vi.fn(),
    exposed,
  };
});

vi.mock('electron', () => ({
  contextBridge: {
    exposeInMainWorld: (key: string, api: Record<string, unknown>) => {
      exposed[key] = api;
    },
  },
  ipcRenderer: { invoke, on, off },
}));

interface LockGrabAPI {
  setLockGrab: (enabled: boolean) => Promise<boolean>;
  onLockCombo: (cb: (data: { combo: string }) => void) => () => void;
}

let api: LockGrabAPI;

beforeEach(async () => {
  vi.clearAllMocks();
  vi.resetModules();
  await import('../preload');
  api = exposed.ahvRemoteConsole as LockGrabAPI;
});

describe('lock-grab preload bridge', () => {
  it('exposes setLockGrab + onLockCombo', () => {
    expect(typeof api.setLockGrab).toBe('function');
    expect(typeof api.onLockCombo).toBe('function');
  });

  it('setLockGrab invokes the set-lock-grab channel with the boolean', async () => {
    await api.setLockGrab(true);
    expect(invoke).toHaveBeenCalledWith('ahv-remote-console:set-lock-grab', true);
    await api.setLockGrab(false);
    expect(invoke).toHaveBeenCalledWith('ahv-remote-console:set-lock-grab', false);
  });

  it('onLockCombo subscribes to lock-combo and unwraps {combo} to the callback', () => {
    const cb = vi.fn();
    const unsub = api.onLockCombo(cb);
    expect(on).toHaveBeenCalledWith('ahv-remote-console:lock-combo', expect.any(Function));
    // Simulate main delivering a combo: the wrapper strips the IpcRendererEvent.
    const wrapped = on.mock.calls.find((c) => c[0] === 'ahv-remote-console:lock-combo')?.[1] as (
      e: unknown,
      p: unknown,
    ) => void;
    wrapped({}, { combo: 'AltTab' });
    expect(cb).toHaveBeenCalledWith({ combo: 'AltTab' });
    // Returns an unsubscribe that detaches the same wrapper.
    unsub();
    expect(off).toHaveBeenCalledWith('ahv-remote-console:lock-combo', wrapped);
  });
});
