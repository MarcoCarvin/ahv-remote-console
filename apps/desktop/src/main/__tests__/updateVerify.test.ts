import { describe, expect, it } from 'vitest';
import { generateKeyPairSync, sign as cryptoSign } from 'node:crypto';
import {
  ed25519PublicKeyFromRaw,
  parseMinisig,
  verifyEd25519,
  verifyUpdateManifest,
} from '../updateVerify';

/**
 * SECU-03: Linux AppImage update manifest signature verification.
 *
 * We mint a real Ed25519 keypair, sign a fake `latest-linux.yml`, wrap the
 * signature in the minisign on-disk format, and prove the verifier accepts a
 * good signature and rejects tampering / wrong key / missing key.
 */

function makeKeypair() {
  const { publicKey, privateKey } = generateKeyPairSync('ed25519');
  // Raw 32-byte public key = last 32 bytes of the SPKI DER.
  const spki = publicKey.export({ type: 'spki', format: 'der' });
  const rawPub = spki.subarray(spki.length - 32);
  return { privateKey, rawPub: Buffer.from(rawPub) };
}

/** Build a minisign `*.minisig` file for a raw Ed25519 signature. */
function makeMinisig(signature: Buffer, algo = 'Ed'): string {
  const keyId = Buffer.alloc(8, 0xab);
  const blob = Buffer.concat([Buffer.from(algo, 'ascii'), keyId, signature]);
  return [
    'untrusted comment: signature from ahv-remote-console release key',
    blob.toString('base64'),
    'trusted comment: timestamp',
    Buffer.alloc(64).toString('base64'),
    '',
  ].join('\n');
}

describe('ed25519PublicKeyFromRaw', () => {
  it('rejects a non-32-byte key', () => {
    expect(() => ed25519PublicKeyFromRaw(Buffer.alloc(16))).toThrow(/32 bytes/);
  });
  it('accepts a 32-byte key', () => {
    const { rawPub } = makeKeypair();
    expect(() => ed25519PublicKeyFromRaw(rawPub)).not.toThrow();
  });
});

describe('verifyEd25519', () => {
  it('accepts a valid signature', () => {
    const { privateKey, rawPub } = makeKeypair();
    const manifest = Buffer.from('version: 1.2.3\npath: app.AppImage\n');
    const sig = cryptoSign(null, manifest, privateKey);
    expect(verifyEd25519(manifest, sig, rawPub)).toBe(true);
  });

  it('rejects a tampered manifest', () => {
    const { privateKey, rawPub } = makeKeypair();
    const manifest = Buffer.from('version: 1.2.3\n');
    const sig = cryptoSign(null, manifest, privateKey);
    expect(verifyEd25519(Buffer.from('version: 6.6.6\n'), sig, rawPub)).toBe(false);
  });

  it('rejects a signature from a different key', () => {
    const a = makeKeypair();
    const b = makeKeypair();
    const manifest = Buffer.from('version: 1.2.3\n');
    const sig = cryptoSign(null, manifest, a.privateKey);
    expect(verifyEd25519(manifest, sig, b.rawPub)).toBe(false);
  });

  it('rejects a wrong-length signature', () => {
    const { rawPub } = makeKeypair();
    expect(verifyEd25519(Buffer.from('x'), Buffer.alloc(10), rawPub)).toBe(false);
  });
});

describe('parseMinisig', () => {
  it('extracts the 64-byte Ed signature', () => {
    const sig = Buffer.alloc(64, 7);
    const { algorithm, signature } = parseMinisig(makeMinisig(sig));
    expect(algorithm).toBe('Ed');
    expect(signature.equals(sig)).toBe(true);
  });

  it('rejects the prehashed ED variant', () => {
    expect(() => parseMinisig(makeMinisig(Buffer.alloc(64), 'ED'))).toThrow(
      /unsupported algorithm/,
    );
  });

  it('rejects malformed content', () => {
    expect(() => parseMinisig('only one line')).toThrow(/malformed/);
  });
});

describe('verifyUpdateManifest', () => {
  it('verifies an end-to-end signed manifest', () => {
    const { privateKey, rawPub } = makeKeypair();
    const manifest = Buffer.from('version: 1.2.3\nfiles:\n  - url: app.AppImage\n');
    const sig = cryptoSign(null, manifest, privateKey);
    const minisig = makeMinisig(sig);
    expect(verifyUpdateManifest(manifest, minisig, rawPub.toString('base64'))).toBe(true);
  });

  it('fails closed when no public key is provisioned (needs-ops default)', () => {
    const { privateKey } = makeKeypair();
    const manifest = Buffer.from('version: 1.2.3\n');
    const sig = cryptoSign(null, manifest, privateKey);
    expect(verifyUpdateManifest(manifest, makeMinisig(sig), '')).toBe(false);
  });

  it('rejects a tampered manifest end-to-end', () => {
    const { privateKey, rawPub } = makeKeypair();
    const manifest = Buffer.from('version: 1.2.3\n');
    const sig = cryptoSign(null, manifest, privateKey);
    const minisig = makeMinisig(sig);
    expect(
      verifyUpdateManifest(Buffer.from('version: 9.9.9\n'), minisig, rawPub.toString('base64')),
    ).toBe(false);
  });
});
