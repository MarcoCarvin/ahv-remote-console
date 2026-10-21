/**
 * Main-process HMAC signer for IPC requests to the local Go runtime.
 *
 * Previously the IPC secret was handed to the renderer as a hex string via
 * `contextBridge.exposeInMainWorld('ahvRemoteConsole', { getIPCSecretHex })`,
 * which meant *any* code running in the renderer's V8 isolate — including
 * DevTools, future renderer-side plugin UIs, or any XSS in a plugin's
 * rendered HTML — could call `window.ahvRemoteConsole.getIPCSecretHex()` and
 * read the secret. Compromised renderer ⇒ full unattenuated access to the
 * runtime, and that runtime is what tunnels customer infrastructure
 * credentials.
 *
 * The right shape is "secret never leaves main": the renderer asks main
 * to sign on its behalf via two narrow RPCs (signRequest / signWebSocket)
 * and only the resulting headers/subprotocol cross the contextBridge.
 * Compromised renderer ⇒ can request signatures for the URIs main is
 * willing to sign for (controllable via per-call authorization), but
 * never gets the secret itself.
 *
 * The signer also gives us a single audit chokepoint.
 */

import { createHash, createHmac, randomBytes } from 'node:crypto';

const IPC_HEADER_SIG = 'X-HC-Sig';
const IPC_HEADER_TS = 'X-HC-Timestamp';
const IPC_HEADER_NONCE = 'X-HC-Nonce';

export interface SignedHeaders {
  [IPC_HEADER_SIG]: string;
  [IPC_HEADER_TS]: string;
  [IPC_HEADER_NONCE]: string;
  // Index signature so callers can spread these into header bags typed
  // as `Record<string, string>` (electron.net.request.setHeader). The
  // concrete fields above keep the contract checkable at use sites.
  [k: string]: string;
}

export class Signer {
  // The secret lives only in this closure. Held as a Buffer so we can
  // zero it on shutdown — Go runtime's matching code does the same via
  // Secret.Wipe() in apps/runtime/internal/ipc/secret.go.
  private secret: Buffer;
  private wiped = false;

  constructor(secret: Buffer) {
    // Defensive copy so the caller can wipe their own buffer separately.
    this.secret = Buffer.from(secret);
  }

  /**
   * Sign an HTTP request. Returns the three header values the runtime
   * checks (X-HC-Sig, X-HC-Timestamp, X-HC-Nonce). Body is hashed
   * server-side comparison; pass null for empty body.
   */
  signRequest(method: string, uri: string, body: string | Buffer | null): SignedHeaders {
    if (this.wiped) throw new Error('signer wiped');
    const timestamp = Math.floor(Date.now() / 1000).toString();
    const nonce = randomBytes(16).toString('hex');
    const bodyHash = createHash('sha256')
      .update(body ?? '')
      .digest('hex');
    const canonical = [method.toUpperCase(), uri, timestamp, nonce, bodyHash].join('\n');
    const sig = createHmac('sha256', this.secret).update(canonical).digest('hex');
    return {
      [IPC_HEADER_SIG]: sig,
      [IPC_HEADER_TS]: timestamp,
      [IPC_HEADER_NONCE]: nonce,
    };
  }

  /**
   * Build the Sec-WebSocket-Protocol subprotocol used for HMAC-
   * authenticated WS upgrades. Runtime expects `hc.v1.<sig>.<ts>.<nonce>`.
   * Bodyless: WebSocket upgrades are GETs with no body.
   */
  signWebSocket(uri: string): string {
    if (this.wiped) throw new Error('signer wiped');
    const timestamp = Math.floor(Date.now() / 1000).toString();
    const nonce = randomBytes(16).toString('hex');
    const bodyHash = createHash('sha256').update('').digest('hex');
    const canonical = ['GET', uri, timestamp, nonce, bodyHash].join('\n');
    const sig = createHmac('sha256', this.secret).update(canonical).digest('hex');
    return `hc.v1.${sig}.${timestamp}.${nonce}`;
  }

  /**
   * Compute SHA-256 of the secret, returned as base64url-no-padding to
   * match the format the runtime writes into the handshake file's
   * `secret_hash`. Used by main on startup to verify the runtime came
   * up with the same secret we fed it on stdin — never exposed to the
   * renderer (the renderer doesn't need to verify because it trusts
   * main, which trusts its own check here).
   */
  secretHash(): string {
    if (this.wiped) throw new Error('signer wiped');
    const digest = createHash('sha256').update(this.secret).digest();
    return digest.toString('base64').replace(/=+$/, '').replace(/\+/g, '-').replace(/\//g, '_');
  }

  /**
   * Zero the secret buffer. After this, every method throws — caller
   * should drop the Signer reference. Called on app shutdown so the
   * secret doesn't sit in V8 memory waiting for a heap snapshot.
   */
  wipe(): void {
    if (this.wiped) return;
    this.secret.fill(0);
    this.wiped = true;
  }
}
