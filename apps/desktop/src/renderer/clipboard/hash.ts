/**
 * SHA-256 hashing for the clipboard ledger and the audit emitter.
 *
 * Uses the platform SubtleCrypto. Electron renderer has it natively
 * regardless of secure-context (it's a chrome:// origin); browsers
 * require HTTPS/localhost which the future browser build will
 * satisfy. There's no in-process fallback because we never want a
 * weaker hash silently substituted — if SubtleCrypto isn't there,
 * the ledger is degraded and we'd rather flag it loudly.
 */

export async function sha256Hex(input: string | Uint8Array): Promise<string> {
  const bytes = typeof input === 'string' ? new TextEncoder().encode(input) : input;
  const digest = await crypto.subtle.digest('SHA-256', bytes as BufferSource);
  const view = new Uint8Array(digest);
  let out = '';
  for (const b of view) {
    out += b.toString(16).padStart(2, '0');
  }
  return out;
}
