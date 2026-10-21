import { useEffect, useMemo, useRef, useState } from 'react';
import {
  ToastProvider,
  useToast,
  CommandPalette,
  type CommandItem,
  useKeyboardShortcut,
} from '@ahv-remote-console/ui';
import { AHVRemoteConsoleClient } from '@ahv-remote-console/sdk';
import { ClientContext } from './ClientContext';
import { Shell } from './Shell';

interface Bootstrap {
  client: AHVRemoteConsoleClient;
  endpoint: string;
}

// Derived from the preload contract so we don't couple the renderer to
// the preload module. Mirrors RuntimeStateEvent in preload/preload.ts.
type RuntimeStateSnapshot = Awaited<ReturnType<typeof window.ahvRemoteConsole.getRuntimeState>>;

export function App() {
  const [boot, setBoot] = useState<Bootstrap | null>(null);
  // Drive the UI off the runtime supervisor's state machine instead of a
  // single one-shot handshake read. The earlier code read the handshake
  // exactly once on mount and, if the runtime was not YET ready (the normal
  // cold-start race — the renderer mounts before the Go runtime finishes
  // its first-exec assessment + plugin load), threw "Runtime handshake
  // unavailable" and rendered a terminal error screen with no recovery.
  // Main already broadcasts starting → ready → error over `runtime-state`;
  // we now consume it.
  const [phase, setPhase] = useState<'starting' | 'ready' | 'error'>('starting');
  const [runtimeError, setRuntimeError] = useState<string | null>(null);
  const [retrying, setRetrying] = useState(false);
  // Endpoint the SDK client was last built for; guards against rebuilding the
  // client on every ready event / StrictMode double-invoke.
  const lastEndpointRef = useRef<string | null>(null);

  useEffect(() => {
    let cancelled = false;

    const apply = (s: RuntimeStateSnapshot) => {
      if (cancelled) return;
      if (s.state === 'ready' && s.handshake) {
        const endpoint = s.handshake.endpoint;
        setPhase('ready');
        setRuntimeError(null);
        // Build the SDK client once per endpoint, OUTSIDE the setState updater
        // (which must stay pure — React may invoke it twice under StrictMode).
        // The runtime binds a fresh loopback port on every (re)start, so if a
        // restart hands us a new endpoint we rebuild rather than keep talking
        // to a dead port. The renderer never holds the IPC secret — main signs.
        if (lastEndpointRef.current !== endpoint) {
          lastEndpointRef.current = endpoint;
          const client = new AHVRemoteConsoleClient({
            endpoint,
            signer: {
              signRequest: (method, uri, body) =>
                window.ahvRemoteConsole.signRequest(method, uri, body),
              signWebSocket: (uri) => window.ahvRemoteConsole.signWebSocket(uri),
            },
          });
          setBoot({ client, endpoint });
        }
      } else if (s.state === 'error') {
        setPhase('error');
        setRuntimeError(s.error ?? 'The local runtime failed to start.');
      } else {
        setPhase('starting');
      }
    };

    // Subscribe BEFORE the snapshot read so we cannot miss a transition that
    // fires in the gap between the two calls.
    const off = window.ahvRemoteConsole.onRuntimeState(apply);
    void window.ahvRemoteConsole
      .getRuntimeState()
      .then(apply)
      .catch(() => {
        /* main not reachable yet; the push subscription will deliver state */
      });

    return () => {
      cancelled = true;
      off();
    };
  }, []);

  const handleRetry = async () => {
    setRetrying(true);
    setRuntimeError(null);
    setPhase('starting');
    try {
      await window.ahvRemoteConsole.restartRuntime();
    } catch (err) {
      setPhase('error');
      setRuntimeError((err as Error).message);
    } finally {
      setRetrying(false);
    }
  };

  // Fatal screen only when we have NEVER reached ready. If we booted and a
  // transient error arrives later, keep the live session up.
  if (phase === 'error' && !boot) {
    return (
      <div
        style={{
          height: '100%',
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          justifyContent: 'center',
          gap: 12,
          color: 'var(--hc-fg-1)',
          textAlign: 'center',
          padding: 24,
        }}
      >
        <div style={{ fontSize: 18, color: 'var(--hc-error)', fontWeight: 600 }}>
          AHV Remote Console cannot reach its local runtime.
        </div>
        <div style={{ fontSize: 13, maxWidth: 480 }}>
          {runtimeError ?? 'The local runtime failed to start.'}
        </div>
        <button
          type="button"
          onClick={() => {
            void handleRetry();
          }}
          disabled={retrying}
          style={{
            marginTop: 4,
            padding: '8px 18px',
            fontSize: 13,
            fontWeight: 600,
            color: '#fff',
            background: 'var(--hc-accent-500)',
            border: 'none',
            borderRadius: 8,
            cursor: retrying ? 'default' : 'pointer',
            opacity: retrying ? 0.6 : 1,
          }}
        >
          {retrying ? 'Restarting…' : 'Retry'}
        </button>
        <div style={{ fontSize: 12, color: 'var(--hc-fg-3)', maxWidth: 480 }}>
          If the problem persists, export a diagnostics bundle from the tray icon (right-click →
          Export Diagnostics) and contact your administrator.
        </div>
      </div>
    );
  }

  if (!boot) {
    return (
      <div
        style={{
          height: '100%',
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          justifyContent: 'center',
          gap: 10,
          color: 'var(--hc-fg-2)',
        }}
      >
        <div aria-hidden style={{ fontSize: 22, animation: 'hc-spin 1s linear infinite' }}>
          ◠
        </div>
        <div style={{ fontSize: 13 }}>Starting runtime…</div>
        <style>{`@keyframes hc-spin { to { transform: rotate(360deg); } }`}</style>
      </div>
    );
  }

  return (
    <ClientContext.Provider value={boot.client}>
      {/* Main window (lobby / cluster screens): bottom-right is free here. */}
      <ToastProvider placement="bottom-right">
        <Root />
      </ToastProvider>
    </ClientContext.Provider>
  );
}

function Root() {
  const [paletteOpen, setPaletteOpen] = useState(false);
  const toast = useToast();

  useKeyboardShortcut({
    combo: 'mod+k',
    handler: () => setPaletteOpen((v) => !v),
  });

  // (auto-update removed — see electron-builder.yml; the onUpdateAvailable /
  // onUpdateDownloaded channels no longer fire, so the former toast
  // subscriptions were dead code and have been dropped.)

  const commands = useMemo<CommandItem[]>(
    () => [
      {
        id: 'export-diagnostics',
        label: 'Export diagnostics bundle',
        keywords: ['logs', 'debug', 'support'],
        onRun: () => {
          void (async () => {
            const path = await window.ahvRemoteConsole.saveDiagnostics();
            if (path) toast.push('success', 'Diagnostics exported', path);
          })();
        },
      },
      {
        id: 'reload',
        label: 'Reload UI',
        keywords: ['refresh'],
        onRun: () => window.location.reload(),
      },
    ],
    [toast],
  );

  return (
    <>
      <Shell onOpenPalette={() => setPaletteOpen(true)} />
      <CommandPalette open={paletteOpen} onClose={() => setPaletteOpen(false)} items={commands} />
    </>
  );
}
