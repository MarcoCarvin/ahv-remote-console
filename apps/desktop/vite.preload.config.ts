/* eslint-disable */
import { defineConfig } from 'vite';
import { resolve } from 'node:path';

export default defineConfig({
  build: {
    outDir: 'dist/preload',
    emptyOutDir: true,
    lib: {
      entry: resolve(__dirname, 'src/preload/preload.ts'),
      formats: ['cjs'],
      fileName: () => 'preload.js',
    },
    rollupOptions: {
      external: ['electron'],
    },
    target: 'node20',
    // Minify in production only (parity with main bundle).
    minify: process.env.NODE_ENV === 'production' ? 'esbuild' : false,
    // No sourcemaps in production. See vite.renderer.config.ts for the
    // 'hidden' upgrade path if Sentry is added later.
    sourcemap: process.env.NODE_ENV === 'production' ? false : true,
  },
});
