/**
 * SnapshotsPanel — the console toolbar's VM-snapshot surface.
 *
 * Lists the VM's snapshots (newest-first), offers a name field + Create button,
 * and a per-row Restore button.
 *
 * NEITHER write is one-click. CREATE only OPENS a confirm modal (informational,
 * since creating is non-destructive) that names the VM + snapshot; only its
 * confirm button calls createSnapshot — so clicking Create can never silently
 * take a snapshot. (Listing/opening the panel is read-only and never creates.)
 *
 * RESTORE is NEVER one-click: clicking a row's Restore only OPENS a mandatory
 * confirmation modal that NAMES the VM + the snapshot + its timestamp. On v3 a
 * restore CREATES A NEW VM from the recovery point — it is NOT an in-place
 * revert of this VM — and the modal copy says so honestly. ONLY the confirm
 * button in that modal calls sdkClient.restoreSnapshot; Cancel is the
 * visually-primary default.
 *
 * Create / Restore controls gate on the per-endpoint capability mirror via
 * `gateCanWrite` — PROVEN-denial only (vm_update !== 'denied' upstream); an
 * "unknown" capability leaves the controls enabled (fail-at-attempt).
 *
 * Per the FE-01 idiom used across this renderer (the test harness runs in node,
 * with no DOM), the pure decisions that make this component safe — whether the
 * write controls are disabled, and the exact irreversibility copy — are exported
 * as pure functions and unit-tested directly.
 */

import { useCallback, useEffect, useState } from 'react';
import type { AHVRemoteConsoleClient, SnapshotInfo } from '@ahv-remote-console/sdk';
import { Icon } from './Icon';
import { Overlay } from './console/Overlay';

/** The SDK surface this panel uses — narrowed so tests can stub it. */
export type SnapshotsSDK = Pick<
  AHVRemoteConsoleClient,
  'createSnapshot' | 'listSnapshots' | 'restoreSnapshot'
>;

export interface SnapshotsPanelProps {
  sdkClient: SnapshotsSDK;
  pluginId: string;
  resourceId: string;
  vmName: string;
  toast: (text: string, kind?: 'info' | 'ok' | 'err') => void;
  /** PROVEN-denial gate: false only when vm_update is a proven upstream denial.
   *  Disables the Create field and every Restore button (and the modal confirm).
   */
  gateCanWrite: boolean;
  onClose: () => void;
}

/** Write controls (Create, Restore, modal confirm) are disabled when the role's
 *  vm_update is a proven denial OR an op is already in flight. Pure for testing. */
// eslint-disable-next-line react-refresh/only-export-components -- intentional: testable helper co-located for unit tests (FE-01)
export function snapshotControlsDisabled(gateCanWrite: boolean, busy: boolean): boolean {
  return !gateCanWrite || busy;
}

/** The Create-confirmation body. Creating a snapshot is non-destructive but
 *  consumes cluster storage and shouldn't be a surprise, so Create is gated by a
 *  lightweight confirm that NAMES the VM + the snapshot. Pure for testing. */
// eslint-disable-next-line react-refresh/only-export-components -- intentional: testable helper co-located for unit tests (FE-01)
export function createConfirmCopy(vmName: string, name: string): string {
  return `Create a snapshot named "${name}" of ${vmName}? This captures the VM's current crash-consistent state as a recovery point.`;
}

/** The mandatory restore-confirmation body. NAMES the source VM + snapshot +
 *  timestamp and states HONESTLY that v3 restore CREATES A NEW VM from the
 *  recovery point (it does NOT revert this VM in place). Pure for testing. */
// eslint-disable-next-line react-refresh/only-export-components -- intentional: testable helper co-located for unit tests (FE-01)
export function restoreConfirmCopy(vmName: string, snap: SnapshotInfo): string {
  const when = snap.created_at ? ` taken ${snap.created_at}` : '';
  return `This restores the snapshot "${snap.name}"${when} of ${vmName} by CREATING A NEW VM from the recovery point. It does NOT revert ${vmName} in place — the running VM is left unchanged, and you will need to clean up the new VM afterward.`;
}

/** The per-row meta line under a recovery point's name: creation time, then its
 *  recovery_point_type, then an expiry note when set. Omits absent fields and
 *  returns '' when nothing is known. Pure for testing. */
// eslint-disable-next-line react-refresh/only-export-components -- intentional: testable helper co-located for unit tests (FE-01)
export function snapshotMetaLine(snap: SnapshotInfo): string {
  const parts: string[] = [];
  if (snap.created_at) parts.push(snap.created_at);
  if (snap.snapshot_type) parts.push(snap.snapshot_type);
  if (snap.expires_at) parts.push(`expires ${snap.expires_at}`);
  return parts.join(' · ');
}

const C = {
  panelBg: 'var(--hc-bg-0, #0c1622)',
  border: 'var(--hc-bg-3, #29405a)',
  fg: 'var(--hc-fg-0, #e8eef6)',
  muted: 'var(--hc-fg-2, #7d93ab)',
  accent: 'var(--hc-accent-500, #4ea3e0)',
  danger: 'var(--hc-error, #e05656)',
  rowBg: 'rgba(255,255,255,0.04)',
} as const;

const FONT = 'var(--hc-font-sans)';

export function SnapshotsPanel({
  sdkClient,
  pluginId,
  resourceId,
  vmName,
  toast,
  gateCanWrite,
  onClose,
}: SnapshotsPanelProps) {
  const [snapshots, setSnapshots] = useState<SnapshotInfo[]>([]);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [newName, setNewName] = useState('');
  // The trimmed name pending a CREATE confirmation. Non-null === the create
  // confirm modal is up. Setting it NEVER creates — only the modal's confirm
  // button does (so clicking Create can never silently take a snapshot).
  const [pendingCreate, setPendingCreate] = useState<string | null>(null);
  // The snapshot pending a restore confirmation. Non-null === the modal is up.
  // Setting it NEVER restores — only the modal's danger button does.
  const [restoreTarget, setRestoreTarget] = useState<SnapshotInfo | null>(null);

  const reload = useCallback(async () => {
    setLoading(true);
    try {
      const res = await sdkClient.listSnapshots(pluginId, resourceId);
      setSnapshots(res.snapshots);
    } catch (err) {
      toast(`Couldn't load snapshots: ${(err as Error).message}`, 'err');
    } finally {
      setLoading(false);
    }
  }, [sdkClient, pluginId, resourceId, toast]);

  useEffect(() => {
    void reload();
  }, [reload]);

  // The ONLY caller of createSnapshot — wired to the create confirm modal's
  // confirm button. The Create button itself only opens the modal.
  const confirmCreate = useCallback(async () => {
    if (!pendingCreate) return;
    const name = pendingCreate;
    setPendingCreate(null);
    setBusy(true);
    toast('Creating snapshot…', 'info');
    try {
      await sdkClient.createSnapshot(pluginId, resourceId, name);
      setNewName('');
      toast('Snapshot created', 'ok');
      await reload();
    } catch (err) {
      toast(`Create failed: ${(err as Error).message}`, 'err');
    } finally {
      setBusy(false);
    }
  }, [pendingCreate, sdkClient, pluginId, resourceId, toast, reload]);

  // The ONLY caller of restoreSnapshot — wired to the modal's danger button.
  const confirmRestore = useCallback(async () => {
    if (!restoreTarget) return;
    const target = restoreTarget;
    setRestoreTarget(null);
    setBusy(true);
    toast('Restoring snapshot…', 'info');
    try {
      await sdkClient.restoreSnapshot(pluginId, resourceId, target.uuid);
      toast('Restore complete — a new VM was created from the snapshot', 'ok');
      await reload();
    } catch (err) {
      toast(`Restore failed: ${(err as Error).message}`, 'err');
    } finally {
      setBusy(false);
    }
  }, [restoreTarget, sdkClient, pluginId, resourceId, toast, reload]);

  const writeDisabled = snapshotControlsDisabled(gateCanWrite, busy);

  return (
    <div
      style={{
        minWidth: 320,
        maxWidth: 380,
        padding: '14px 16px',
        borderRadius: 12,
        background: C.panelBg,
        border: `1px solid ${C.border}`,
        boxShadow: '0 16px 48px rgba(0,0,0,0.55)',
        fontFamily: FONT,
        fontSize: 12,
        color: C.fg,
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', marginBottom: 10 }}>
        <span
          style={{
            flex: 1,
            fontSize: 11,
            letterSpacing: '0.06em',
            textTransform: 'uppercase',
            color: C.muted,
          }}
        >
          Snapshots
        </span>
        <button
          type="button"
          aria-label="Close snapshots"
          onClick={onClose}
          style={{
            border: 'none',
            background: 'transparent',
            color: C.muted,
            cursor: 'pointer',
            padding: 2,
            lineHeight: 0,
          }}
        >
          <Icon name="close" size={14} />
        </button>
      </div>

      {/* Create */}
      <div style={{ display: 'flex', gap: 6, marginBottom: 12 }}>
        <input
          type="text"
          value={newName}
          onChange={(e) => setNewName(e.target.value)}
          placeholder="New snapshot name"
          disabled={writeDisabled}
          style={{
            flex: 1,
            padding: '6px 8px',
            borderRadius: 6,
            border: `1px solid ${C.border}`,
            background: 'rgba(0,0,0,0.25)',
            color: C.fg,
            fontSize: 12,
            fontFamily: FONT,
          }}
        />
        <button
          type="button"
          // Opens the create confirm modal — does NOT create. Only the modal's
          // confirm button calls createSnapshot.
          onClick={() => {
            const name = newName.trim();
            if (name) setPendingCreate(name);
          }}
          disabled={writeDisabled || newName.trim() === ''}
          style={{
            padding: '6px 12px',
            borderRadius: 6,
            border: `1px solid ${C.accent}`,
            background: 'transparent',
            color: C.accent,
            cursor: writeDisabled || newName.trim() === '' ? 'default' : 'pointer',
            opacity: writeDisabled || newName.trim() === '' ? 0.5 : 1,
            fontSize: 12,
            whiteSpace: 'nowrap',
          }}
        >
          Create
        </button>
      </div>

      {/* List */}
      {loading && snapshots.length === 0 ? (
        <div style={{ color: C.muted }}>Loading…</div>
      ) : snapshots.length === 0 ? (
        <div style={{ color: C.muted }}>No snapshots yet.</div>
      ) : (
        <div
          style={{
            display: 'flex',
            flexDirection: 'column',
            gap: 6,
            maxHeight: 260,
            overflowY: 'auto',
          }}
        >
          {snapshots.map((s) => (
            <div
              key={s.uuid}
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 8,
                padding: '6px 8px',
                borderRadius: 6,
                background: C.rowBg,
              }}
            >
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {s.name}
                </div>
                {snapshotMetaLine(s) && (
                  <div style={{ color: C.muted, fontSize: 11 }}>{snapshotMetaLine(s)}</div>
                )}
              </div>
              <button
                type="button"
                // Opens the mandatory confirm modal — does NOT restore.
                onClick={() => setRestoreTarget(s)}
                disabled={writeDisabled}
                style={{
                  flexShrink: 0,
                  padding: '4px 10px',
                  borderRadius: 6,
                  border: `1px solid ${C.danger}`,
                  background: 'transparent',
                  color: C.danger,
                  cursor: writeDisabled ? 'default' : 'pointer',
                  opacity: writeDisabled ? 0.5 : 1,
                  fontSize: 12,
                }}
              >
                Restore
              </button>
            </div>
          ))}
        </div>
      )}

      {/* CREATE confirmation modal. A true overlay (not a toast). Snapshot
          creation is non-destructive, so this is informational (accent, not
          danger) — but it guarantees a snapshot is never taken without an
          explicit second click. Only its confirm button calls createSnapshot. */}
      {pendingCreate && (
        <Overlay
          label="Create snapshot?"
          onClose={() => setPendingCreate(null)}
          zIndex={140}
          backdropBlur={false}
        >
          <div
            style={{
              width: 420,
              maxWidth: '100%',
              padding: '18px 20px',
              borderRadius: 12,
              background: C.panelBg,
              border: `1px solid ${C.accent}`,
              boxShadow: '0 16px 48px rgba(0,0,0,0.55)',
              color: C.fg,
              fontFamily: FONT,
            }}
          >
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 10 }}>
              <span style={{ color: C.accent, lineHeight: 0 }}>
                <Icon name="layers" size={18} />
              </span>
              <span style={{ fontSize: 15, fontWeight: 600 }}>Create snapshot?</span>
            </div>
            <p style={{ margin: '0 0 16px', fontSize: 13, lineHeight: 1.5, color: C.fg }}>
              {createConfirmCopy(vmName, pendingCreate)}
            </p>
            <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
              <button
                type="button"
                onClick={() => setPendingCreate(null)}
                style={{
                  padding: '8px 16px',
                  borderRadius: 8,
                  border: `1px solid ${C.border}`,
                  background: 'transparent',
                  color: C.fg,
                  cursor: 'pointer',
                  fontSize: 13,
                }}
              >
                Cancel
              </button>
              {/* The ONLY trigger of createSnapshot. */}
              <button
                type="button"
                onClick={() => void confirmCreate()}
                disabled={!gateCanWrite}
                style={{
                  padding: '8px 16px',
                  borderRadius: 8,
                  border: 'none',
                  background: C.accent,
                  color: '#fff',
                  cursor: gateCanWrite ? 'pointer' : 'default',
                  opacity: gateCanWrite ? 1 : 0.5,
                  fontSize: 13,
                  fontWeight: 600,
                }}
              >
                Create snapshot
              </button>
            </div>
          </div>
        </Overlay>
      )}

      {/* MANDATORY restore confirmation modal — a centered fade-in overlay.
          Only its danger button calls restoreSnapshot; Cancel is primary. */}
      {restoreTarget && (
        <Overlay
          label="Restore snapshot to a new VM?"
          onClose={() => setRestoreTarget(null)}
          zIndex={140}
          backdropBlur={false}
        >
          <div
            style={{
              width: 420,
              maxWidth: '100%',
              padding: '18px 20px',
              borderRadius: 12,
              background: C.panelBg,
              border: `1px solid ${C.danger}`,
              boxShadow: '0 16px 48px rgba(0,0,0,0.55)',
              color: C.fg,
              fontFamily: FONT,
            }}
          >
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 10 }}>
              <span style={{ color: C.danger, lineHeight: 0 }}>
                <Icon name="info" size={18} />
              </span>
              <span style={{ fontSize: 15, fontWeight: 600 }}>Restore snapshot to a new VM?</span>
            </div>
            <p style={{ margin: '0 0 16px', fontSize: 13, lineHeight: 1.5, color: C.fg }}>
              {restoreConfirmCopy(vmName, restoreTarget)}
            </p>
            <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
              {/* Cancel is the VISUALLY PRIMARY / default action. */}
              <button
                type="button"
                onClick={() => setRestoreTarget(null)}
                style={{
                  padding: '8px 16px',
                  borderRadius: 8,
                  border: 'none',
                  background: C.accent,
                  color: '#fff',
                  cursor: 'pointer',
                  fontSize: 13,
                  fontWeight: 600,
                }}
              >
                Cancel
              </button>
              {/* The ONLY trigger of the destructive restore. */}
              <button
                type="button"
                onClick={() => void confirmRestore()}
                disabled={!gateCanWrite}
                style={{
                  padding: '8px 16px',
                  borderRadius: 8,
                  border: `1px solid ${C.danger}`,
                  background: 'transparent',
                  color: C.danger,
                  cursor: gateCanWrite ? 'pointer' : 'default',
                  opacity: gateCanWrite ? 1 : 0.5,
                  fontSize: 13,
                }}
              >
                Restore — create a new VM
              </button>
            </div>
          </div>
        </Overlay>
      )}
    </div>
  );
}
