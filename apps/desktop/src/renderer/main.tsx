import { Fragment, StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { LazyMotion, MotionConfig, domAnimation } from 'framer-motion';
import '@ahv-remote-console/ui/reset.css';
import '@ahv-remote-console/ui/tokens.css';
// Bundled brand fonts (woff2 served from the app origin — CSP-safe, offline-
// safe). Jost is the sans face; JetBrains Mono carries all data values.
// Latin-only subsets: the operator UI is English (incl. — – … · “ ” ↑ ↓, all in
// the latin unicode-range), so we drop the Cyrillic/Greek/Vietnamese subsets
// (~184 KB) that the bare `400.css` entrypoints would also pull in.
import '@fontsource/jost/latin-400.css';
import '@fontsource/jost/latin-500.css';
import '@fontsource/jost/latin-600.css';
import '@fontsource/jost/latin-700.css';
import '@fontsource/jetbrains-mono/latin-400.css';
import '@fontsource/jetbrains-mono/latin-500.css';
import '@fontsource/jetbrains-mono/latin-600.css';
import './motion/motion.css';
import './motion/atomic.css';
import { App } from './app/App';
import { ConsoleWindowApp } from './app/ConsoleWindowApp';
import { MotionProvider } from './motion/MotionContext';
import { setSink } from '@ahv-remote-console/telemetry';

setSink((event) => {
  // Forwarded into the runtime later; for now, no-op on the renderer side.
  if (event.kind === 'event' && event.name.startsWith('error')) {
    console.warn('[telemetry]', event);
  }
});

const params = new URLSearchParams(window.location.search);
const mode = params.get('mode');

// PERF mode (main sets ?perf=1 under AHV_REMOTE_CONSOLE_PERF=1): drop StrictMode
// so the dev build doesn't double-invoke the whole tree's renders + effects,
// making console refresh / input latency representative of a shipping build for
// benchmarking. StrictMode is already a no-op in production, so this only
// changes dev. Default keeps StrictMode's double-invoke bug-catching.
// eslint-disable-next-line react-refresh/only-export-components -- main.tsx is the render entry, not a fast-refresh component module; Strict is a wrapper alias.
const Strict = params.get('perf') === '1' ? Fragment : StrictMode;

const root = createRoot(document.getElementById('root')!);

// LazyMotion features={domAnimation} (~15 KB): includes animate,
// exit, AnimatePresence, layout, layoutId — everything the route-
// level transitions + T4 container-transform need. We DON'T use drag
// or pan anywhere in the app, so domMax (25 KB) is wasteful.
//
// MotionConfig reducedMotion="user": when the OS reports
// prefers-reduced-motion, Framer Motion automatically swaps animate
// targets to their initial values (instant). The CSS animation kill
// in motion.css covers the same case for non-motion components.
if (mode === 'console') {
  // Pop-out window: the main process spawned us with a session ID to host a
  // single VM's console. Mount only the console; no sidebar, no sign-in.
  const sessionId = params.get('session') ?? '';
  const vmName = params.get('vm') ?? 'Console';
  root.render(
    <Strict>
      <LazyMotion features={domAnimation} strict>
        <MotionConfig reducedMotion="user">
          <ConsoleWindowApp sessionId={sessionId} vmName={vmName} />
        </MotionConfig>
      </LazyMotion>
    </Strict>,
  );
} else {
  root.render(
    <Strict>
      <LazyMotion features={domAnimation} strict>
        <MotionConfig reducedMotion="user">
          <MotionProvider>
            <App />
          </MotionProvider>
        </MotionConfig>
      </LazyMotion>
    </Strict>,
  );
}

// Fade out the static boot splash (index.html) once React has committed and
// painted its first frame — replacing the dark pre-paint window so it never
// reads as a frozen black screen during the dev module/font load. Two rAFs
// guarantee React's DOM is on screen before we reveal it; the CSS opacity
// transition + delayed remove keep the handoff seamless.
function dismissBootSplash(): void {
  const el = document.getElementById('boot-splash');
  if (!el) return;
  el.style.pointerEvents = 'none';
  el.style.opacity = '0';
  window.setTimeout(() => el.remove(), 300);
}
requestAnimationFrame(() => requestAnimationFrame(dismissBootSplash));
