/* eslint-disable */
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { resolve } from 'node:path';

export default defineConfig({
  plugins: [react()],
  root: resolve(__dirname, 'src/renderer'),
  base: './',
  build: {
    outDir: resolve(__dirname, 'dist/renderer'),
    emptyOutDir: true,
    // No sourcemaps in production: ships ~1.5–2 MB of .map files otherwise.
    // Switch to 'hidden' if/when Sentry (or another crash reporter) is added
    // so crash reports can be symbolicated without shipping the //# source
    // mapping URL comment.
    sourcemap: process.env.NODE_ENV === 'production' ? false : true,
    target: 'chrome120',
  },
  server: {
    port: 5173,
    strictPort: true,
  },
});
