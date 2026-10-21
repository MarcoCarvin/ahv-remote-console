/**
 * Spawns and supervises the Go runtime (`ahv-remote-console-runtime`). The runtime is
 * bundled with the Electron app under build/runtime/<platform>/.
 *
 * Lifecycle:
 *
 *   1. spawn the binary with an inherited stdio that lets us feed the IPC
 *      secret on stdin and capture stderr for logging.
 *   2. watch for runtime.json handshake file to appear *in the background*.
 *   3. emit `runtime:starting` / `runtime:ready` / `runtime:error` events
 *      so the main process can forward state transitions to the renderer
 *      without blocking window creation.
 *   4. expose handshake() to the renderer via IPC.
 *   5. on exit, attempt one restart with exponential backoff; if the runtime
 *      keeps crashing, surface a fatal error to the renderer.
 */

import { spawn, type ChildProcessByStdio } from 'node:child_process';
import { existsSync, readFileSync, createWriteStream, unlinkSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { platform, arch } from 'node:os';
import { setTimeout as wait } from 'node:timers/promises';
import { EventEmitter } from 'node:events';
import type { Writable, Readable } from 'node:stream';
import { app } from 'electron';
import type log from 'electron-log';
import type { Signer } from './signer';

/**
 * Resolve the path to the bridge installer ISO that ships with this
 * Electron app. Two cases:
 *
 *   Packaged builds (production .app/.exe): the ISO is bundled via
 *     electron-builder extraResources and lives at
 *     `${process.resourcesPath}/agent.iso`.
 *
 *   Dev (vite-dev): the ISO is produced by `make iso` at the repo
 *     root and lives at `<repo>/build/agent.iso`. dev.sh exports
 *     AHV_REMOTE_CONSOLE_AGENT_ISO_PATH explicitly, so this
 *     fallback is only used if Electron is launched some other way.
 *
 * Both paths are returned as absolute strings so the spawned Go
 * runtime resolves them regardless of its CWD.
 */
function agentIsoPath(): string {
  if (app.isPackaged) {
    return join(process.resourcesPath, 'agent.iso');
  }
  // In dev, app.getAppPath() is the directory containing the
  // built main bundle (e.g. apps/desktop/dist/main). Walk back to
  // the repo root.
  return join(app.getAppPath(), '..', '..', '..', '..', 'build', 'agent.iso');
}

export interface RuntimeOptions {
  dataDir: string;
  /**
   * Raw IPC secret used once at spawn time to feed the runtime over stdin.
   * The caller is expected to zero this buffer once the handshake check
   * passes — the Signer takes its own defensive copy for ongoing signing.
   */
  ipcSecret: Buffer;
  /**
   * Signer to use for diagnostics / any main-originated signed requests.
   * Required even if you also pass ipcSecret — keeps signing logic in
   * one place.
   */
  signer: Signer;
  logger: typeof log;
}

export interface RuntimeHandshake {
  version: string;
  pid: number;
  endpoint: string;
  secret_hash: string;
  started_at: string;
}

/**
 * State of the supervised runtime, in the order an operator sees it during
 * a normal cold launch:
 *
 *   - `starting`: spawn() has been called and we're polling for the
 *     handshake file. The renderer should show a "connecting" overlay.
 *   - `ready`: the handshake file appeared with a valid secret-hash; the
 *     IPC server is up. All renderer-side runtime calls are safe to make.
 *   - `error`: the runtime failed to come up within the timeout, the binary
 *     was missing, or the secret-hash check failed. The renderer should
 *     surface a fatal error.
 */
export type RuntimeState = 'starting' | 'ready' | 'error';

export interface RuntimeStateEvent {
  state: RuntimeState;
  handshake?: RuntimeHandshake;
  error?: string;
}

export class RuntimeManager extends EventEmitter {
  private proc: ChildProcessByStdio<Writable, Readable, Readable> | null = null;
  private cachedHandshake: RuntimeHandshake | null = null;
  private restartAttempts = 0;
  private stopped = false;
  private spawnedAt = 0;
  // Monotonic spawn generation. Bumped on every start(); a pending
  // restartWithBackoff timer captures the generation before its wait and
  // bails if it changed (operator restart() / a newer spawn happened),
  // preventing a double-spawn that would orphan a runtime process.
  private generation = 0;
  private state: RuntimeState = 'starting';
  private lastError: string | null = null;
  /**
   * Promise that resolves the first time the runtime reaches `ready`.
   * Initialized lazily so callers can do `await runtime.whenReady()` at any
   * point — before, during, or after the handshake is observed.
   */
  private readyPromise: Promise<RuntimeHandshake> | null = null;
  private readyResolve: ((hs: RuntimeHandshake) => void) | null = null;
  private readyReject: ((err: Error) => void) | null = null;

  constructor(private readonly opts: RuntimeOptions) {
    super();
    this.resetReadyPromise();
  }

  /**
   * Current state snapshot. `state === 'ready'` is the only condition under
   * which calls that proxy to the runtime (signing requests, listing
   * endpoints, minting bridge envelopes) are guaranteed to succeed.
   */
  getState(): RuntimeStateEvent {
    return {
      state: this.state,
      handshake: this.cachedHandshake ?? undefined,
      error: this.lastError ?? undefined,
    };
  }

  /** True once the handshake has been observed and verified. */
  isReady(): boolean {
    return this.state === 'ready' && this.cachedHandshake !== null;
  }

  /**
   * Resolves with the handshake once the runtime is ready, or rejects if
   * the runtime fails to start. Useful for the rare main-process callers
   * (diagnostics, bridge envelope mint) that genuinely need to wait.
   *
   * IPC handlers that proxy renderer calls should NOT await this — they
   * should fail fast with a clear "runtime starting" error instead so
   * the renderer can show a retry UI.
   */
  whenReady(): Promise<RuntimeHandshake> {
    return this.readyPromise ?? Promise.reject(new Error('runtime not initialized'));
  }

  private resetReadyPromise(): void {
    this.readyPromise = new Promise<RuntimeHandshake>((resolve, reject) => {
      this.readyResolve = resolve;
      this.readyReject = reject;
    });
    // Prevent UnhandledPromiseRejection if nobody awaits this before the
    // runtime fails. The state machine is the canonical signal; the
    // promise is a convenience for callers that opt-in.
    this.readyPromise.catch(() => undefined);
  }

  private transitionTo(
    next: RuntimeState,
    payload: { handshake?: RuntimeHandshake; error?: Error },
  ): void {
    this.state = next;
    if (next === 'ready' && payload.handshake) {
      this.cachedHandshake = payload.handshake;
      this.lastError = null;
      this.emit('runtime:ready', payload.handshake);
      this.readyResolve?.(payload.handshake);
    } else if (next === 'error' && payload.error) {
      this.lastError = payload.error.message;
      this.emit('runtime:error', payload.error);
      this.readyReject?.(payload.error);
    } else if (next === 'starting') {
      this.cachedHandshake = null;
      this.lastError = null;
      // Refresh the promise so a subsequent restart can resolve a new one.
      this.resetReadyPromise();
      this.emit('runtime:starting');
    }
  }

  /**
   * Spawn the runtime process and kick off background handshake polling.
   *
   * This call is fire-and-forget: it returns synchronously after spawn()
   * succeeds. State transitions are reported via the `runtime:starting`,
   * `runtime:ready`, and `runtime:error` events. Callers that need to
   * block on readiness should use `whenReady()` — but the whole point of
   * this refactor is that the BrowserWindow can be created immediately
   * and the renderer drives its own "connecting" overlay.
   *
   * Returns the promise that resolves with the handshake once ready.
   * Awaiting it is optional — `main.ts` deliberately does NOT.
   */
  start(): Promise<RuntimeHandshake> {
    this.stopped = false;
    this.generation++;
    this.transitionTo('starting', {});
    const bin = this.resolveBinary();
    if (!existsSync(bin)) {
      const err = new Error(`runtime binary not found at ${bin}`);
      this.transitionTo('error', { error: err });
      return Promise.reject(err);
    }

    // Delete any stale runtime.json BEFORE spawning the new runtime.
    // A handshake file left over from a previous (possibly-crashed)
    // runtime carries the OLD secret hash; awaitHandshake would
    // happily pick it up before our just-spawned runtime overwrites
    // it, and the secret_hash check in main would then refuse to
    // continue with a confusing "secret mismatch" error. Removing it
    // here makes the file's presence a reliable signal that the
    // CURRENT runtime is up.
    const handshakePath = join(this.opts.dataDir, 'runtime.json');
    try {
      unlinkSync(handshakePath);
    } catch {
      /* not present, fine */
    }
    // Capture the spawn-time wall clock so awaitHandshake can also
    // sanity-check via mtime in case a write races at file-system
    // granularity.
    this.spawnedAt = Date.now();

    // TODO(plugins-dir): in-tree plugins (AHV) are statically linked via
    // a blank import in apps/runtime/internal/plugins/registration.go —
    // sdk.Registered() returns the linked-in builders and `--plugins-dir`
    // is currently a no-op for them. If/when we ship out-of-tree plugins
    // as .so/.dylib/.dll files via electron-builder extraResources, this
    // arg must branch on app.isPackaged and point at the bundled location
    // (e.g. `${process.resourcesPath}/runtime/${os}-${arch}/plugins/`).
    // For now we keep the userData/plugins path so the Go runtime's
    // os.MkdirAll on plugin-dir succeeds without touching read-only
    // resources inside the app bundle.
    const args = [
      '--ipc-secret-stdin',
      '--plugins-dir',
      join(this.opts.dataDir, 'plugins'),
      '--data-dir',
      this.opts.dataDir,
      '--log-format',
      'json',
      '--log-level',
      'info',
    ];

    this.opts.logger.info('spawning runtime', { bin, args: args.slice(0, 6) });
    // The runtime needs to know where the bridge ISO lives so the
    // AHV plugin can upload + attach it on operator request. Two
    // resolutions in priority order:
    //   1. Operator/CI override: AHV_REMOTE_CONSOLE_AGENT_ISO_PATH
    //      already in process.env (dev.sh exports it; CI sets it).
    //   2. Built-in default via agentIsoPath() — packaged builds
    //      resolve to process.resourcesPath/agent.iso, dev builds
    //      resolve to <repo>/build/agent.iso.
    // The runtime falls back to "build/agent.iso" relative to CWD
    // if the env var isn't set, but we don't rely on that; the
    // explicit value below makes the operator's machine work
    // regardless of where Electron was launched from.
    const env: NodeJS.ProcessEnv = {
      ...process.env,
      AHV_REMOTE_CONSOLE_LOG_DIR: join(this.opts.dataDir, 'logs'),
      AHV_REMOTE_CONSOLE_AGENT_ISO_PATH:
        // eslint-disable-next-line @typescript-eslint/prefer-nullish-coalescing -- intentional falsy check: an empty-string env var is unusable and must fall through to the default
        process.env.AHV_REMOTE_CONSOLE_AGENT_ISO_PATH || agentIsoPath(),
    };
    this.opts.logger.info('runtime env', {
      isoPath: env.AHV_REMOTE_CONSOLE_AGENT_ISO_PATH,
    });
    const proc = spawn(bin, args, {
      stdio: ['pipe', 'pipe', 'pipe'],
      env,
      windowsHide: true,
    }) as ChildProcessByStdio<Writable, Readable, Readable>;
    this.proc = proc;

    // Pipe stderr/stdout to log file.
    const logStream = createWriteStream(join(this.opts.dataDir, 'runtime.log'), { flags: 'a' });
    proc.stderr.pipe(logStream);
    proc.stdout.pipe(logStream);

    // If the child closes stdin before we finish writing — for example if a
    // previous orphaned runtime caused our spawn to fail fast — Node would
    // otherwise throw EPIPE synchronously here and crash the main process.
    // Attach an error listener and use the callback form so the supervisor's
    // exit handler (below) is the one that decides what to do.
    proc.stdin.on('error', (err) => {
      this.opts.logger.warn('runtime stdin error', err);
    });
    proc.stdin.write(this.opts.ipcSecret.toString('hex') + '\n', (err) => {
      if (err) this.opts.logger.warn('failed to write IPC secret to runtime stdin', err);
    });
    proc.stdin.end();

    proc.once('exit', (code, signal) => {
      // Ignore the exit of a process we've already replaced (e.g. an
      // operator-triggered restart() detached this child before spawning
      // a new one). Without this guard a stale exit would null out the
      // CURRENT proc reference and could trigger a spurious second
      // restart.
      if (this.proc !== proc) {
        this.opts.logger.warn('stale runtime exited (already replaced)', { code, signal });
        return;
      }
      this.opts.logger.warn('runtime exited', { code, signal });
      this.proc = null;
      this.cachedHandshake = null;
      if (!this.stopped) {
        // Mark as starting so any new ready-waiters block on the
        // restart attempt; restartWithBackoff calls start() again
        // which re-arms the state machine.
        if (this.state === 'ready') {
          this.transitionTo('starting', {});
        }
        void this.restartWithBackoff();
      }
    });

    // Background-poll for the handshake file. The returned promise is
    // attached to the state machine — it resolves the readyPromise on
    // success, or transitions to 'error' on timeout. We deliberately do
    // not await it here so start() returns immediately.
    void this.awaitHandshake(proc).then(
      (hs) => {
        // Defensive: if stop() was called or this child was replaced
        // while we were polling, do not transition. The exit handler /
        // restart path owns the next transition.
        if (this.stopped || this.proc !== proc) return;
        this.restartAttempts = 0;
        this.transitionTo('ready', { handshake: hs });
      },
      (err: Error) => {
        // If we were stopped or this child already exited (and was thus
        // replaced/cleared), the exit handler owns the restart — do NOT
        // clobber it with a fatal 'error' state. We only surface a true
        // fatal error when THIS child is still the current one and alive
        // but failed to hand off (the hard-cap case).
        if (this.stopped || this.proc !== proc) return;
        this.opts.logger.error('runtime handshake failed', err);
        // The child is wedged (alive but never bound). Kill it so we don't
        // leave a zombie. Detach first so its 'exit' no-ops (proc !== this.proc).
        this.proc = null;
        try {
          proc.kill();
        } catch {
          /* already gone */
        }
        this.transitionTo('error', { error: err });
      },
    );

    return this.readyPromise!;
  }

  /**
   * Poll for the handshake file while the spawned runtime is alive.
   *
   * We deliberately do NOT enforce a tight wall-clock deadline. On a cold
   * first launch macOS can spend several seconds assessing and starting an
   * un-notarized helper, and a plugin's Init may do bounded network I/O.
   * Killing a healthy-but-slow process at a fixed 10 s only produced a
   * kill→respawn loop that made first launch *look* broken (the renderer's
   * "cannot reach its local runtime" screen). Instead we poll until either
   * (a) the handshake appears, (b) THIS child process exits — in which case
   * the 'exit' handler owns the restart, so we bail without a fatal error —
   * or (c) a generous absolute safety cap elapses (guards against a wedged
   * process that bound nothing and never dies).
   */
  private async awaitHandshake(
    proc: ChildProcessByStdio<Writable, Readable, Readable>,
    hardCapMs = 90_000,
  ): Promise<RuntimeHandshake> {
    const handshakePath = join(this.opts.dataDir, 'runtime.json');
    const deadline = Date.now() + hardCapMs;
    while (Date.now() < deadline) {
      if (this.stopped) {
        throw new Error('runtime stopped before handshake completed');
      }
      // If this specific child died or was replaced, stop polling. The
      // 'exit' handler has already scheduled a restart; the rejection
      // here is swallowed by the caller's `this.proc !== proc` guard.
      if (this.proc !== proc || proc.exitCode !== null) {
        throw new Error('runtime process exited before handshake');
      }
      if (existsSync(handshakePath)) {
        // Second guard against picking up a stale file: ensure the
        // handshake was written AFTER our spawn. If the runtime is
        // fast enough, its mtime can equal spawnedAt — accept >=
        // rather than strictly >.
        let mtime = 0;
        try {
          mtime = statSync(handshakePath).mtimeMs;
        } catch {
          /* race: file appeared then vanished, retry */
        }
        if (mtime >= this.spawnedAt) {
          try {
            const data = readFileSync(handshakePath, 'utf8');
            const parsed = JSON.parse(data) as RuntimeHandshake;
            if (parsed.endpoint) {
              return parsed;
            }
          } catch (err) {
            this.opts.logger.warn('handshake parse failed, retrying', err);
          }
        }
      }
      await wait(120);
    }
    throw new Error(`runtime did not produce handshake within ${Math.round(hardCapMs / 1000)}s`);
  }

  /**
   * Operator-triggered restart (the renderer's "Retry" button after a
   * fatal error). Resets the automatic-restart budget and re-spawns even
   * if the supervisor had already exhausted it. Safe to call in any state.
   */
  restart(): Promise<RuntimeHandshake> {
    this.opts.logger.info('runtime restart requested');
    this.restartAttempts = 0;
    if (this.proc) {
      // Detach BEFORE killing so the old child's 'exit' handler no-ops
      // (its `this.proc !== proc` guard trips) and does not race a second
      // restart against the start() below.
      const old = this.proc;
      this.proc = null;
      try {
        old.kill();
      } catch {
        /* already dead */
      }
    }
    return this.start();
  }

  private async restartWithBackoff(): Promise<void> {
    this.restartAttempts++;
    if (this.restartAttempts > 5) {
      // Surface a TERMINAL error so the renderer leaves its "Starting…"
      // spinner and shows the fatal screen + Retry. Without this the
      // supervisor was left in 'starting' forever after a crash loop (the
      // exit handler / awaitHandshake guards intentionally don't emit an
      // error during a restart, so budget-exhaustion is the only place left
      // to declare failure). Operator Retry re-spawns because restart()
      // resets restartAttempts.
      this.opts.logger.error('runtime restart limit exceeded — giving up');
      this.transitionTo('error', {
        error: new Error('runtime failed to start after 5 attempts — see runtime.log'),
      });
      return;
    }
    // Capture the generation we're restarting from; if an operator restart()
    // (or stop()) happens during the backoff wait, the generation advances and
    // this stale timer must NOT spawn a second runtime.
    const gen = this.generation;
    const backoff = Math.min(15_000, 500 * 2 ** this.restartAttempts);
    this.opts.logger.warn(`restarting runtime in ${backoff}ms (attempt ${this.restartAttempts})`);
    await wait(backoff);
    if (this.stopped || this.generation !== gen) {
      this.opts.logger.info('backoff restart superseded — skipping stale spawn');
      return;
    }
    try {
      await this.start();
    } catch (err) {
      this.opts.logger.error('runtime restart failed', err);
    }
  }

  handshake(): RuntimeHandshake | null {
    return this.cachedHandshake;
  }

  async exportDiagnostics(destPath: string): Promise<void> {
    if (!this.cachedHandshake) {
      throw new Error('runtime starting, try again');
    }
    const { net } = await import('electron');
    const request = net.request({
      method: 'GET',
      url: `${this.cachedHandshake.endpoint}/v1/diagnostics/bundle`,
    });
    const sigHeaders = this.signHeaders('GET', '/v1/diagnostics/bundle');
    for (const [k, v] of Object.entries(sigHeaders)) request.setHeader(k, v);
    await new Promise<void>((resolve, reject) => {
      request.on('response', (response) => {
        const stream = createWriteStream(destPath);
        // electron.net.IncomingMessage is an EventEmitter, not a real
        // node:stream.Readable, so we fan out manually instead of .pipe().
        response.on('data', (chunk: Buffer) => stream.write(chunk));
        response.on('end', () => stream.end(() => resolve()));
        response.on('error', reject);
      });
      request.on('error', reject);
      request.end();
    });
  }

  /**
   * HMAC-sign headers for main-process originated requests (e.g. diagnostics
   * download). Delegates to the Signer instead of touching the raw secret.
   */
  private signHeaders(method: string, uri: string): Record<string, string> {
    return this.opts.signer.signRequest(method, uri, null);
  }

  stop(): void {
    this.stopped = true;
    if (this.proc) {
      this.proc.kill('SIGTERM');
      this.proc = null;
    }
    // Settle any pending readyPromise so callers awaiting it do not hang
    // through the rest of the shutdown sequence.
    this.readyReject?.(new Error('runtime stopped'));
  }

  private resolveBinary(): string {
    const ext = platform() === 'win32' ? '.exe' : '';
    // Match electron-builder's ${os}-${arch} template: mac/win/linux + arm64/x64
    const archDir = arch() === 'arm64' ? 'arm64' : 'x64';
    const platformDir = platform() === 'win32' ? 'win' : platform() === 'darwin' ? 'mac' : 'linux';

    // Production: bundled by electron-builder under resources/runtime/<plat>-<arch>/
    const bundled = join(
      process.resourcesPath ?? '',
      'runtime',
      `${platformDir}-${archDir}`,
      `ahv-remote-console-runtime${ext}`,
    );
    if (existsSync(bundled)) return bundled;

    // Dev: built by `make -C apps/runtime build`.
    const dev = join(
      __dirname,
      '..',
      '..',
      '..',
      'runtime',
      'bin',
      `ahv-remote-console-runtime${ext}`,
    );
    if (existsSync(dev)) return dev;

    // Fallback: same dir as the renderer build (rare but covers single-file shipping).
    return join(__dirname, '..', 'runtime', `ahv-remote-console-runtime${ext}`);
  }
}
