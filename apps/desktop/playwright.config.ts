import { defineConfig } from '@playwright/test';

/**
 * Playwright config for end-to-end smoke tests against the PACKAGED
 * Electron app (apps/desktop/release/<platform>/...). These tests are
 * a dev/prod parity guard — they exercise the same .app/.exe/AppImage
 * the user installs and catch asar issues, resource-path mismatches,
 * signing failures, and plugin-loading regressions that the dev-mode
 * vitest unit tests cannot.
 *
 * They are NOT integration tests: no real Nutanix cluster is required,
 * and total wall-clock time is well under 60s.
 *
 * To run locally after a `pnpm package:mac` / package:linux / package:win:
 *   pnpm --filter @ahv-remote-console/desktop e2e
 *
 * Or with the inspector:
 *   pnpm --filter @ahv-remote-console/desktop e2e:debug
 */
export default defineConfig({
  testDir: './e2e',
  // 60s total: each spec spawns Electron, opens a window, runs a few
  // assertions, and quits. Anything beyond a minute means something is
  // genuinely wedged.
  timeout: 60_000,
  // Electron-driven tests cannot share state (each launches its own
  // chromium-electron process and tray icon) — keep them serial.
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: [['list'], ['html', { open: 'never', outputFolder: 'playwright-report' }]],
  use: {
    trace: 'retain-on-failure',
  },
});
