# Motion and animation

This page describes the motion system of the renderer: the shared timing tokens,
the CSS animation layers, the `data-motion-*` gates, the route transitions and the
reduced-motion rules. It also lists which parts are live and which are authored
but not used.

## Where motion lives

| Concern | File |
| --- | --- |
| JS tokens for Framer Motion | [`motion/tokens.ts`](../../apps/desktop/src/renderer/motion/tokens.ts) |
| CSS token mirror | [`packages/ui/src/tokens.css`](../../packages/ui/src/tokens.css) |
| Legacy CSS layer, gated | [`motion/motion.css`](../../apps/desktop/src/renderer/motion/motion.css) |
| Atomic CSS layer, always on | [`motion/atomic.css`](../../apps/desktop/src/renderer/motion/atomic.css) |
| Console overlay layer | [`components/console-motion/consoleMotion.css`](../../apps/desktop/src/renderer/components/console-motion/consoleMotion.css) |
| Layer gates | [`motion/MotionContext.tsx`](../../apps/desktop/src/renderer/motion/MotionContext.tsx) |
| Route variants | [`motion/variants.ts`](../../apps/desktop/src/renderer/motion/variants.ts) |
| Route transition host | [`app/Shell.tsx`](../../apps/desktop/src/renderer/app/Shell.tsx) |
| Console motion state | [`console-motion/useConsoleMotion.ts`](../../apps/desktop/src/renderer/components/console-motion/useConsoleMotion.ts) |

`main.tsx` imports `motion.css` and `atomic.css` for both window modes. Each
console-motion component imports `consoleMotion.css` itself.

## Tokens

Durations and eases exist twice, in seconds for Framer Motion and in
milliseconds for CSS. Keep them in step by hand.

| JS token | CSS token | Value | Use |
| --- | --- | --- | --- |
| `duration.short3` | `--dur-short3` | 0.15s / 150ms | Crossfade |
| `duration.short4` | `--dur-short4` | 0.2s / 200ms | Exit |
| `duration.medium2` | `--dur-medium2` | 0.3s / 300ms | Enter |
| `duration.medium4` | `--dur-medium4` | 0.4s / 400ms | Shared element |
| `ease.standard` | `--ease-standard` | `[0.2, 0, 0, 1]` | Default |
| `ease.decel` | `--ease-decel` | `[0.05, 0.7, 0.1, 1]` | Entering |
| `ease.accel` | `--ease-accel` | `[0.3, 0, 0.8, 0.15]` | Exiting |
| `containerSpring` | none | spring, stiffness 380, damping 38 | Tile to viewport morph (not used) |

The legacy set (`--hc-motion-fast 120ms`, `--hc-motion-base 220ms`,
`--hc-motion-slow 360ms` and two eases) stays for back-compat. New work uses
`--dur-*` and `--ease-*`. The console overlay layer redeclares the three curves
locally as `--cm-e-std`, `--cm-e-dec` and `--cm-e-acc`.

## Route transitions

This is the main motion you can see today. `Shell` wraps the routed screen in one
`<AnimatePresence mode="wait">` inside a `<LayoutGroup>`. The child is an `m.div`
keyed on the route. `mode="wait"` lets the old screen finish leaving before the
new one mounts, so two screens never overlap.

Routes are `splash`, `no-endpoints`, `add-endpoint`, `endpoint`, `signin` and
`lobby`. `routeReducer.ts` orders them and infers `forward` or `back` for each
change.

| Pair | Variant | Enter | Exit |
| --- | --- | --- | --- |
| `splash` to `signin` | T1 fade-through | opacity 0 to 1, y 8 to 0, 300ms decel | opacity to 0, 150ms accel |
| All others | T2/T3 shared axis X | opacity 0 to 1, x ±24 to 0, 300ms decel | opacity to 0, x ∓24, 200ms accel |

Forward navigation slides in from the right and leaves to the left. Back reverses
both.

`main.tsx` wraps both window modes in `LazyMotion features={domAnimation} strict`
(about 15 KB, no drag or pan) and `MotionConfig reducedMotion="user"`. `strict`
requires the `m.*` namespace. The only other `AnimatePresence` in the renderer is
the tasks bar in `ConsoleTaskBar.tsx`. The shared toast and button components in
`packages/ui` also use Framer Motion; see [`shared-ui.md`](shared-ui.md).

## Layer gates (`data-motion-*`)

`MotionProvider` keeps one on/off flag per layer and writes
`data-motion-<layer>="on|off"` on the root element. CSS in `motion.css` selects
`[data-motion-XX="on"] .className`.

| Layer | Default | Gates |
| --- | --- | --- |
| A1 | on | Staggered entrance (`.entrance-*`, `.deco-*`) |
| B3 | on | Live-dot pulse (`.live-dot`, 2.2s) |
| C1 | on | Decorative drift (`.deco-*`, 14 to 22s loops) |
| D1 | on | Card hover lift (`.epcard:hover`, -2px, 0.2s) |
| D3 | on | Focus glow (`.epcard:focus-visible`, also applies with the gate off) |
| D2, E1-E4 | on | Ripple, connect orbit, success check, page exit, error shake. These classes are not gated |
| B1, B2 | on | No selector |
| B4, B5, C2, C3 | off | No selector |

`MotionProvider` wraps only the main window. In a console window, `useMotion()`
returns the default context, with `enabled` true.

## CSS layers

The atomic layer in `atomic.css` holds always-on classes that animate only
transform, opacity and background position: `.live-pulse`, `.shim` (skeleton
sweep), `.card-lift`, `.ripple-host`, `.shake-active` and `.check-svg`. They use the
`--dur-*` and `--ease-*` tokens.

The legacy layer in `motion.css` holds the gated classes in the table above.

> [!NOTE]
> No `.tsx` file in the renderer uses the class names of either layer today. The
> stylesheets load and the attributes are written, but nothing rendered matches
> them. Treat both layers as an available vocabulary, not live behaviour.

## Console overlay motion (C1 to C12)

`consoleMotion.css` defines twelve overlay layers that sit on the live
framebuffer and animate only on the compositor. All tokens are scoped under
`.cm-root`. The layer has its own palette: `--cm-blue #1f6fe5`,
`--cm-green #33b55a`, `--cm-warn #e0911f` and `--cm-danger #e5484d`.

| ID | Draws | Rendered today |
| --- | --- | --- |
| C1 | Breathing capture glow at the canvas edge | No |
| C2 | Latency dot and ping ring | Built (`LatencyRing`), not mounted |
| C3 | Data particles | No |
| C4 | Four signal bars with a staggered wave, 1.4s | Yes, in the top rail |
| C5, C6 | Hand-off ring, keystroke ripple | No |
| C7 | Remote cursors with name labels | Built (`RemoteCursors`), not mounted |
| C8 | Amber reconnect scanline | No |
| C9-C12 | Clipboard flash, flip-in, idle tilt, background fade | No |

`useConsoleMotion` derives a state from live signals: `connecting`,
`capturedLive`, `releasedLive`, `degraded` (RTT over 150ms), `reconnecting`,
`poweredOff`, `idle` or `closed`. It then sets per-layer flags. `ConsoleView` passes
only `signalBars` on to the rail. `signal.ts` maps latency to colour (green under
50ms, amber 50 to 150ms, red above), to a bar count of 4, 3, 2 or 1, and to a
ping cadence clamped to 800 to 3000ms.

The rail's spinning brand glyph during (re)connect uses the connection manager's
`ec-spark-spin` class (8s per turn), not a console-motion layer.

## Reduced motion

Three mechanisms apply. Each backs up the others.

1. Framer Motion `reducedMotion="user"` swaps animate targets to their initial
   values. This covers the route transitions and the shared toasts and buttons.
2. `MotionProvider` listens to `prefers-reduced-motion` and turns every layer
   off. `useConsoleMotion` then zeroes most console flags. This works in the main
   window only.
3. CSS media queries:
   - `motion.css` sets every animation and transition to 0.01ms with one
     iteration on all elements. This also overrides inline styles.
   - `consoleMotion.css` freezes each C layer at a static end state. Bars keep
     their count and colour, and the capture glow stays visible.
   - `tokens.css` zeroes the legacy duration tokens.

The rule the console gate follows: information that motion carried must survive
the freeze.

## Accessibility

- Console motion overlays are `aria-hidden`. They never enter the accessibility
  tree.
- Status is never colour-only. The rail status dot has `role="img"`, an
  `aria-label` and a text label. Signal bars encode health by count.
- Focus styles do not depend on motion. The `.epcard` focus glow applies with its
  gate off, and the connection screens use `.ec-focus`, a 2px `--blue` outline.
- `AnimatePresence mode="wait"` keeps exactly one route mounted.
- All three reduced-motion paths follow the OS setting. There is no in-app toggle.

## Known limitations

- `containerSpring` and `reducedMotionVariants` are exported but never imported.
- The tile-to-console morph (T4) is not wired. `layoutId` appears only in
  comments. A console opens in a separate OS window, so no single DOM tree spans
  the tile and the viewport. The console window fades in from the main process
  instead; see [`console-popout.md`](console-popout.md).
- The legacy and atomic CSS classes have no consumers.
- Eleven of the twelve console motion flags drive nothing.

## Related documentation

- [Design system and tokens](design-system.md)
- [Console motion visuals](console-motion-visuals.md)
- [App shell](app-shell.md)
