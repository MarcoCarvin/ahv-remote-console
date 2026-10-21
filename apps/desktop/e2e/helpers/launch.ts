import { existsSync, readdirSync, statSync } from 'node:fs';
import { basename, join, resolve } from 'node:path';
import { arch, platform } from 'node:os';

/**
 * Resolve the absolute path to the PACKAGED Electron app's main
 * executable for the current platform/arch. electron-builder writes
 * unpacked output to apps/desktop/release/<dir>/ — this helper walks
 * the platform-specific layout and returns the binary that Playwright
 * can hand to `_electron.launch({ executablePath })`.
 *
 * IMPORTANT (VAL-E2E-1): electron-builder.yml sets
 *   productName: AHV Remote Console
 * and deliberately does NOT set `executableName`. That means the
 * packaged outputs use the SPACED, mixed-case product name, NOT the
 * dashed/lowercase `ahv-remote-console` slug:
 *
 *   macOS arm64   -> release/mac-arm64/AHV Remote Console.app/Contents/MacOS/AHV Remote Console
 *   macOS x64     -> release/mac/AHV Remote Console.app/Contents/MacOS/AHV Remote Console
 *   Linux         -> release/linux-unpacked/<exe>            (electron-builder names this
 *                                                             from name/productName; we
 *                                                             discover it dynamically)
 *   Windows       -> release/win-unpacked/AHV Remote Console.exe
 *
 * A previous version of this helper hardcoded the dashed
 * `ahv-remote-console[.app|.exe]` paths. Those NEVER exist on disk, so
 * `existsSync` always failed, `resolvePackagedApp()` always returned
 * null, and the entire smoke suite SILENTLY SKIPPED — it could go green
 * in CI without ever launching the app. We now resolve the bundle
 * dynamically (glob the real output) instead of hardcoding any name, so
 * the suite locates whatever electron-builder actually produced.
 *
 * Tests should call this once. If a packaged build is ABSENT the caller
 * must skip-with-loud-warning or hard-error (see smoke.spec.ts) — it must
 * NOT pass silently, otherwise the suite can green without launching.
 */

const REPO_DESKTOP = resolve(__dirname, '..', '..');
const RELEASE_DIR = join(REPO_DESKTOP, 'release');

/**
 * The human-facing product name from electron-builder.yml. Used as the
 * preferred match when several executables exist in an unpacked dir
 * (Linux/Windows ship helper binaries alongside the app binary).
 */
const PRODUCT_NAME = 'AHV Remote Console';

/**
 * Helper/sidecar binaries electron ships next to the real app executable
 * in the Linux/Windows unpacked output. We never want to launch these.
 */
const NON_APP_EXECUTABLES = new Set(
  [
    'chrome-sandbox',
    'chrome_crashpad_handler',
    'crashpad_handler',
    'libEGL.so',
    'libGLESv2.so',
    'libffmpeg.so',
    'libvk_swiftshader.so',
    'libvulkan.so.1',
  ].map((n) => n.toLowerCase()),
);

export interface ResolvedApp {
  /** Absolute path to the executable to hand Playwright. */
  executablePath: string;
  /** Absolute path to the .app bundle (macOS only) or unpacked dir (other). */
  bundlePath: string;
  /** Absolute path to the Resources/ directory (where bridge.iso, icon.icns live). */
  resourcesPath: string;
}

function safeReaddir(dir: string): string[] {
  try {
    return readdirSync(dir);
  } catch {
    return [];
  }
}

function isFileExecutable(path: string): boolean {
  try {
    const st = statSync(path);
    if (!st.isFile()) return false;
    // On POSIX the app binary has an exec bit; on Windows we key off the
    // .exe extension instead (handled by the caller).
    return platform() === 'win32' ? true : (st.mode & 0o111) !== 0;
  } catch {
    return false;
  }
}

/**
 * macOS: glob release/ for any `*.app` bundle and return its main
 * executable. electron-builder names the bundle from productName, so it
 * lands at `<dir>/AHV Remote Console.app`. We don't hardcode that name —
 * we take the first `.app` we find under release/ (arm64 and x64 builds
 * land in distinct dirs, so there's normally exactly one for the current
 * arch). The executable is `Contents/MacOS/<bundle basename minus .app>`.
 */
function findMacAppBundle(): string | null {
  // Search shallowly first (the common electron-builder layout puts the
  // bundle directly under release/<mac|mac-arm64>/), then fall back to a
  // bounded recursive walk so we tolerate layout changes.
  const found: string[] = [];
  const walk = (dir: string, depth: number): void => {
    if (depth > 4) return;
    for (const entry of safeReaddir(dir)) {
      const full = join(dir, entry);
      if (entry.endsWith('.app')) {
        found.push(full);
        continue; // don't descend into the bundle itself
      }
      let isDir = false;
      try {
        isDir = statSync(full).isDirectory();
      } catch {
        isDir = false;
      }
      if (isDir) walk(full, depth + 1);
    }
  };
  walk(RELEASE_DIR, 0);
  if (found.length === 0) return null;
  // Prefer an arch-appropriate bundle if the dir name hints at it.
  const wantArm = arch() === 'arm64';
  const preferred = found.find((p) =>
    wantArm ? /mac-arm64/i.test(p) : /(?:^|[\\/])mac(?:[\\/]|$)/i.test(p) && !/mac-arm64/i.test(p),
  );
  return preferred ?? found[0] ?? null;
}

/**
 * Linux/Windows: pick the app executable out of an unpacked dir. We
 * prefer an exact match on the product name (with the platform suffix on
 * Windows), then fall back to the single non-helper executable present.
 */
function findUnpackedExecutable(dir: string, ext: '' | '.exe'): string | null {
  const entries = safeReaddir(dir);
  if (entries.length === 0) return null;

  // 1) Exact product-name match (the expected electron-builder output).
  const exact = `${PRODUCT_NAME}${ext}`;
  if (entries.includes(exact)) {
    const full = join(dir, exact);
    if (existsSync(full)) return full;
  }

  // 2) Fall back to dynamic discovery: the real app binary is the one
  //    file that is executable (POSIX) / ends in .exe (Windows) and is
  //    not a known electron sidecar/helper.
  const candidates = entries
    .map((name) => ({ name, full: join(dir, name) }))
    .filter(({ name }) => {
      if (ext === '.exe') return name.toLowerCase().endsWith('.exe');
      // POSIX: skip obvious shared libs and known helpers.
      if (NON_APP_EXECUTABLES.has(name.toLowerCase())) return false;
      if (/\.(so|so\.\d+|dylib|pak|dat|bin|json|html)$/i.test(name)) return false;
      return true;
    })
    .filter(({ name, full }) => {
      if (NON_APP_EXECUTABLES.has(name.toLowerCase())) return false;
      return isFileExecutable(full);
    });

  if (candidates.length === 1) return candidates[0]!.full;
  // Several candidates (rare) — prefer one whose name looks like the
  // product (case-insensitive, spaces or dashes tolerated).
  const slug = (s: string): string => s.toLowerCase().replace(/[\s_-]+/g, '');
  const wanted = slug(PRODUCT_NAME);
  const byName = candidates.find(({ name }) => slug(basename(name, ext)) === wanted);
  return byName?.full ?? candidates[0]?.full ?? null;
}

export function resolvePackagedApp(): ResolvedApp | null {
  const plat = platform();

  if (plat === 'darwin') {
    const bundlePath = findMacAppBundle();
    if (!bundlePath) return null;
    const exeName = basename(bundlePath).replace(/\.app$/, '');
    const executablePath = join(bundlePath, 'Contents', 'MacOS', exeName);
    const resourcesPath = join(bundlePath, 'Contents', 'Resources');
    if (!existsSync(executablePath)) return null;
    return { executablePath, bundlePath, resourcesPath };
  }

  if (plat === 'linux') {
    const bundlePath = join(RELEASE_DIR, 'linux-unpacked');
    const executablePath = findUnpackedExecutable(bundlePath, '');
    if (!executablePath) return null;
    const resourcesPath = join(bundlePath, 'resources');
    return { executablePath, bundlePath, resourcesPath };
  }

  if (plat === 'win32') {
    const bundlePath = join(RELEASE_DIR, 'win-unpacked');
    const executablePath = findUnpackedExecutable(bundlePath, '.exe');
    if (!executablePath) return null;
    const resourcesPath = join(bundlePath, 'resources');
    return { executablePath, bundlePath, resourcesPath };
  }

  return null;
}

/**
 * Match the platform-arch directory naming the Go runtime uses inside
 * `resources/runtime/<os>-<arch>/`. Mirrors RuntimeManager.resolveBinary
 * so the smoke test asserts the exact path the app will look at.
 */
export function runtimeDirName(): string {
  const cpu = arch() === 'arm64' ? 'arm64' : 'x64';
  const os = platform() === 'win32' ? 'win' : platform() === 'darwin' ? 'mac' : 'linux';
  return `${os}-${cpu}`;
}

export function runtimeBinaryName(): string {
  return platform() === 'win32' ? 'ahv-remote-console-runtime.exe' : 'ahv-remote-console-runtime';
}

/**
 * Human-readable instruction for producing the packaged build the smoke
 * suite needs. Used in the loud warning / hard-error path so a missing
 * bundle is never a silent green.
 */
export function packageCommandHint(): string {
  const target = platform() === 'darwin' ? 'mac' : platform() === 'win32' ? 'win' : 'linux';
  return `pnpm --filter @ahv-remote-console/desktop package:${target}`;
}

/**
 * Resolve the packaged app or fail LOUDLY. Returns the resolved app when
 * present; otherwise prints a prominent warning to stderr describing
 * where it looked and how to build, then either throws (CI) or returns
 * null (local dev, where the caller skips-with-warning). Crucially it
 * NEVER returns null quietly — the absence is always surfaced so the
 * suite cannot go green without actually launching the app.
 *
 * @param opts.hardFail when true, throw instead of returning null. The
 *   spec passes `!!process.env.CI` so CI runners hard-fail (a green run
 *   MUST mean the app launched) while local dev gets a loud skip.
 */
export function resolvePackagedAppOrWarn(opts: { hardFail: boolean }): ResolvedApp | null {
  const app = resolvePackagedApp();
  if (app) return app;

  const lines = [
    '',
    '════════════════════════════════════════════════════════════════',
    '  E2E SMOKE: packaged Electron app NOT FOUND.',
    `  Looked under: ${RELEASE_DIR}`,
    `  Platform: ${platform()} / ${arch()}`,
    `  Build it first with:  ${packageCommandHint()}`,
    '  (The suite resolves the bundle dynamically from electron-builder',
    `   output — productName "${PRODUCT_NAME}" — so a hardcoded name is`,
    '   NOT the cause; the release/ directory simply has no build.)',
    '════════════════════════════════════════════════════════════════',
    '',
  ].join('\n');

  // eslint-disable-next-line no-console
  console.error(lines);

  if (opts.hardFail) {
    throw new Error(
      `E2E smoke cannot run: no packaged app under ${RELEASE_DIR}. Run \`${packageCommandHint()}\` before the e2e job.`,
    );
  }
  return null;
}
