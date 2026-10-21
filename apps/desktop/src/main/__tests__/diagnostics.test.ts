import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { mkdtempSync, writeFileSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { unzipSync, zipSync } from 'fflate';

/**
 * FE-05: tests for the diagnostics-bundle host-log helpers extracted verbatim
 * from main.ts. The behavior under test is pure given (dataDir, files on disk):
 * tail-capping, zip assembly, and the merge-into-existing-bundle path. We mock
 * `electron` (app.getVersion) and `electron-log` (the main.log path + warn),
 * and exercise the real node:fs + fflate logic against a temp dir.
 */

const { mainLogPath } = vi.hoisted(() => ({ mainLogPath: { path: '' } }));

vi.mock('electron', () => ({
  app: { getVersion: () => '9.9.9-test' },
}));

vi.mock('electron-log', () => ({
  default: {
    transports: { file: { getFile: () => ({ path: mainLogPath.path }) } },
    warn: vi.fn(),
  },
}));

import { collectHostLogFiles, mergeHostLogsIntoBundle, exportLocalLogsZip } from '../diagnostics';

let dir: string;

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), 'hc-diag-'));
  mainLogPath.path = join(dir, 'main.log');
});

afterEach(() => {
  rmSync(dir, { recursive: true, force: true });
});

describe('collectHostLogFiles', () => {
  it('includes main.log and runtime.log when present', () => {
    writeFileSync(mainLogPath.path, 'main log body');
    writeFileSync(join(dir, 'runtime.log'), 'runtime log body');
    const files = collectHostLogFiles(dir);
    expect(Object.keys(files).sort()).toEqual(['main.log', 'runtime.log']);
    expect(new TextDecoder().decode(files['main.log'])).toBe('main log body');
    expect(new TextDecoder().decode(files['runtime.log'])).toBe('runtime log body');
  });

  it('omits absent log files rather than throwing', () => {
    // Neither file exists on disk.
    const files = collectHostLogFiles(dir);
    expect(files).toEqual({});
  });

  it('applies the prefix to every entry key', () => {
    writeFileSync(mainLogPath.path, 'x');
    writeFileSync(join(dir, 'runtime.log'), 'y');
    const files = collectHostLogFiles(dir, 'host-logs/');
    expect(Object.keys(files).sort()).toEqual(['host-logs/main.log', 'host-logs/runtime.log']);
  });

  it('caps each file at the last 4 MiB tail', () => {
    const MAX_TAIL = 4 * 1024 * 1024;
    // 4 MiB + 10 bytes: first 10 bytes are a marker that must be dropped,
    // leaving exactly the last 4 MiB.
    const head = 'HEADMARKER';
    const body = head + 'a'.repeat(MAX_TAIL);
    writeFileSync(join(dir, 'runtime.log'), body);
    const files = collectHostLogFiles(dir);
    const out = files['runtime.log'];
    expect(out).toBeDefined();
    expect(out?.byteLength).toBe(MAX_TAIL);
    // The 10-byte head marker was trimmed off the front.
    expect(new TextDecoder().decode(out!.subarray(0, 10))).toBe('a'.repeat(10));
  });

  it('keeps a small file untouched (no tail trim)', () => {
    writeFileSync(join(dir, 'runtime.log'), 'short');
    const files = collectHostLogFiles(dir);
    const out = files['runtime.log'];
    expect(out).toBeDefined();
    expect(out?.byteLength).toBe('short'.length);
  });
});

describe('exportLocalLogsZip', () => {
  it('writes a zip containing about.txt plus the present logs', () => {
    writeFileSync(mainLogPath.path, 'main');
    writeFileSync(join(dir, 'runtime.log'), 'runtime');
    const dest = join(dir, 'out.zip');
    exportLocalLogsZip(dest, dir);
    const entries = unzipSync(new Uint8Array(readFileSync(dest)));
    expect(Object.keys(entries).sort()).toEqual(['about.txt', 'main.log', 'runtime.log']);
    const about = new TextDecoder().decode(entries['about.txt']);
    expect(about).toContain('local-log fallback');
    expect(about).toContain('9.9.9-test');
    expect(about).toContain(join(dir, 'runtime.log'));
  });

  it('still produces a zip with just about.txt when no logs exist', () => {
    const dest = join(dir, 'out.zip');
    exportLocalLogsZip(dest, dir);
    const entries = unzipSync(new Uint8Array(readFileSync(dest)));
    expect(Object.keys(entries)).toEqual(['about.txt']);
  });
});

describe('mergeHostLogsIntoBundle', () => {
  it('adds host-logs/ entries + about.txt to an existing zip, preserving prior entries', () => {
    writeFileSync(mainLogPath.path, 'main');
    writeFileSync(join(dir, 'runtime.log'), 'runtime');
    // Simulate a runtime-produced bundle already on disk.
    const dest = join(dir, 'bundle.zip');
    writeFileSync(
      dest,
      zipSync({ 'runtime-bundle/info.txt': new TextEncoder().encode('from runtime') }),
    );

    mergeHostLogsIntoBundle(dest, dir);

    const entries = unzipSync(new Uint8Array(readFileSync(dest)));
    expect(Object.keys(entries).sort()).toEqual([
      'host-logs/about.txt',
      'host-logs/main.log',
      'host-logs/runtime.log',
      'runtime-bundle/info.txt',
    ]);
    // Pre-existing entry untouched.
    expect(new TextDecoder().decode(entries['runtime-bundle/info.txt'])).toBe('from runtime');
    expect(new TextDecoder().decode(entries['host-logs/about.txt'])).toContain(
      'host-side logs (always included',
    );
  });

  it('leaves the destination untouched when it is not a valid zip', () => {
    const dest = join(dir, 'notazip.bin');
    writeFileSync(dest, 'this is not a zip archive');
    // Should swallow the unzip error (best-effort) and not throw.
    expect(() => mergeHostLogsIntoBundle(dest, dir)).not.toThrow();
    // Original bytes preserved.
    expect(readFileSync(dest, 'utf8')).toBe('this is not a zip archive');
  });
});
