/**
 * Tests for runMountISO — the mount-ISO core extracted from useAgent. Same
 * FE-01 idiom as consoleFocusPolicy.test.ts: the pure async core is tested
 * directly in the node environment, the hook only binds it to React state.
 *
 * The behavior pinned here:
 *   - the consent re-call ([Restart now] in the prerequisite dialog) passes
 *     powerCycle:true through to the SDK; a plain mount sends NO option at
 *     all (the wire field defaults to absent);
 *   - the in-flight state is a SINGLE indeterminate `mounting` with NO
 *     fabricated step numbers / timer-driven labels (the real percent is
 *     surfaced by the Tasks tray via the runtime task);
 *   - results and thrown SDK errors map to the same AgentMountState
 *     transitions the popover renders.
 */

import { describe, it, expect, vi } from 'vitest';
import { AHVRemoteConsoleError } from '@ahv-remote-console/sdk';
import {
  runMountISO,
  runLoadISOCatalog,
  runMountChosenISO,
  runEjectISO,
  type AgentMountClient,
  type AgentMountState,
  type CatalogState,
  type EjectState,
} from '../useAgent';

const OK_RESULT = {
  imageUuid: 'img-1',
  driveLetterHint: 'D:',
  reused: true,
  serialNote: 'serial_port_added_cycled',
  taskId: 'task-1',
};

/**
 * A full AgentMountClient with all four methods stubbed. The runMountISO tests
 * only override mountAgentISO; the catalog tests override the catalog methods.
 * Defaults reject so an unexpected call fails loudly.
 */
function makeClient(overrides: Partial<AgentMountClient> = {}): AgentMountClient {
  return {
    mountAgentISO: vi.fn().mockResolvedValue(OK_RESULT),
    listISOImages: vi.fn().mockRejectedValue(new Error('listISOImages not stubbed')),
    mountISO: vi.fn().mockRejectedValue(new Error('mountISO not stubbed')),
    ejectISO: vi.fn().mockRejectedValue(new Error('ejectISO not stubbed')),
    ...overrides,
  };
}

function collect(): { states: AgentMountState[]; push: (s: AgentMountState) => void } {
  const states: AgentMountState[] = [];
  return { states, push: (s) => states.push(s) };
}

describe('runMountISO', () => {
  it('passes powerCycle:true through to the SDK on the consent re-call', async () => {
    const mountAgentISO = vi.fn().mockResolvedValue(OK_RESULT);
    const { states, push } = collect();
    const next = await runMountISO(makeClient({ mountAgentISO }), 'sess-1', push, {
      powerCycle: true,
    });
    expect(mountAgentISO).toHaveBeenCalledTimes(1);
    expect(mountAgentISO.mock.calls[0]![0]).toBe('sess-1');
    expect(mountAgentISO.mock.calls[0]![1]).toEqual({ powerCycle: true });
    expect(next).toEqual({
      kind: 'mounted',
      driveLetterHint: 'D:',
      reused: true,
      serialNote: 'serial_port_added_cycled',
    });
    expect(states.at(-1)).toEqual(next);
  });

  it('sends no power-cycle option on a plain mount (wire default = absent)', async () => {
    const mountAgentISO = vi.fn().mockResolvedValue(OK_RESULT);
    const { push } = collect();
    await runMountISO(makeClient({ mountAgentISO }), 'sess-1', push);
    expect(mountAgentISO.mock.calls[0]![0]).toBe('sess-1');
    expect(mountAgentISO.mock.calls[0]![1]).toBeUndefined();
  });

  it('uses a single indeterminate mounting state — no fabricated steps/labels', async () => {
    let resolveCall!: (v: typeof OK_RESULT) => void;
    const mountAgentISO = vi.fn().mockReturnValue(
      new Promise((r) => {
        resolveCall = r;
      }),
    );
    const { states, push } = collect();
    const p = runMountISO(makeClient({ mountAgentISO }), 'sess-1', push, { powerCycle: true });

    // The in-flight state is set synchronously and is a bare `mounting` — no
    // step/steps/message/detail timer machinery survives.
    expect(states[0]).toEqual({ kind: 'mounting' });
    for (const s of states) {
      if (s.kind !== 'mounting') continue;
      expect(s).not.toHaveProperty('step');
      expect(s).not.toHaveProperty('steps');
      expect(s).not.toHaveProperty('message');
      expect(s).not.toHaveProperty('detail');
    }

    resolveCall(OK_RESULT);
    const next = await p;
    expect(next).toMatchObject({ kind: 'mounted', serialNote: 'serial_port_added_cycled' });
    // Exactly two transitions: mounting -> mounted. No timer-driven churn.
    expect(states).toHaveLength(2);
  });

  it('maps a thrown SDK error to mountError', async () => {
    const mountAgentISO = vi.fn().mockRejectedValue(new Error('prism said no'));
    const { states, push } = collect();
    const next = await runMountISO(makeClient({ mountAgentISO }), 'sess-1', push);
    expect(next).toEqual({ kind: 'mountError', reason: 'prism said no' });
    expect(states.at(-1)).toEqual(next);
  });

  it('carries the typed SDK error code (image_publish_required) into mountError', async () => {
    // The REAL error class the SDK throws: toError() parses the runtime's
    // {error:{code,message}} body (the router maps the plugin's
    // sdk.ErrImagePublishRequired sentinel to wire code
    // "image_publish_required" — pinned Go-side) into an
    // AHVRemoteConsoleError whose `code` ConsoleView maps to the
    // publish-image notice instead of the generic failure line.
    const err = new AHVRemoteConsoleError(
      'image_publish_required',
      'agent image not in catalog',
      502,
    );
    const mountAgentISO = vi.fn().mockRejectedValue(err);
    const { states, push } = collect();
    const next = await runMountISO(makeClient({ mountAgentISO }), 'sess-1', push);
    expect(next).toEqual({
      kind: 'mountError',
      reason: 'agent image not in catalog',
      code: 'image_publish_required',
    });
    expect(states.at(-1)).toEqual(next);
  });
});

describe('runLoadISOCatalog', () => {
  it('loads the catalog and transitions loading -> loaded', async () => {
    const images = [
      { uuid: 'iso-1', name: 'win.iso', state: 'COMPLETE' },
      { uuid: 'iso-2', name: 'rhel.iso', state: 'COMPLETE' },
    ];
    const listISOImages = vi.fn().mockResolvedValue({ images });
    const states: CatalogState[] = [];
    const next = await runLoadISOCatalog(makeClient({ listISOImages }), 'sess-1', (s) =>
      states.push(s),
    );
    expect(listISOImages).toHaveBeenCalledWith('sess-1');
    expect(states[0]).toEqual({ kind: 'loading' });
    expect(next).toEqual({ kind: 'loaded', images });
    expect(states.at(-1)).toEqual(next);
  });

  it('maps a list failure to an error state', async () => {
    const listISOImages = vi.fn().mockRejectedValue(new Error('catalog walk failed'));
    const states: CatalogState[] = [];
    const next = await runLoadISOCatalog(makeClient({ listISOImages }), 'sess-1', (s) =>
      states.push(s),
    );
    expect(next).toEqual({ kind: 'error', reason: 'catalog walk failed' });
    expect(states.at(-1)).toEqual(next);
  });
});

describe('runMountChosenISO', () => {
  it('mounts by UUID and maps to mounted with serialNote always empty', async () => {
    const mountISO = vi
      .fn()
      .mockResolvedValue({ imageUuid: 'iso-1', driveLetterHint: 'D:', reused: false });
    const states: AgentMountState[] = [];
    const next = await runMountChosenISO(makeClient({ mountISO }), 'sess-1', 'iso-1', (s) =>
      states.push(s),
    );
    expect(mountISO).toHaveBeenCalledWith('sess-1', 'iso-1');
    expect(states[0]).toMatchObject({ kind: 'mounting' });
    expect(next).toEqual({
      kind: 'mounted',
      driveLetterHint: 'D:',
      reused: false,
      serialNote: '',
    });
    expect(states.at(-1)).toEqual(next);
  });

  it('maps a thrown SDK error to mountError', async () => {
    const mountISO = vi.fn().mockRejectedValue(new Error('image not on cluster'));
    const states: AgentMountState[] = [];
    const next = await runMountChosenISO(makeClient({ mountISO }), 'sess-1', 'iso-1', (s) =>
      states.push(s),
    );
    expect(next).toEqual({ kind: 'mountError', reason: 'image not on cluster' });
  });
});

describe('runEjectISO', () => {
  it('ejects and reports ejected=true', async () => {
    const ejectISO = vi.fn().mockResolvedValue({ ejected: true });
    const states: EjectState[] = [];
    const next = await runEjectISO(makeClient({ ejectISO }), 'sess-1', (s) => states.push(s));
    expect(ejectISO).toHaveBeenCalledWith('sess-1');
    expect(states[0]).toEqual({ kind: 'ejecting' });
    expect(next).toEqual({ kind: 'ejected', ejected: true });
  });

  it('reports ejected=false on an idempotent no-op (empty drive)', async () => {
    const ejectISO = vi.fn().mockResolvedValue({ ejected: false });
    const next = await runEjectISO(makeClient({ ejectISO }), 'sess-1', () => {
      /* noop */
    });
    expect(next).toEqual({ kind: 'ejected', ejected: false });
  });

  it('maps a thrown SDK error to error', async () => {
    const ejectISO = vi.fn().mockRejectedValue(new Error('eject failed'));
    const next = await runEjectISO(makeClient({ ejectISO }), 'sess-1', () => {
      /* noop */
    });
    expect(next).toEqual({ kind: 'error', reason: 'eject failed' });
  });
});
