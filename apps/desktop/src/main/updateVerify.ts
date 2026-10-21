/**
 * Update-manifest signature verification (SECU-03).
 *
 * On Linux, electron-updater's AppImage path performs NO publisher-signature
 * enforcement: it downloads `latest-linux.yml` + the AppImage over TLS and
 * applies it, trusting only transport integrity. A feed compromise (or a
 * TLS-terminating proxy with a trusted cert) could therefore push a malicious
 * AppImage. macOS (Squirrel.Mac) and Windows (NSIS) enforce the OS code
 * signature on the downloaded artifact; Linux AppImage has no such gate, so we
 * add an application-level detached Ed25519 signature over the update manifest.
 *
 * SCHEME (minisign-compatible, Ed25519):
 *   - Ops generates a keypair with `minisign -G` (or libsodium). The 32-byte
 *     raw Ed25519 PUBLIC key is embedded in the app (see UPDATE_PUBKEY_B64
 *     below — currently a placeholder; see the SECU-03 entry under "incomplete":
 *     key generation + distribution is needs-ops and MUST NOT be invented here).
 *   - The release pipeline signs the raw bytes of `latest-linux.yml` with the
 *     matching SECRET key and publishes the detached signature alongside it as
 *     `latest-linux.yml.minisig`.
 *   - Before applying a Linux AppImage update we fetch the .minisig, parse the
 *     Ed25519 signature line, and verify it against the manifest bytes with the
 *     embedded public key. Verification failure => abort the update.
 *
 * This module is pure (node:crypto only, no Electron) so it is unit-testable.
 */

import { createPublicKey, verify as cryptoVerify } from 'node:crypto';

/**
 * DER SubjectPublicKeyInfo prefix for a raw 32-byte Ed25519 public key. Node's
 * crypto.createPublicKey needs SPKI/PEM, but minisign distributes the raw key,
 * so we re-wrap: this 12-byte prefix + the 32 raw bytes = a valid Ed25519 SPKI.
 */
const ED25519_SPKI_PREFIX = Buffer.from([
  0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00,
]);

/**
 * Wrap a raw 32-byte Ed25519 public key as a node:crypto KeyObject. Throws if
 * the key is not exactly 32 bytes.
 */
export function ed25519PublicKeyFromRaw(raw: Buffer): ReturnType<typeof createPublicKey> {
  if (raw.length !== 32) {
    throw new Error(`ed25519 public key must be 32 bytes, got ${raw.length}`);
  }
  const spki = Buffer.concat([ED25519_SPKI_PREFIX, raw]);
  return createPublicKey({ key: spki, format: 'der', type: 'spki' });
}

/**
 * Parse a minisign signature file (`*.minisig`). Layout (text, LF-separated):
 *   line 0: untrusted comment: <text>
 *   line 1: base64( signature_algorithm[2] || key_id[8] || ed25519_sig[64] )
 *   line 2: trusted comment: <text>
 *   line 3: base64( ed25519_sig_of_(payload || trusted_comment)[64] )
 *
 * We support the legacy/pure Ed25519 algorithm `Ed` (signature over the raw
 * file bytes). The prehashed variant `ED` (BLAKE2b-512 of the file) is NOT
 * supported here — the pipeline must sign with the non-prehashed algorithm.
 * Returns the 64-byte Ed25519 signature over the payload.
 */
export function parseMinisig(contents: string): { algorithm: string; signature: Buffer } {
  const lines = contents.split('\n');
  if (lines.length < 2) {
    throw new Error('minisig: malformed (expected at least 2 lines)');
  }
  const sigLine = lines[1]!.trim();
  const blob = Buffer.from(sigLine, 'base64');
  if (blob.length !== 2 + 8 + 64) {
    throw new Error(`minisig: unexpected signature blob length ${blob.length}`);
  }
  const algorithm = blob.subarray(0, 2).toString('ascii');
  if (algorithm !== 'Ed') {
    // 'ED' is the prehashed variant; we deliberately only accept the
    // straight-Ed25519-over-file-bytes form to keep verification simple.
    throw new Error(`minisig: unsupported algorithm ${algorithm} (expected 'Ed')`);
  }
  const signature = blob.subarray(10);
  return { algorithm, signature };
}

/**
 * Verify a detached Ed25519 signature over `manifest` using a raw 32-byte
 * public key. Returns true iff valid. Never throws on a bad signature — only
 * on a structurally invalid key (wrong length).
 */
export function verifyEd25519(manifest: Buffer, signature: Buffer, rawPublicKey: Buffer): boolean {
  const key = ed25519PublicKeyFromRaw(rawPublicKey);
  if (signature.length !== 64) return false;
  try {
    return cryptoVerify(null, manifest, key, signature);
  } catch {
    return false;
  }
}

/**
 * Verify a Linux update manifest against its minisign signature file using the
 * embedded raw public key (base64). Returns true iff the signature is valid.
 *
 * `publicKeyB64` is the raw 32-byte Ed25519 key, base64-encoded. When it is
 * empty/unset (the current default — no key has been provisioned; see
 * "incomplete"), verification CANNOT pass: this returns false so the updater
 * fails closed rather than applying an unverified AppImage.
 */
export function verifyUpdateManifest(
  manifest: Buffer,
  minisigContents: string,
  publicKeyB64: string,
): boolean {
  if (!publicKeyB64) return false;
  let rawPublicKey: Buffer;
  try {
    rawPublicKey = Buffer.from(publicKeyB64, 'base64');
  } catch {
    return false;
  }
  let signature: Buffer;
  try {
    ({ signature } = parseMinisig(minisigContents));
  } catch {
    return false;
  }
  return verifyEd25519(manifest, signature, rawPublicKey);
}

/**
 * The release-signing PUBLIC key (raw 32-byte Ed25519, base64).
 *
 * NEEDS-OPS (SECU-03): empty by default. Ops must generate the keypair
 * (`minisign -G`), publish the manifest signature in the pipeline, and inject
 * the public key here (or via an env/build define). DO NOT invent a key — an
 * empty value makes verifyUpdateManifest() fail closed, which keeps the Linux
 * AppImage updater disabled until a real key is provisioned.
 */
export const UPDATE_PUBKEY_B64 = process.env.AHV_REMOTE_CONSOLE_UPDATE_PUBKEY_B64 ?? '';
