/**
 * Diagnostics-bundle host-log collection + zip assembly, extracted verbatim
 * from main.ts (FE-05). These helpers do not touch the main window or the
 * runtime — they only read host-side logs and (re)write a zip on disk — so
 * they take the resolved `dataDir` as a parameter instead of reaching through
 * `this`. main.ts's `exportDiagnostics()` orchestrates the save dialog and the
 * runtime export, then calls these.
 */

import { app } from 'electron';
import log from 'electron-log';
import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import { zipSync, unzipSync } from 'fflate';
import { join } from 'node:path';

/**
 * Collect the host-side log files (main.log + runtime.log) as zip entries,
 * each capped at its last 4 MiB tail. runtime.log/main.log can grow large; we
 * only need the recent tail for diagnostics, and reading an unbounded file
 * fully into memory (then zipping synchronously on the main thread) could
 * jank or OOM the UI. Unreadable/absent logs are simply omitted.
 */
export function collectHostLogFiles(dataDir: string, prefix = ''): Record<string, Uint8Array> {
  const files: Record<string, Uint8Array> = {};
  const MAX_TAIL = 4 * 1024 * 1024;
  const add = (label: string, p: string | undefined) => {
    try {
      if (!p || !existsSync(p)) return;
      const buf = readFileSync(p);
      const tail = buf.length > MAX_TAIL ? buf.subarray(buf.length - MAX_TAIL) : buf;
      files[`${prefix}${label}`] = new Uint8Array(tail);
    } catch {
      /* unreadable log — skip */
    }
  };
  try {
    add('main.log', log.transports.file.getFile().path);
  } catch {
    /* electron-log file path unavailable */
  }
  add('runtime.log', join(dataDir, 'runtime.log'));
  return files;
}

/**
 * DX-S-03: read the runtime-produced diagnostics zip back, add the host-side
 * logs (under host-logs/) plus a small note recording the runtime.log path,
 * and rewrite the zip. Best-effort: if the bundle can't be reopened (e.g. the
 * runtime wrote a non-zip), we leave the original untouched and log a warning
 * — the operator still has the runtime bundle.
 */
export function mergeHostLogsIntoBundle(destPath: string, dataDir: string): void {
  try {
    const existing = unzipSync(new Uint8Array(readFileSync(destPath)));
    const merged: Record<string, Uint8Array> = { ...existing };
    Object.assign(merged, collectHostLogFiles(dataDir, 'host-logs/'));
    merged['host-logs/about.txt'] = new TextEncoder().encode(
      [
        'AHV Remote Console host-side logs (always included — DX-S-03)',
        `generated: ${new Date().toISOString()}`,
        `app version: ${app.getVersion()}`,
        `platform: ${process.platform} ${process.arch}`,
        `runtime.log path: ${join(dataDir, 'runtime.log')}`,
        'note: main.log is the Electron main-process log; runtime.log is the',
        '      Go runtime stdout/stderr tail. Both are host-side and are not',
        "      part of the runtime's own diagnostics endpoint.",
        '',
      ].join('\n'),
    );
    writeFileSync(destPath, zipSync(merged, { level: 6 }));
  } catch (err) {
    log.warn('diagnostics: could not merge host logs into runtime bundle', err);
  }
}

/**
 * Build a diagnostics zip from host-side logs only (main.log + runtime.log
 * + a small about.txt). Used when the runtime's /v1/diagnostics/bundle is
 * unreachable. Uses fflate (already a dependency) so we don't shell out.
 */
export function exportLocalLogsZip(destPath: string, dataDir: string): void {
  const files: Record<string, Uint8Array> = collectHostLogFiles(dataDir);
  files['about.txt'] = new TextEncoder().encode(
    [
      'AHV Remote Console diagnostics (local-log fallback)',
      `generated: ${new Date().toISOString()}`,
      `app version: ${app.getVersion()}`,
      `platform: ${process.platform} ${process.arch}`,
      `runtime.log path: ${join(dataDir, 'runtime.log')}`,
      'note: the local runtime was unreachable, so this bundle contains',
      '      host-side logs only (no runtime diagnostics endpoint).',
      '',
    ].join('\n'),
  );
  writeFileSync(destPath, zipSync(files, { level: 6 }));
}
