/**
 * useAgent — per-tab hook that exposes the agent popover's
 * surface to ConsoleView: a connection status badge and the
 * installer-ISO mount action.
 *
 * Clipboard now flows through the vdagent serial channel (COM3),
 * so no renderer→agent handshake or framed transport is needed.
 * We mark the popover "connected" as soon as the tab is active so
 * the operator sees the mount-ISO action without a probe wait.
 */

import { useCallback, useEffect, useState } from 'react';
import type { CatalogISOImage } from '@ahv-remote-console/protocol';
import type { RFBClient } from '../rfb/client';
import { useClient } from '../app/ClientContext';

export type AgentStatus =
  | { kind: 'probing' }
  | { kind: 'connected'; version: string; os: string }
  | { kind: 'unavailable' }
  | { kind: 'installing' }
  | { kind: 'error'; reason: string };

/**
 * MountState tracks the operator-facing state of the "Mount installer
 * ISO" toolbar action. Replaces the old auto-typed install flow — now
 * the runtime asks the AHV plugin to upload the bundled ISO to Prism
 * Image Service and attach it as a CD-ROM. Operator runs
 * ahv-remote-console-agent.exe once per VM and the agent takes over.
 *
 *  - `idle`        — no mount attempted yet (or operator dismissed
 *                    the success message); UI shows the [Mount
 *                    installer ISO] button.
 *  - `mounting`    — call in flight; UI shows an indeterminate "installing"
 *                    indicator. The real per-phase percent comes from the
 *                    Tasks tray polling the runtime task (no fabricated steps).
 *  - `mounted`     — Prism accepted the upload + attach; UI shows
 *                    the success message with the drive letter hint
 *                    plus a [Re-mount] button.
 *  - `error`       — call failed (plugin doesn't support ISO mount,
 *                    Prism rejected the upload, …); UI surfaces
 *                    `reason` inline with a [Retry] button.
 */
export type AgentMountState =
  | { kind: 'idle' }
  // The install is one blocking runtime call with no live events over IT, so we
  // do NOT fabricate step numbers or timer-driven labels. The banner shows a
  // single indeterminate "installing" state; the REAL per-phase percent is
  // driven by the Tasks tray polling the runtime task (getTask). `taskId`, once
  // known, lets a caller poll that task directly.
  | { kind: 'mounting'; taskId?: string }
  | { kind: 'mounted'; driveLetterHint: string; reused: boolean; serialNote: string }
  // `code` is the runtime's machine-readable error code (AHVRemoteConsoleError
  // .code) so ConsoleView can map typed outcomes — image_publish_required
  // (catalog missing + image-create denied; upload never attempted) — to a
  // specific notice instead of the generic failure line.
  | { kind: 'mountError'; reason: string; code?: string };

/**
 * CatalogState tracks the "Mount ISO from catalog" picker's lifecycle. The
 * list is loaded on demand (when the menu opens) so a large multi-cluster
 * catalog never slows the toolbar.
 *
 *  - `idle`    — not loaded yet.
 *  - `loading` — listISOImages in flight.
 *  - `loaded`  — images available (possibly empty).
 *  - `error`   — the list call failed; `reason` is the inline message.
 */
export type CatalogState =
  | { kind: 'idle' }
  | { kind: 'loading' }
  | { kind: 'loaded'; images: CatalogISOImage[] }
  | { kind: 'error'; reason: string };

/**
 * EjectState tracks the "Eject CD-ROM" action.
 *
 *  - `idle`     — no eject attempted.
 *  - `ejecting` — call in flight.
 *  - `ejected`  — done; `ejected` is false on an idempotent no-op (drive was
 *                 already empty).
 *  - `error`    — call failed; `reason` is the inline message.
 */
export type EjectState =
  | { kind: 'idle' }
  | { kind: 'ejecting' }
  | { kind: 'ejected'; ejected: boolean }
  | { kind: 'error'; reason: string };

export interface MountISOOptions {
  /**
   * Consent re-call: ask the runtime to gracefully power-cycle the VM so
   * the agent serial channel can be attached (AHV cannot hot-add it).
   * Serialized as the `power_cycle` wire field. MUST only be set after the
   * operator explicitly confirmed via [Restart now] in the consent dialog —
   * never programmatically.
   */
  powerCycle?: boolean;
}

export interface AgentAPI {
  status: AgentStatus;
  /**
   * Local state for the installer-ISO mount action surfaced in the
   * agent popover. ConsoleView reads this to render the inline
   * status (idle / mounting / mounted / error).
   */
  mountState: AgentMountState;
  /**
   * Ask the runtime to upload + attach the agent installer ISO
   * to the session's VM. Idempotent on the runtime side — calling
   * it again after a successful mount short-circuits to a "reused"
   * result. Errors are captured into `mountState` rather than
   * thrown, since the popover surfaces them inline.
   *
   * Resolves to the resulting `AgentMountState` so callers can branch
   * on the outcome (e.g. surface the serial-port note) without reading
   * the stale `mountState` closure synchronously after the await.
   */
  mountISO: (options?: MountISOOptions) => Promise<AgentMountState>;

  // ----- arbitrary catalog ISO mount / eject -----

  /** Picker state for "Mount ISO from catalog". */
  catalogState: CatalogState;
  /** Load (or reload) the catalog ISO list on demand — call when the menu opens. */
  loadISOCatalog: () => Promise<CatalogState>;
  /**
   * Mount a chosen catalog ISO (by UUID) into the VM's CD-ROM. Maps the outcome
   * into the same `mountState` ('mounted'/'mountError', serialNote always '')
   * the agent-ISO mount uses, so ConsoleView's surfaceMountOutcome renders it.
   */
  mountChosenISO: (imageUuid: string) => Promise<AgentMountState>;
  /** State of the "Eject CD-ROM" action. */
  ejectState: EjectState;
  /** Eject the current CD-ROM media. Resolves to the outcome (ejected true/false). */
  ejectISO: () => Promise<EjectState>;
}

export interface UseAgentOptions {
  /** Lazy accessor for the active RFB client. */
  clientRef: React.MutableRefObject<RFBClient | null>;
  /** Only the active tab probes. Inactive tabs hold last-known status. */
  active: boolean;
  /**
   * The session ID this hook is bound to. Required for the
   * installer-ISO mount call, which the runtime resolves to a
   * plugin + VM. Passing it through the options (rather than a
   * separate hook arg) keeps useAgent's call-site shape stable.
   */
  sessionId: string;
}

/** The slice of the SDK client runMountISO needs — narrow so tests can stub it. */
export interface AgentMountClient {
  mountAgentISO(
    sessionId: string,
    options?: { powerCycle?: boolean },
  ): Promise<{
    imageUuid: string;
    driveLetterHint: string;
    reused: boolean;
    serialNote: string;
    taskId: string;
  }>;
  // Arbitrary catalog ISO mount / eject (additive — kept narrow for stubs).
  listISOImages(sessionId: string): Promise<{ images: CatalogISOImage[] }>;
  mountISO(
    sessionId: string,
    imageUuid: string,
  ): Promise<{ imageUuid: string; driveLetterHint: string; reused: boolean }>;
  ejectISO(sessionId: string): Promise<{ ejected: boolean }>;
}

/**
 * The mount-ISO core: SDK call + outcome mapping, pure of React so it can be
 * unit-tested directly (FE-01 idiom — the hook only binds it to component
 * state).
 *
 * No fabricated progress: the runtime call is one blocking op, so the in-flight
 * state is a single indeterminate `mounting`. The REAL per-phase percent is
 * surfaced by the Tasks tray polling the runtime task (the returned `taskId`),
 * not by a renderer-side timer that only guesses at the cadence.
 */
export async function runMountISO(
  client: AgentMountClient,
  sessionId: string,
  setMountState: (s: AgentMountState) => void,
  options?: MountISOOptions,
): Promise<AgentMountState> {
  const powerCycle = options?.powerCycle === true;
  setMountState({ kind: 'mounting' });
  try {
    const result = await client.mountAgentISO(
      sessionId,
      powerCycle ? { powerCycle: true } : undefined,
    );
    const next: AgentMountState = {
      kind: 'mounted',
      driveLetterHint: result.driveLetterHint,
      reused: result.reused,
      serialNote: result.serialNote,
    };
    setMountState(next);
    return next;
  } catch (err) {
    // The SDK throws `AHVRemoteConsoleError` with a `.message`
    // already populated from the runtime's JSON response — for
    // both the 503 (iso_mount_unsupported) and 502
    // (iso_mount_failed) paths the operator sees a one-line
    // human-readable reason. Its machine `.code` rides along so
    // typed outcomes (image_publish_required) stay mappable.
    const code = (err as { code?: unknown }).code;
    const next: AgentMountState = {
      kind: 'mountError',
      reason: String((err as Error).message ?? err),
      ...(typeof code === 'string' ? { code } : {}),
    };
    setMountState(next);
    return next;
  }
}

/**
 * Load the catalog ISO list. Pure of React (FE-01 idiom) so it can be unit
 * tested directly; the hook only binds the result to component state.
 */
export async function runLoadISOCatalog(
  client: AgentMountClient,
  sessionId: string,
  setCatalogState: (s: CatalogState) => void,
): Promise<CatalogState> {
  setCatalogState({ kind: 'loading' });
  try {
    const { images } = await client.listISOImages(sessionId);
    const next: CatalogState = { kind: 'loaded', images };
    setCatalogState(next);
    return next;
  } catch (err) {
    const next: CatalogState = {
      kind: 'error',
      reason: String((err as Error).message ?? err),
    };
    setCatalogState(next);
    return next;
  }
}

/**
 * Mount a chosen catalog ISO and map the outcome into the shared mountState
 * vocabulary ('mounted' / 'mountError'). serialNote is always '' — arbitrary
 * ISOs never add the COM3 channel — so ConsoleView's surfaceMountOutcome shows
 * only the drive-letter copy, not a channel note.
 */
export async function runMountChosenISO(
  client: AgentMountClient,
  sessionId: string,
  imageUuid: string,
  setMountState: (s: AgentMountState) => void,
): Promise<AgentMountState> {
  setMountState({ kind: 'mounting' });
  try {
    const result = await client.mountISO(sessionId, imageUuid);
    const next: AgentMountState = {
      kind: 'mounted',
      driveLetterHint: result.driveLetterHint,
      reused: result.reused,
      serialNote: '',
    };
    setMountState(next);
    return next;
  } catch (err) {
    const code = (err as { code?: unknown }).code;
    const next: AgentMountState = {
      kind: 'mountError',
      reason: String((err as Error).message ?? err),
      ...(typeof code === 'string' ? { code } : {}),
    };
    setMountState(next);
    return next;
  }
}

/** Eject the current CD-ROM media. Pure of React so it can be unit tested. */
export async function runEjectISO(
  client: AgentMountClient,
  sessionId: string,
  setEjectState: (s: EjectState) => void,
): Promise<EjectState> {
  setEjectState({ kind: 'ejecting' });
  try {
    const { ejected } = await client.ejectISO(sessionId);
    const next: EjectState = { kind: 'ejected', ejected };
    setEjectState(next);
    return next;
  } catch (err) {
    const next: EjectState = {
      kind: 'error',
      reason: String((err as Error).message ?? err),
    };
    setEjectState(next);
    return next;
  }
}

export function useAgent(opts: UseAgentOptions): AgentAPI {
  const { active, sessionId } = opts;
  const client = useClient();
  const [status, setStatus] = useState<AgentStatus>({ kind: 'probing' });
  const [mountState, setMountState] = useState<AgentMountState>({ kind: 'idle' });
  const [catalogState, setCatalogState] = useState<CatalogState>({ kind: 'idle' });
  const [ejectState, setEjectState] = useState<EjectState>({ kind: 'idle' });

  // No handshake needed — clipboard flows through vdagent serial
  // channel (COM3). Mark the agent connected when the tab activates.
  useEffect(() => {
    if (!active) return;
    setStatus({ kind: 'connected', version: '', os: '' });
  }, [active]);

  /**
   * Trigger the installer-ISO upload + CD-ROM attach via the runtime
   * (and, on the consent re-call, the orchestrated power cycle).
   * Replaces the old auto-typed install flow. Errors don't propagate
   * out of the hook (we own the inline-error rendering surface on
   * the popover); instead, every outcome maps to a `mountState`
   * transition the popover renders. See runMountISO for the core.
   */
  const mountISO = useCallback(
    (options?: MountISOOptions): Promise<AgentMountState> =>
      runMountISO(client, sessionId, setMountState, options),
    [client, sessionId],
  );

  const loadISOCatalog = useCallback(
    (): Promise<CatalogState> => runLoadISOCatalog(client, sessionId, setCatalogState),
    [client, sessionId],
  );
  const mountChosenISO = useCallback(
    (imageUuid: string): Promise<AgentMountState> =>
      runMountChosenISO(client, sessionId, imageUuid, setMountState),
    [client, sessionId],
  );
  const ejectISO = useCallback(
    (): Promise<EjectState> => runEjectISO(client, sessionId, setEjectState),
    [client, sessionId],
  );

  return {
    status,
    mountState,
    mountISO,
    catalogState,
    loadISOCatalog,
    mountChosenISO,
    ejectState,
    ejectISO,
  };
}
