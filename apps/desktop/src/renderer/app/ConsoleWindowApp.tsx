/**
 * ConsoleWindowApp — renderer entry mounted in pop-out windows. The main
 * process opens these windows with `?mode=console&session=<id>&vm=<name>`;
 * we boot the same runtime client the main window uses (handshake + IPC
 * secret), open a WebSocket for the session, and render a single
 * ConsoleView fullscreen-friendly chrome.
 *
 * Each pop-out is independent — its own pointer lock, its own keyboard
 * capture, its own RFB client. The runtime tolerates multiple consecutive
 * WS opens for the same session because OpenStream re-dials the upstream
 * on every call.
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import { ToastProvider, useToast } from '@ahv-remote-console/ui';
import { AHVRemoteConsoleClient } from '@ahv-remote-console/sdk';
import { ClientContext } from './ClientContext';
import { ConsoleView, type ConsoleStatus } from '../components/ConsoleView';
import { isRetryableCause } from '../components/console/status';
import { OverlayButton } from '../components/console/ConsoleOverlays';
import { reopenSessionStream } from './reopenStream';

export interface ConsoleWindowAppProps {
  sessionId: string;
  vmName: string;
}

// Mirrors RuntimeStateEvent in preload/preload.ts without coupling to it.
type RuntimeStateSnapshot = Awaited<ReturnType<typeof window.ahvRemoteConsole.getRuntimeState>>;

// Bounded self-heal backoff (ms): 1s, 2s, 4s, then 8s ×5, then give up. Module
// scope so the close-handler closes over a stable reference (no effect/callback
// dependency churn).
const reconnectBackoff = [1000, 2000, 4000, 8000, 8000, 8000, 8000, 8000];

// Defense-in-depth cap on CONSECUTIVE auto-reauth cycles (no 'connected' frame
// between them). The real loop fix is server-side (the re-auth signal is now
// re-armable), but this backstops a DIFFERENT future cause from spinning the SSO
// popup forever: after this many auto-attempts we stop auto-opening SSO and leave
// the manual "Sign in" overlay, which still works. Reset on a real 'connected'.
const maxAutoReauth = 3;

export function ConsoleWindowApp(props: ConsoleWindowAppProps) {
  return (
    // The console's bottom edge is occupied by its status rail + the guest
    // taskbar, so notifications dock top-center where the viewport is clear.
    <ToastProvider placement="top-center">
      <Inner {...props} />
    </ToastProvider>
  );
}

function Inner({ sessionId, vmName }: ConsoleWindowAppProps) {
  const [ws, setWs] = useState<WebSocket | null>(null);
  // The AHVRemoteConsoleClient must be provided to ConsoleView via the
  // ClientContext — Console-internal hooks (useAgent in
  // particular) read it through useClient() and previously crashed
  // the pop-out window with "ClientContext not initialized" because
  // we created the client inside the effect but never put it in a
  // Provider. Hoisted to state so the Provider sees it.
  const [client, setClient] = useState<AHVRemoteConsoleClient | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [status, setStatus] = useState<ConsoleStatus>({ kind: 'connecting' });
  // The pop-out is bootstrapped with only sessionId + vmName (see main.tsx), so
  // it lacks the VM UUID that power/serial actions need as resource_id.
  const [vmUuid, setVmUuid] = useState<string | undefined>(undefined);
  // The session id can CHANGE during this window's life: if the runtime restarts
  // or idle-reaps the original session, reopenStream() re-mints a fresh one (see
  // below) and updates this. Every session read-site uses the current id (via
  // currentSessionIdRef) rather than the original `sessionId` prop, so power /
  // serial / clipboard / close keep targeting the session that's actually live.
  const [currentSessionId, setCurrentSessionId] = useState(sessionId);
  const toast = useToast();

  // Resolve the VM UUID (resource_id) from the runtime once the client is up:
  // the session was opened with it. Without this, ConsoleView sends an empty
  // resource_id and power/serial actions fail with "resource_id required".
  // While we're here, capture the VM identity (plugin + resource) so reopenStream
  // can re-mint the session if the runtime later drops it.
  useEffect(() => {
    if (!client) return;
    let cancelled = false;
    void client
      .listSessions()
      .then(({ sessions }) => {
        if (cancelled) return;
        const match = sessions.find((s) => s.id === currentSessionId);
        if (match?.resource_id) setVmUuid(match.resource_id);
        if (match) {
          sessionIdentityRef.current = {
            pluginId: match.plugin_id,
            resourceId: match.resource_id,
          };
        }
      })
      .catch(() => {
        /* non-fatal — the action itself will surface any error */
      });
    return () => {
      cancelled = true;
    };
  }, [client, currentSessionId]);

  // ── Per-window self-heal ─────────────────────────────────────────────────
  // The renderer↔runtime WebSocket can drop while idle (the proxy's pong
  // deadline lapses if this window was throttled/asleep, or the runtime
  // hiccups). When that happens the proxy can no longer send ANY control
  // frame, so the console would hang. We reopen the stream (backoff) and swap
  // `ws` — ConsoleView re-attaches on the prop change. This mirrors the
  // self-heal that used to live in Shell's armReconnect, scoped to this single
  // session.
  //
  // deliberateCloseRef suppresses reconnect when WE close the socket (cleanup,
  // window teardown); reconnectingRef de-dupes concurrent loops. We auto-
  // reconnect on every non-deliberate, non-auth-fatal close THAT FOLLOWED a
  // successful connect, and keep retrying until the VM/runtime is reachable
  // again, so an established console is never left frozen. An initial open that
  // closes before ever connecting (e.g. a deleted VM / unreachable cluster
  // closing the upstream) is a hard failure retrying can't fix — connectedRef
  // gates that path so it fails fast to the error overlay instead of ~40s of
  // reconnect flicker.
  const clientRef = useRef<AHVRemoteConsoleClient | null>(null);
  const deliberateCloseRef = useRef(false);
  const reconnectingRef = useRef(false);
  const connectedRef = useRef(false);
  // Loopback endpoint the SDK client was last built for. The runtime binds a
  // FRESH port on every (re)start, so a restart hands us a new endpoint and we
  // must rebuild the client — otherwise the pop-out keeps dialing the dead old
  // port and every reconnect fails with a "WebSocket error". Mirrors App.tsx.
  const lastEndpointRef = useRef<string | null>(null);
  // Persistent self-heal backoff cursor. CRITICAL: it advances across SEPARATE
  // onClose firings and resets ONLY on a confirmed 'connected' (status effect
  // below). Previously each drop ran a fresh internal loop that always "succeeded"
  // on slot 1 — because openStream resolves before the socket connects — so the
  // backoff never accumulated and the give-up branch was dead code, re-dialing the
  // loopback every ~1s forever behind a dead overlay. Now each drop takes ONE
  // step; after the schedule is exhausted we stop and hand off to ConsoleView's
  // watchdog/error overlay.
  const reconnectAttemptsRef = useRef(0);
  // Set true when ConsoleView declares the session terminally dead (status
  // 'error' — its 90s give-up deadline) or a fatal cause arrives; the self-heal
  // close-handler checks it so we stop re-dialing instead of looping forever.
  const stoppedRef = useRef(false);
  // Counts CONSECUTIVE auto-reauth cycles (SH1) with no intervening 'connected'.
  // Independent of reconnectAttemptsRef (which reconnectNow zeroes), so the cap
  // can't be masked by the reconnect that each reauth triggers. Reset on a real
  // 'connected'.
  const reauthAttemptsRef = useRef(0);
  // Current (possibly re-minted) session id, read by the reopen / cleanup /
  // reauth paths so they always target the session that's actually live.
  const currentSessionIdRef = useRef(sessionId);
  // VM identity (plugin + resource) captured from listSessions while the session
  // is ALIVE. It's what lets reopenStream re-mint after the runtime drops the
  // original session — without it we could only stream an id, never recreate one.
  const sessionIdentityRef = useRef<{ pluginId: string; resourceId: string } | null>(null);

  // Mirror of the live socket. On a swap the PRIOR socket is TAGGED so its own
  // onClose handler does not trigger a second reconnect.
  //
  // CRITICAL: we must NOT close the prior socket here. On an upstream drop the
  // proxy keeps this SAME loopback OPEN and re-handshakes in place (it sends
  // rfb_reset; ConsoleView recreates the RFBClient). Closing it from the renderer
  // races that recovery: the proxy's handshake write fails ("websocket: close
  // sent"), it marks the client gone and exits the run loop with NO rfb_reset, so
  // ConsoleView never recreates its client — the console freezes (the RFB client's
  // 50ms request timer then spams a dead socket). The proxy is the sole owner of
  // upstream-drop recovery; the renderer only opens a fresh stream when the
  // loopback has ACTUALLY closed (armReconnect.onClose). A genuinely stranded
  // socket (manual reconnect / runtime restart) is reaped server-side (~2min
  // idle) — a brief linger is the safe trade against freezing a live console.
  const wsRef = useRef<WebSocket | null>(null);
  const swapWs = useCallback((next: WebSocket | null) => {
    const prev = wsRef.current;
    if (prev && prev !== next) {
      (prev as WebSocket & { __swappedOut?: boolean }).__swappedOut = true;
    }
    wsRef.current = next;
    setWs(next);
  }, []);

  // reopenStream is the single chokepoint for (re)opening this window's stream,
  // and the fix for the failure that used to strand the console forever: the
  // runtime LOST the session (it restarted and dropped its session table, or
  // reaped it as idle), so streaming the old id can never connect — the operator
  // is stuck on "Connecting…" or a Reconnect button that does nothing. The
  // re-mint decision lives in reopenSessionStream (unit-tested); here we just
  // feed it the live id + captured identity and adopt any new id it mints. Boot,
  // self-heal, and manual Reconnect all go through here, so every recovery path
  // re-mints when it needs to.
  const reopenStream = useCallback(
    (c: AHVRemoteConsoleClient): Promise<WebSocket> =>
      reopenSessionStream(c, currentSessionIdRef.current, sessionIdentityRef.current, (id) => {
        currentSessionIdRef.current = id;
        setCurrentSessionId(id);
      }),
    [],
  );

  const armReconnect = useCallback(
    (socket: WebSocket) => {
      const onClose = (e: CloseEvent) => {
        socket.removeEventListener('close', onClose);
        if ((socket as WebSocket & { __swappedOut?: boolean }).__swappedOut) return;
        if (deliberateCloseRef.current) return;
        if (stoppedRef.current) return; // ConsoleView gave up — stop re-dialing
        if (reconnectingRef.current) return;
        // Terminal fatal close codes: 1008 (ClosePolicyViolation = session_expired
        // / rbac_denied / vm_not_found / config_error / session_closed) and 1009
        // (frame_too_large). Retrying cannot succeed — the typed error overlay
        // drives recovery. The fatal control frame also latches stoppedRef via the
        // status effect; the code check guards the race where close beats the frame.
        if (e.code === 1008 || e.code === 1009) return;
        // Only self-heal a connection that actually reached 'connected'. A socket
        // that closed before connecting (hard upstream failure: deleted VM, wrong
        // endpoint, unreachable cluster) won't recover by retrying — fail fast so
        // the error overlay appears at once.
        if (!connectedRef.current) return;
        const c = clientRef.current;
        if (!c) return;
        const attempt = reconnectAttemptsRef.current;
        if (attempt >= reconnectBackoff.length) {
          // Bounded self-heal EXHAUSTED — stop. The last socket is already closed,
          // so ConsoleView is in 'reconnecting' and its frame-gated watchdog will
          // surface the actionable error overlay (manual Retry). The cursor resets
          // only on a real 'connected', so a once-connected-but-permanently-broken
          // session ends here instead of re-dialing forever.
          return;
        }
        reconnectingRef.current = true;
        reconnectAttemptsRef.current = attempt + 1;
        // ±15% jitter so N windows dropped by one runtime restart don't retry in
        // lockstep (thundering herd against the runtime).
        const slot = reconnectBackoff[attempt] ?? reconnectBackoff[reconnectBackoff.length - 1]!;
        const delay = slot * (0.85 + Math.random() * 0.3);
        void (async () => {
          try {
            await new Promise((r) => setTimeout(r, delay));
            if (deliberateCloseRef.current || stoppedRef.current) return;
            const next = await reopenStream(c);
            if (deliberateCloseRef.current || stoppedRef.current) {
              next.close();
              return;
            }
            armReconnectRef.current(next); // re-arm the fresh ws
            swapWs(next); // swapped while CONNECTING — ConsoleView attaches before any byte
          } catch {
            /* reopenStream threw (session gone + no identity, or signing) — the
               watchdog surfaces the error overlay */
          } finally {
            reconnectingRef.current = false;
          }
        })();
      };
      socket.addEventListener('close', onClose);
    },
    [reopenStream, swapWs],
  );
  const armReconnectRef = useRef(armReconnect);
  armReconnectRef.current = armReconnect;

  // Guards a manual reconnect/reauth in flight so a double-click (or an overlay
  // click overlapping the still-armed self-heal) can't open redundant sockets +
  // spawn duplicate runtime proxy goroutines / upstream dials.
  const manualBusyRef = useRef(false);

  // Manual reconnect for the error overlay's "Reconnect" action (unreachable):
  // open a fresh stream immediately and swap it in (re-arming self-heal),
  // bypassing the automatic backoff. Clears the give-up/backoff state so the
  // operator's explicit retry gets a fresh budget.
  const reconnectNow = useCallback(() => {
    const c = clientRef.current;
    if (!c) return;
    if (manualBusyRef.current) return; // dedup double-clicks
    manualBusyRef.current = true;
    stoppedRef.current = false;
    reconnectAttemptsRef.current = 0;
    void (async () => {
      try {
        const next = await reopenStream(c);
        if (deliberateCloseRef.current) {
          next.close();
          return;
        }
        armReconnectRef.current(next);
        // swapWs only TAGS the prior socket (__swappedOut); it deliberately does
        // NOT close it (closing races the proxy's in-place re-handshake — see
        // swapWs). Here the prior socket is the dead one that dropped us into the
        // error overlay (already closing/closed), and the runtime reaps an idle
        // session ~2min, so it cannot strand.
        swapWs(next);
      } catch {
        /* still down — the error overlay stays so the operator can retry */
      } finally {
        manualBusyRef.current = false;
      }
    })();
  }, [reopenStream, swapWs]);

  // In-console re-authentication for the error overlay's "Re-authenticate"
  // action (session_expired): re-run Prism's embedded SSO window to recapture
  // the session, hand the fresh cookies to the plugin, then reconnect in place
  // — the operator never leaves the console or returns to the lobby. The
  // endpoint is resolved entirely client-side from the session's resource_id
  // (format "<endpointId>/<vmUUID>"), so the pop-out needs no extra context.
  const reauthNow = useCallback(() => {
    const c = clientRef.current;
    if (!c) return;
    if (manualBusyRef.current) return; // dedup: SSO flow keeps the overlay clickable
    manualBusyRef.current = true;
    void (async () => {
      try {
        const { sessions } = await c.listSessions();
        const sess = sessions.find((s) => s.id === currentSessionIdRef.current);
        if (!sess) throw new Error('session not found');
        const endpointId = sess.resource_id.split('/')[0];
        const { endpoints } = await c.listEndpoints();
        const ep = endpoints.find((e) => e.id === endpointId);
        if (!ep) throw new Error('endpoint not found');
        const result = await window.ahvRemoteConsole.openSSOLogin({
          endpointId: ep.id,
          prismURL: ep.url,
          allowInsecureTLS: ep.allow_insecure_tls,
          pinnedCertB64: ep.pinned_cert_b64,
        });
        if (!result.cookies || result.cookies.length === 0) {
          throw new Error('No session cookies captured from Prism.');
        }
        const res = (await c.startAuth(sess.plugin_id, {
          endpoint_id: ep.id,
          method: 'cookie',
          cookies: result.cookies,
        })) as { authenticated?: boolean };
        if (!res.authenticated) throw new Error('Prism rejected the captured session.');
        manualBusyRef.current = false; // release so reconnectNow can acquire
        reconnectNow();
      } catch {
        manualBusyRef.current = false;
        toast.push('error', 'Sign-in didn’t work', 'We couldn’t sign you in. Please try again.');
      }
    })();
  }, [reconnectNow, toast]);

  // Bootstrap: wait for the runtime to be ready (it usually already is,
  // since pop-outs open after the main window booted), then open a fresh
  // WebSocket for this session. We drive off the runtime-state machine
  // rather than a single getRuntimeHandshake() read so a pop-out spawned
  // during a runtime restart shows progress instead of a hard failure.
  useEffect(() => {
    let cancelled = false;
    let openedWs: WebSocket | null = null;
    let off: (() => void) | null = null;

    const boot = async (handshake: NonNullable<RuntimeStateSnapshot['handshake']>) => {
      // Re-arm self-heal for this fresh socket. deliberateCloseRef is a teardown
      // latch (set on unmount / window close); it must be scoped to a live
      // socket, not monotonic. Without this reset, React StrictMode's dev
      // mount→cleanup→mount leaves it latched true on the surviving mount —
      // permanently disabling BOTH self-heal and manual reconnect — and a
      // runtime-restart rebuild (new loopback port) would inherit a stale flag.
      deliberateCloseRef.current = false;
      try {
        // Secret never crosses the contextBridge — main signs on our behalf
        // and has already verified handshake.secret_hash on startup.
        const c = new AHVRemoteConsoleClient({
          endpoint: handshake.endpoint,
          signer: {
            signRequest: (method, uri, body) =>
              window.ahvRemoteConsole.signRequest(method, uri, body),
            signWebSocket: (uri) => window.ahvRemoteConsole.signWebSocket(uri),
          },
        });
        clientRef.current = c;
        openedWs = await reopenStream(c);
        if (cancelled) {
          openedWs.close();
          return;
        }
        armReconnect(openedWs);
        setClient(c);
        swapWs(openedWs);
      } catch (err) {
        if (!cancelled) setError((err as Error).message);
      }
    };

    const apply = (s: RuntimeStateSnapshot) => {
      if (cancelled) return;
      if (s.state === 'ready' && s.handshake) {
        // Rebuild the client + reopen the stream whenever the runtime endpoint
        // CHANGES — first boot, or a runtime restart (crash / sleep-wake) that
        // bound a new loopback port. We deliberately stay subscribed for the
        // pop-out's whole lifetime (the listener is released only on unmount,
        // below): previously we detached after the first boot, so a restart
        // left this window dialing the dead old port and reconnect failed with
        // "WebSocket error" forever. The endpoint guard makes repeat 'ready'
        // events (and the StrictMode double-invoke) idempotent.
        if (lastEndpointRef.current !== s.handshake.endpoint) {
          lastEndpointRef.current = s.handshake.endpoint;
          setError(null); // clear any earlier transient-error message
          void boot(s.handshake);
        }
      } else if (s.state === 'error') {
        setError(s.error ?? 'The local runtime is unavailable.');
      }
    };

    off = window.ahvRemoteConsole.onRuntimeState(apply);
    void window.ahvRemoteConsole
      .getRuntimeState()
      .then(apply)
      .catch(() => {
        /* push subscription will deliver state */
      });

    return () => {
      cancelled = true;
      off?.();
      // Mark deliberate FIRST so the self-heal close-handler on openedWs (and
      // any later ws we swapped in) doesn't re-dial a stream we're tearing down.
      deliberateCloseRef.current = true;
      // Tear down the live socket AND the originally-opened one (they differ
      // after a self-heal swap). dispose() stops the RFB client's 50ms request
      // timer, visibilitychange listener, and transport handlers immediately so
      // they neither leak across open/close cycles nor fire a phantom into a
      // still-live tree on a quick reopen; then close the socket. The Set
      // dedupes when no swap happened.
      for (const s of new Set([openedWs, wsRef.current])) {
        if (!s) continue;
        (s as WebSocket & { __hcRfbClient?: { dispose: () => void } }).__hcRfbClient?.dispose();
        s.close();
      }
    };
  }, [armReconnect, swapWs, reopenStream]);

  // ── Session cleanup ──────────────────────────────────────────────────────
  // When this window goes away we must close the runtime session so it doesn't
  // leak. We do this on React unmount AND on the window's 'beforeunload' (the
  // belt-and-suspenders path for a real OS window close, which may unload
  // before React effect cleanup runs). Mark deliberate-close first so the
  // self-heal loop above never tries to re-dial during teardown. Best-effort:
  // the runtime also reaps idle sessions, so a missed close is not fatal.
  useEffect(() => {
    const closeNow = () => {
      deliberateCloseRef.current = true;
      const c = clientRef.current;
      if (c) {
        // Close the CURRENT session id — after a reopenStream re-mint this is
        // the live one, not the original prop that's already gone.
        void c.closeSession(currentSessionIdRef.current).catch(() => {
          /* best-effort — runtime reaps idle sessions anyway */
        });
      }
    };
    window.addEventListener('beforeunload', closeNow);
    return () => {
      window.removeEventListener('beforeunload', closeNow);
      closeNow();
    };
  }, []);

  // Quietly surface non-fatal status changes so the operator has a hint when
  // the proxy reconnects.
  useEffect(() => {
    if (status.kind === 'connected') {
      // Latch once the console actually connects, so the self-heal close-handler
      // knows this session was established and a later drop is worth retrying.
      connectedRef.current = true;
      // A real connect earns a FRESH self-heal budget and clears any prior
      // give-up — without this, the bounded backoff cursor would never reset.
      reconnectAttemptsRef.current = 0;
      // A real connect also clears the auto-reauth budget: the loop is broken,
      // so a future expiry gets a fresh set of auto-attempts.
      reauthAttemptsRef.current = 0;
      stoppedRef.current = false;
    }
    // No 'reconnecting' toast: ConsoleView's connection-state overlay already
    // shows the full-bleed "Reconnecting…" surface — a toast on top duplicates
    // the same event.
    if (status.kind === 'error') {
      // Halt the self-heal close-handler ONLY for a typed PERMANENT cause
      // (session_expired / rbac_denied / vm_not_found / config_error) — retrying
      // those can't help. Retryable causes (unreachable / session_closed /
      // frame_too_large) must NOT latch stoppedRef: Reconnect re-mints the
      // session (reopenStream), so recovery stays possible. A causeless 'error'
      // is the watchdog give-up deadline, handled by the exhausted backoff. We
      // raise NO toast here: ConsoleView's typed error overlay is the single,
      // plain-language surface that guides the operator, and stacking a jargon
      // "Console error" toast on top only confuses.
      if (status.cause !== undefined && !isRetryableCause(status.cause)) {
        stoppedRef.current = true;
      }
    }
  }, [status, toast]);

  // Circular re-auth arm (SH1): when THIS console's Prism session expires and the
  // window is FOCUSED, auto-open the SSO flow (reauthNow → validate → reconnect)
  // so the operator just signs in and the console resumes — no manual click. A
  // background/unfocused console keeps the one-click overlay (no surprise
  // credential popup when you're looking elsewhere); if the operator later
  // focuses it, the one-shot focus listener auto-opens then. reauthNow is
  // deduped by manualBusyRef, and a cancelled/failed sign-in falls back to the
  // overlay's Re-authenticate button — so the loop stays alive and repeatable.
  useEffect(() => {
    if (status.kind !== 'error' || status.cause !== 'session_expired') return;
    // Backstop: stop AUTO-opening SSO after maxAutoReauth consecutive cycles
    // with no 'connected' between them. The overlay's manual Re-authenticate
    // button (onReauth) still fires reauthNow, so the operator is never stuck —
    // we just stop the surprise auto-popup if something keeps re-expiring.
    if (reauthAttemptsRef.current >= maxAutoReauth) return;
    const fire = () => {
      reauthAttemptsRef.current += 1;
      reauthNow();
    };
    if (document.hasFocus()) {
      fire();
      return;
    }
    const onFocus = () => fire();
    window.addEventListener('focus', onFocus, { once: true });
    return () => window.removeEventListener('focus', onFocus);
  }, [status, reauthNow]);

  if (error) {
    return (
      <div
        style={{
          height: '100vh',
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          justifyContent: 'center',
          background: '#000',
          color: 'var(--hc-fg-1)',
          padding: 24,
          textAlign: 'center',
          gap: 8,
        }}
      >
        <div style={{ fontSize: 16, color: 'var(--hc-error)', fontWeight: 600 }}>
          Can’t open this VM console
        </div>
        <div style={{ fontSize: 13, maxWidth: 480, lineHeight: 1.5, opacity: 0.9 }}>
          The local helper that connects to your VMs isn’t responding. Click Try again in a moment.
          If it keeps happening, close this window and open the VM again from the main app.
        </div>
        <div style={{ display: 'flex', gap: 10, marginTop: 8 }}>
          <OverlayButton onClick={() => window.location.reload()} primary>
            Try again
          </OverlayButton>
          <OverlayButton onClick={() => window.close()}>Close</OverlayButton>
        </div>
      </div>
    );
  }

  if (!ws || !client) {
    return (
      <div
        style={{
          height: '100vh',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          background: '#000',
          color: 'var(--hc-fg-2)',
        }}
      >
        Connecting to {vmName}…
      </div>
    );
  }

  return (
    <ClientContext.Provider value={client}>
      <div style={{ height: '100vh', background: '#000' }}>
        <ConsoleView
          sessionId={currentSessionId}
          vmName={vmName}
          vmUuid={vmUuid}
          active
          popout
          ws={ws}
          onClose={() => window.close()}
          onStatusChange={setStatus}
          onReconnect={reconnectNow}
          onReauth={reauthNow}
        />
      </div>
    </ClientContext.Provider>
  );
}
