// Shared building blocks for the Select-Cluster screen family (the Cluster Map
// and its List fallback). Single source of truth for the tool-grade brand
// tokens, the brand glyph, and status mapping. NOTE: the toolbar is only
// partly shared — the Cluster Map uses BrandToolbar (the 50px Burst header),
// while EndpointPicker keeps its own 56px Spark toolbar by design (different
// glyph/height/add-button), so the two views deliberately do NOT render an
// identical toolbar from here.
import { useEffect, useRef } from 'react';
import { titleBarLeft } from '../platform';
import { T, noDrag, statusMeta, type DotState, type SignalMood } from './clusterTokens';

// One shared motion/interaction stylesheet for the whole family (was previously
// duplicated in EndpointPicker and ClusterMap). All motion is reduced-motion
// gated. Injected once at module load.
const SHARED_CSS_ID = 'ec-cluster-css';
if (typeof document !== 'undefined' && !document.getElementById(SHARED_CSS_ID)) {
  const s = document.createElement('style');
  s.id = SHARED_CSS_ID;
  s.textContent = `
.ec-focus:focus-visible { outline: 2px solid var(--blue); outline-offset: 2px; border-radius: 6px; }
.ec-row:hover { background: var(--soft); }
.ec-row.ec-row--sel, .ec-row.ec-row--sel:hover { background: var(--blueTint); }
.ec-burst-sync { animation: ec-spin 2.4s linear infinite; transform-origin: 50% 50%; }
@keyframes ec-spin { to { transform: rotate(360deg); } }
/* Slow brand-glyph rotation used by the console rail while (re)connecting. */
.ec-spark-spin { animation: ec-spin 8s linear infinite; transform-origin: 50% 50%; }
/* Playful brand-glyph spin on hover (e.g. the toolbar logo). */
.ec-spark-hoverspin { transform-origin: 50% 50%; transition: transform 200ms ease; cursor: pointer; }
.ec-spark-hoverspin:hover { animation: ec-spin 1.1s linear infinite; }
/* Soft expanding pulse ring behind a live StatusDot (Lobby + console). */
.ec-pulse { position: relative; }
.ec-pulse::after {
  content: ''; position: absolute; inset: 0; border-radius: 50%;
  background: currentColor; opacity: .5; animation: ec-pulse-ring 2.4s ease-out infinite;
}
@keyframes ec-pulse-ring { 0% { transform: scale(1); opacity: .5; } 80%,100% { transform: scale(2.6); opacity: 0; } }
/* Live "heartbeat" for a cluster node's status dot: the dot gently beats and
   emits an expanding ring (currentColor), so EVERY node reads as live, not a
   static box. The dot sets its own colour, so the ring is health-coded. */
.ec-dot-live { animation: ec-dot-beat 2.4s ease-in-out infinite; }
.ec-dot-live::after {
  content: ''; position: absolute; inset: 0; border-radius: 50%;
  background: currentColor; opacity: .55; animation: ec-pulse-ring 2.4s ease-out infinite;
}
@keyframes ec-dot-beat { 0%, 100% { transform: scale(1); } 50% { transform: scale(1.22); } }
.ec-flow-ok  { stroke-dasharray: 5 9; animation: ec-dash 2s linear infinite; }
.ec-flow-deg { stroke-dasharray: 5 9; animation: ec-dash 3.4s linear infinite; }
/* The link being drawn (elastic preview): same flow, faster, so a connection
   in creation reads as actively forming rather than settled. */
.ec-flow-draft { stroke-dasharray: 5 9; animation: ec-dash 1s linear infinite; }
@keyframes ec-dash { to { stroke-dashoffset: -28; } }
/* Indeterminate sweep for the console ambient line while reconnecting/lost. */
.ec-sweep { background-size: 200% 100% !important; animation: ec-sweep-x 1.4s linear infinite; }
@keyframes ec-sweep-x { to { background-position: -200% 0; } }
/* Horizontal swipe carousel (Lobby RECENT) — hide the scrollbar but keep scroll. */
.ec-hscroll { scrollbar-width: none; -ms-overflow-style: none; }
.ec-hscroll::-webkit-scrollbar { display: none; }
/* VM grid category switch: a staggered fade+slide cascade. Pure opacity +
   transform (GPU-composited) so it stays smooth at 200+ tiles — unlike the
   framer layout springs it replaced, it does no per-frame layout measurement.
   Each tile sets its own animation-delay inline for the cascade. */
.ec-fade-in { animation: ec-fade-in-kf 160ms ease both; }
@keyframes ec-fade-in-kf { from { opacity: 0; } to { opacity: 1; } }
/* fill-mode backwards (not both): the from-state holds during the per-tile
   delay so tiles stay hidden until their turn, but nothing is force-held after
   the animation ends — so runtime inline styles (e.g. the drag-dim opacity) win. */
.ec-tile-in { animation: ec-tile-in-kf 280ms cubic-bezier(0.2, 0.8, 0.2, 1) backwards; }
@keyframes ec-tile-in-kf {
  from { opacity: 0; transform: translateX(-14px) scale(0.985); }
  to { opacity: 1; transform: none; }
}
/* Folder chip "pop" when a dragged VM is dropped onto it. */
.ec-drop-pop { animation: ec-drop-pop-kf 340ms cubic-bezier(0.2, 0.8, 0.2, 1); }
@keyframes ec-drop-pop-kf {
  0% { transform: scale(1); }
  40% { transform: scale(1.13); }
  100% { transform: scale(1); }
}
/* Ambient brand-decor motion for the cluster MAP canvas (BrandDecor animated):
   a breathing accent disc, a slowly-rotating dot ring, and a gentle equalizer,
   so the map surface reads as alive rather than a flat backdrop. Paint/composite
   only (opacity + transform); disabled under reduce-motion below. */
.ec-decor-breathe { animation: ec-breathe 6s ease-in-out infinite; }
@keyframes ec-breathe { 0%, 100% { opacity: .1; } 50% { opacity: .2; } }
.ec-decor-spin-slow { transform-box: fill-box; transform-origin: center; animation: ec-spin 48s linear infinite; }
.ec-eq-bar { transform-box: fill-box; transform-origin: bottom; animation: ec-eq 2.2s ease-in-out infinite; }
@keyframes ec-eq { 0%, 100% { transform: scaleY(0.62); } 50% { transform: scaleY(1); } }
/* Connection-manager empty-state hero: the spark inside orbit rings + a periodic
   ping (a quiet nod to the reachability probe). A single mood class re-tints and
   re-times everything: idle (calm) / active (working, faster) / error (frozen). */
.spark-stage { position: relative; display: grid; place-items: center; z-index: 1;
  --spark: var(--blue); --sparkRing: var(--bluePale); --sparkRing2: var(--blueL);
  --halo1: rgba(14,118,196,.13); --halo2: rgba(140,194,94,.07); }
.spark-stage.error { --spark: var(--red); --sparkRing: #ebcfcd; --sparkRing2: #d98c89;
  --halo1: rgba(198,63,62,.10); --halo2: rgba(198,63,62,.04); }
.spark-stage svg { display: block; overflow: visible; }
.spark-stage .ring { transform-box: fill-box; transform-origin: center; }
.spark-stage .ring-out { animation: ec-ringSpin 44s linear infinite; }
.spark-stage .ring-mid { animation: ec-ringSpin 30s linear infinite reverse; }
@keyframes ec-ringSpin { to { transform: rotate(360deg); } }
.spark-stage .signal { transform-box: fill-box; transform-origin: center; animation: ec-ping 3.6s cubic-bezier(.22,.61,.36,1) infinite; }
@keyframes ec-ping { 0% { transform: scale(.62); opacity: .55; } 70% { opacity: 0; } 100% { transform: scale(2); opacity: 0; } }
.spark-stage .spark-breathe { transform-box: fill-box; transform-origin: center; animation: ec-breath 4s ease-in-out infinite; }
@keyframes ec-breath { 0%, 100% { transform: scale(1); } 50% { transform: scale(1.06); } }
.spark-stage.active .ring-out { animation-duration: 9s; }
.spark-stage.active .ring-mid { animation-duration: 6s; }
.spark-stage.active .signal { animation-duration: 1.7s; }
.spark-stage.active .spark-breathe { animation-duration: 1.5s; }
.spark-stage.error .signal { display: none; }
.spark-stage.error .ring-out, .spark-stage.error .ring-mid { animation: none; }
.spark-stage.error .spark-breathe { animation: none; opacity: .78; }
@media (prefers-reduced-motion: reduce) {
  .ec-burst-sync, .ec-spark-spin, .ec-spark-hoverspin:hover, .ec-flow-ok, .ec-flow-deg, .ec-flow-draft, .ec-sweep, .ec-fade-in, .ec-tile-in, .ec-drop-pop, .ec-decor-breathe, .ec-decor-spin-slow, .ec-eq-bar, .ec-dot-live { animation: none; }
  .ec-pulse::after, .ec-dot-live::after { animation: none; opacity: 0; }
  .spark-stage .ring-out, .spark-stage .ring-mid, .spark-stage .signal, .spark-stage .spark-breathe { animation: none; }
}
`;
  document.head.appendChild(s);
}

// The 8-spoke "spark" geometry — the single source for both the Spark glyph
// and the SignalStage hero (each [x1,y1,x2,y2] in the 0..100 viewBox). Burst is
// NOT driven from this table: it is a deliberately distinct glyph (shorter
// spokes 12..88, a smaller un-rounded 14×14 centre rect, fixed strokeWidth 7),
// so it keeps its own hand-tuned coordinates below.
const SPARK_SPOKES: readonly [number, number, number, number][] = [
  [50, 9, 50, 33],
  [50, 67, 50, 91],
  [9, 50, 33, 50],
  [67, 50, 91, 50],
  [21, 21, 38.5, 38.5],
  [61.5, 61.5, 79, 79],
  [79, 21, 61.5, 38.5],
  [38.5, 61.5, 21, 79],
];

// The brand "burst" — a distinct brand glyph used in toolbars, status bars,
// and map nodes (its geometry differs from Spark/SPARK_SPOKES — see above — so
// it is intentionally not unified with that table). Spins (and tints sage via
// `color`) while a sync is in flight.
export const Burst = ({
  size = 18,
  color = T.blue,
  spin = false,
}: {
  size?: number;
  color?: string;
  spin?: boolean;
}) => (
  <svg
    width={size}
    height={size}
    viewBox="0 0 100 100"
    style={{ color }}
    className={spin ? 'ec-burst-sync' : undefined}
    aria-hidden="true"
  >
    <g stroke="currentColor" strokeWidth="7" strokeLinecap="round">
      <line x1="50" y1="12" x2="50" y2="34" />
      <line x1="50" y1="66" x2="50" y2="88" />
      <line x1="12" y1="50" x2="34" y2="50" />
      <line x1="66" y1="50" x2="88" y2="50" />
      <line x1="23" y1="23" x2="38" y2="38" />
      <line x1="62" y1="62" x2="77" y2="77" />
      <line x1="77" y1="23" x2="62" y2="38" />
      <line x1="38" y1="62" x2="23" y2="77" />
    </g>
    <rect x="43" y="43" width="14" height="14" transform="rotate(45 50 50)" fill="currentColor" />
  </svg>
);

// The brand "spark" — the redesign brand glyph (Lobby spine, live band, console
// rail). Eight spokes radiating from a 45°-rotated rounded square. Distinct from
// Burst (the connection-manager loader) so the existing screens are untouched.
// Pass className="ec-spark-spin" to rotate it (e.g. while the console reconnects).
export const Spark = ({
  size = 24,
  color = T.blue,
  stroke = 7,
  className,
}: {
  size?: number;
  color?: string;
  stroke?: number;
  className?: string;
}) => (
  <svg
    width={size}
    height={size}
    viewBox="0 0 100 100"
    className={className}
    aria-hidden="true"
    style={{ display: 'block', flexShrink: 0 }}
  >
    <g stroke={color} strokeWidth={stroke} strokeLinecap="round">
      {SPARK_SPOKES.map(([a, b, c, d], i) => (
        <line key={i} x1={a} y1={b} x2={c} y2={d} />
      ))}
    </g>
    <rect x="41" y="41" width="18" height="18" rx="2" transform="rotate(45 50 50)" fill={color} />
  </svg>
);

/**
 * The connection-manager empty-state hero: the brand spark centred in two slow
 * orbit rings + a soft halo, with a periodic ping radiating outward (a quiet nod
 * to the reachability probe). All colour/timing comes from CSS custom props, so
 * the `mood` class alone re-tints and re-times: idle (calm), active (working,
 * faster), error (frozen + dimmed, no ping). Motion is reduced-motion gated.
 */
export function SignalStage({ size = 224, mood = 'idle' }: { size?: number; mood?: SignalMood }) {
  // Momentum hover-spin: the central spark accelerates to full speed on hover
  // and COASTS to a stop on leave (instead of snapping back to 0), resting at
  // its final angle. Re-hovering mid-coast resumes the spin. Disabled for the
  // error mood (frozen) and under prefers-reduced-motion.
  const breatheRef = useRef<SVGGElement>(null);
  const hovering = useRef(false);
  const vel = useRef(0); // deg/s
  const angle = useRef(0); // deg
  const raf = useRef<number | null>(null);
  const last = useRef(0);

  useEffect(
    () => () => {
      if (raf.current != null) cancelAnimationFrame(raf.current);
    },
    [],
  );

  const reduced =
    typeof window !== 'undefined' &&
    typeof window.matchMedia === 'function' &&
    window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  const step = (ts: number) => {
    const g = breatheRef.current;
    const dt = last.current ? Math.min(0.064, (ts - last.current) / 1000) : 0.016;
    last.current = ts;
    const target = hovering.current ? 360 : 0; // deg/s (≈ one turn/s at full hover)
    // Exponential approach → smooth spin-up on hover AND smooth coast-down on leave.
    vel.current += (target - vel.current) * Math.min(1, dt * 5);
    angle.current += vel.current * dt;
    if (g) {
      g.style.animation = 'none'; // take over from the CSS breathe while live
      g.style.transform = `rotate(${angle.current}deg)`;
    }
    if (!hovering.current && vel.current < 1.5) {
      // Coasted to rest — stop, leaving the spark at its final angle (no snap).
      vel.current = 0;
      raf.current = null;
      last.current = 0;
      return;
    }
    raf.current = requestAnimationFrame(step);
  };

  const ensureLoop = () => {
    if (reduced || mood === 'error') return;
    if (raf.current == null) {
      last.current = 0;
      raf.current = requestAnimationFrame(step);
    }
  };

  return (
    <div
      className={`spark-stage ${mood}`}
      style={{ width: size, height: size }}
      onMouseEnter={() => {
        hovering.current = true;
        ensureLoop();
      }}
      onMouseLeave={() => {
        hovering.current = false;
        ensureLoop();
      }}
    >
      <svg viewBox="0 0 240 240" width={size} height={size} aria-hidden="true">
        <defs>
          <radialGradient id={`ec-halo-${mood}`} cx="50%" cy="50%" r="50%">
            <stop offset="0" stopColor="var(--halo1)" />
            <stop offset="44%" stopColor="var(--halo2)" />
            <stop offset="72%" stopColor="rgba(0,0,0,0)" />
          </radialGradient>
        </defs>
        <circle cx="120" cy="120" r="118" fill={`url(#ec-halo-${mood})`} />
        <circle
          className="ring ring-out"
          cx="120"
          cy="120"
          r="100"
          fill="none"
          stroke="var(--sparkRing)"
          strokeWidth="1.5"
          strokeDasharray="1.5 12"
        />
        <circle
          className="ring ring-mid"
          cx="120"
          cy="120"
          r="74"
          fill="none"
          stroke="var(--sparkRing2)"
          strokeWidth="1.5"
          strokeDasharray="1.5 9"
          opacity=".45"
        />
        <circle
          className="signal"
          cx="120"
          cy="120"
          r="52"
          fill="none"
          stroke="var(--sparkRing2)"
          strokeWidth="1.5"
        />
        <g className="spark-breathe" ref={breatheRef}>
          <g transform="translate(70,70)">
            <g stroke="var(--spark)" strokeWidth="7" strokeLinecap="round">
              {SPARK_SPOKES.map(([a, b, c, d], i) => (
                <line key={i} x1={a} y1={b} x2={c} y2={d} />
              ))}
            </g>
            <rect
              x="41"
              y="41"
              width="18"
              height="18"
              rx="2"
              transform="rotate(45 50 50)"
              fill="var(--spark)"
            />
          </g>
        </g>
      </svg>
    </div>
  );
}

/**
 * Brand decoration — the geometric motifs of the brand identity
 * (concentric arcs, dotted-grid circles, a green accent disc, vertical bars) in
 * brand blue/green. Rendered as a subtle, non-interactive background layer so
 * the surface feels on-brand while staying legible (low opacity, behind
 * content). Place inside a `position: relative` container.
 */
export function BrandDecor({
  animated = false,
  bars = true,
}: { animated?: boolean; bars?: boolean } = {}) {
  const barList = Array.from({ length: 16 });
  return (
    <div
      aria-hidden
      style={{ position: 'absolute', inset: 0, overflow: 'hidden', pointerEvents: 'none' }}
    >
      {/* concentric arcs, top-right */}
      <svg
        width="300"
        height="300"
        viewBox="0 0 300 300"
        style={{ position: 'absolute', top: -64, right: -56 }}
      >
        <g fill="none" stroke="var(--blue)" strokeWidth="1.5" opacity="0.13">
          {[44, 74, 104, 134, 164, 194].map((r) => (
            <circle key={r} cx="230" cy="70" r={r} />
          ))}
        </g>
      </svg>
      {/* solid green accent disc */}
      <svg width="150" height="150" style={{ position: 'absolute', top: 96, right: 188 }}>
        <circle
          cx="75"
          cy="75"
          r="54"
          fill="var(--green)"
          opacity="0.1"
          className={animated ? 'ec-decor-breathe' : undefined}
        />
      </svg>
      {/* dotted-grid circle, mid-right */}
      <svg
        width="170"
        height="170"
        viewBox="0 0 170 170"
        style={{ position: 'absolute', bottom: 150, right: -36, opacity: 0.55 }}
      >
        <defs>
          <pattern id="ec-decor-dots" width="11" height="11" patternUnits="userSpaceOnUse">
            <circle cx="2" cy="2" r="1.6" fill="var(--blue)" />
          </pattern>
          <clipPath id="ec-decor-clip">
            <circle cx="85" cy="85" r="80" />
          </clipPath>
        </defs>
        <rect
          width="170"
          height="170"
          fill="url(#ec-decor-dots)"
          clipPath="url(#ec-decor-clip)"
          opacity="0.45"
          className={animated ? 'ec-decor-spin-slow' : undefined}
        />
      </svg>
      {/* vertical bars, bottom-right (the "equalizer" motif). Suppressed on the
          not-connected hero (bars={false}): static bars there read as real
          (frozen) telemetry rather than ornament. */}
      {bars && (
        <svg
          width="220"
          height="130"
          style={{ position: 'absolute', bottom: -8, right: 24, opacity: 0.14 }}
        >
          {barList.map((_, i) => {
            const h = 24 + ((i * 7) % 5) * 18;
            return (
              <rect
                key={i}
                x={i * 13}
                y={130 - h}
                width="4"
                height={h}
                rx="2"
                fill="var(--blue)"
                className={animated ? 'ec-eq-bar' : undefined}
                style={animated ? { animationDelay: `${(i % 8) * 0.14}s` } : undefined}
              />
            );
          })}
        </svg>
      )}
    </div>
  );
}

/** Status dot paired with an accessible label — status is never color-only. */
export function StatusDot({ state, size = 8 }: { state: DotState; size?: number }) {
  const meta = statusMeta(state);
  return (
    <span
      role="img"
      aria-label={`Status: ${meta.label}`}
      style={{
        width: size,
        height: size,
        borderRadius: '50%',
        background: meta.color,
        display: 'inline-block',
        flexShrink: 0,
      }}
    />
  );
}

/** Map | List segmented control, shared by both views. */
export function ViewToggle({
  view,
  onSetView,
}: {
  view: 'map' | 'list';
  onSetView: (v: 'map' | 'list') => void;
}) {
  const item = (v: 'map' | 'list', label: string) => (
    <button
      className="ec-focus"
      onClick={() => onSetView(v)}
      aria-pressed={view === v}
      style={{
        ...noDrag,
        fontFamily: T.sans,
        fontSize: 12,
        fontWeight: 600,
        padding: '5px 12px',
        border: 'none',
        background: view === v ? T.bg : 'transparent',
        color: view === v ? T.blue : T.ink3,
        borderRadius: 6,
        cursor: 'pointer',
        boxShadow: view === v ? '0 1px 2px rgba(15,34,53,.12)' : undefined,
      }}
    >
      {label}
    </button>
  );
  return (
    <span
      style={{
        ...noDrag,
        display: 'inline-flex',
        gap: 2,
        background: T.soft,
        border: `1px solid ${T.line2}`,
        borderRadius: 8,
        padding: 2,
      }}
    >
      {item('map', 'Map')}
      {item('list', 'List')}
    </span>
  );
}

/**
 * The brand wordmark: the brand burst glyph + the PRODUCT name "AHV Remote
 * Console" ("Console" in brand blue). Single source so every screen agrees —
 * The glyph is the brand mark; the product is AHV Remote Console.
 * Module-private: only BrandHeader (below) renders it; not re-exported.
 */
function BrandWordmark({ size = 20 }: { size?: number }) {
  return (
    <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
      <Burst size={size} />
      <span style={{ fontFamily: T.sans, fontSize: 14, fontWeight: 700, color: T.ink }}>
        AHV Remote <span style={{ color: T.blue }}>Console</span>
      </span>
    </span>
  );
}

/**
 * The 50px brand header strip shared by every pre-lobby screen (add/edit
 * endpoint, sign-in, and the select-screen toolbar). Brand wordmark on the
 * left, optional controls passed as children on the right. Handles the
 * platform title-bar inset + drag region in one place.
 */
export function BrandHeader({ children }: { children?: React.ReactNode }) {
  return (
    <header
      style={
        {
          height: 50,
          flexShrink: 0,
          display: 'flex',
          alignItems: 'center',
          gap: 12,
          // macOS reserves 78px for the overlaid traffic lights; native-frame
          // platforms (Windows/Linux) keep the normal 16px.
          padding: `0 16px 0 ${titleBarLeft(16)}px`,
          background: T.soft,
          borderTop: `2px solid ${T.blue}`,
          borderBottom: `1px solid ${T.line}`,
          WebkitAppRegion: 'drag',
        } as React.CSSProperties
      }
    >
      <BrandWordmark />
      <span style={{ flex: 1 }} />
      {children}
    </header>
  );
}

/**
 * The select-screen toolbar: the shared brand header plus an optional middle
 * slot (e.g. the map's search box), the Map|List toggle, and the "+" add button.
 */
export function BrandToolbar({
  view,
  onSetView,
  onAddNew,
  middle,
}: {
  view: 'map' | 'list';
  onSetView?: (v: 'map' | 'list') => void;
  onAddNew: () => void;
  middle?: React.ReactNode;
}) {
  return (
    <BrandHeader>
      {middle}
      {onSetView && <ViewToggle view={view} onSetView={onSetView} />}
      <button
        className="ec-focus"
        onClick={onAddNew}
        aria-label="Add endpoint"
        title="Add endpoint"
        style={{
          ...noDrag,
          width: 32,
          height: 32,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          border: `1px solid ${T.line2}`,
          borderRadius: 8,
          background: T.bg,
          color: T.ink2,
          cursor: 'pointer',
        }}
      >
        <span style={{ fontSize: 18, lineHeight: 1 }}>+</span>
      </button>
    </BrandHeader>
  );
}
