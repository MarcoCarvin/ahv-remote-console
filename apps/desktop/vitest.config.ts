import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    // Exclude Playwright e2e tests (they run separately via `pnpm e2e`).
    exclude: ['e2e/**', 'node_modules/**', 'dist/**', 'release/**'],
    include: ['src/**/*.{test,spec}.{ts,tsx}'],
  },
});
