/* eslint-disable */
import { builtinModules } from 'node:module';
import { resolve } from 'node:path';
import { defineConfig } from 'vite';

// Every Node built-in (and its `node:` prefix variant) must be externalized
// when bundling the Electron main process — they are provided by the Node
// runtime that ships inside Electron, not by Vite.
const nodeBuiltins = [...builtinModules, ...builtinModules.map((m) => `node:${m}`)];

export default defineConfig({
  build: {
    outDir: 'dist/main',
    emptyOutDir: true,
    lib: {
      entry: resolve(__dirname, 'src/main/main.ts'),
      formats: ['cjs'],
      fileName: () => 'main.js',
    },
    rollupOptions: {
      external: ['electron', 'electron-log', 'electron-updater', ...nodeBuiltins],
      output: {
        format: 'cjs',
      },
    },
    target: 'node22',
    // Minify in production only. Main bundle is ~39 KB; minified saves
    // ~10–15 KB and shaves a few ms of parse time at app startup.
    minify: process.env.NODE_ENV === 'production' ? 'esbuild' : false,
    // No sourcemaps in production (see vite.renderer.config.ts). Switch to
    // 'hidden' when/if Sentry is wired up.
    sourcemap: process.env.NODE_ENV === 'production' ? false : true,
  },
});
