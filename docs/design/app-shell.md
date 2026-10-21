# Main window and app shell

This page describes the container around the main-window screens: the native
window, the runtime gate shown before the app is ready, the route shell that swaps
screens, and the global toasts and command palette. The screens themselves have
their own pages.

## Files

| Layer | File |
| --- | --- |
| Native windows | [`apps/desktop/src/main/main.ts`](../../apps/desktop/src/main/main.ts) |
| Renderer entry and mode split | [`main.tsx`](../../apps/desktop/src/renderer/main.tsx) |
| Runtime gate and global chrome | [`app/App.tsx`](../../apps/desktop/src/renderer/app/App.tsx) |
| Route shell | [`app/Shell.tsx`](../../apps/desktop/src/renderer/app/Shell.tsx) |
| Route order and direction | [`app/routeReducer.ts`](../../apps/desktop/src/renderer/app/routeReducer.ts) |
| Title bar inset helper | [`platform.ts`](../../apps/desktop/src/renderer/platform.ts) |

The shell draws no header, sidebar or tab bar of its own. Each screen draws its
own draggable header.

## Layout

```text
macOS main window, 1440 x 900 (min 960 x 600)
+--------------------------------------------------------------+
| o o o  <- traffic lights over the content                    |
|        each screen leaves 78px on the left and draws its     |
|        own drag strip                                        |
|                                                              |
|   one routed screen at a time:                               |
|   splash | add-endpoint | endpoint (map/list) | signin | lobby|
|                                                              |
|   [command palette, mod+k]                   [toasts]        |
+--------------------------------------------------------------+
```

On Windows and Linux the OS draws a normal title bar above the content, so
screens need no inset.

## Native window

The main window is 1440 x 900 with a minimum of 960 x 600. Its background is
`#0f1b2a`, the same as `--hc-bg-0`. It has a native frame. macOS uses
`titleBarStyle: 'hiddenInset'`, other platforms use `'default'`. There is no
custom title bar overlay.

The window starts hidden and shows on `did-finish-load`, not `ready-to-show`. On
macOS a hidden window's first frame can stall for a long time, so the app shows it
early. You see the dark background colour until React paints, never white.

`titleBarLeft(base)` returns 78px on macOS and `base` elsewhere. Screens use it for
their header padding. Interactive controls inside a drag strip use the `noDrag`
style from `clusterTokens.ts` so they stay clickable.

All app windows share hardened web preferences: context isolation, sandbox, no
Node integration, web security on, no `<webview>`, no spellcheck, and no
background throttling. The renderer never holds the IPC secret.

## Console windows

Opening a VM never changes the main window. The main process opens a separate
window with `?mode=console&session=<id>&vm=<name>`. It is 1280 x 800 (min 640 x 400)
on the same dark background, with the menu bar hidden and the title
`<vm> — AHV Remote Console`. Opening the same session again focuses the existing
window. Closing one window ends only that session. The window fades in from
opacity 0 to 1 over about 200ms. See [`console-popout.md`](console-popout.md).

## Renderer entry

`main.tsx` reads `?mode`:

- `mode=console` mounts `ConsoleWindowApp` only, with no sign-in.
- Anything else mounts `MotionProvider` around `App`.

Both branches sit in `StrictMode`, `LazyMotion` and `MotionConfig
reducedMotion="user"`. Only the main window gets `MotionProvider`.

`main.tsx` also loads the reset, the tokens, the bundled fonts and the two motion
stylesheets. It fades out the static boot splash from `index.html` after React
has painted. The splash is described in [`design-system.md`](design-system.md).

## Runtime gate

`App` shows one of three full-window states from a `phase` of `starting`, `ready`
or `error`. It gets the phase from the runtime supervisor. It subscribes before it
reads the current state so it misses no change. All three states use the dark
canvas.

| Phase | What you see |
| --- | --- |
| `starting` | A spinning glyph (`aria-hidden`) and "Starting runtime…" in `--hc-fg-2` |
| `error`, never booted | "AHV Remote Console cannot reach its local runtime." in `--hc-error`, a detail line, a **Retry** button in `--hc-accent-500`, and a hint about tray diagnostics |
| `ready` | The shell, with toasts and the command palette |

**Retry** restarts the runtime and returns to `starting`. While it works, the button
is disabled at opacity 0.6 and reads "Restarting…". After a first successful boot,
later errors keep the live UI instead of the fatal screen.

On `ready`, `App` builds the SDK client once per loopback endpoint. It rebuilds it
when the runtime restarts on a new port.

## Route shell

`Shell` keeps an `AppStage` and maps it to a route.

| AppStage | Route | Screen |
| --- | --- | --- |
| `loading` | `splash` | Spinner "Loading endpoints..." |
| `no-endpoints`, `add-endpoint` | same | `AddEndpointPanel` |
| `select-endpoint` | `endpoint` | `ClusterMap` (default) or `EndpointPicker` |
| `sign-in` | `signin` | `SignInPanel` |
| `ready` | `lobby` | `Lobby` |

In `select-endpoint`, a map/list toggle switches between `ClusterMap` and
`EndpointPicker`, the accessible list. Both share the selected endpoint.

Route order is `splash`, `no-endpoints`, `add-endpoint`, `endpoint`, `signin`,
`lobby`, `console`. A move to a later route counts as forward, otherwise back.
The `console` route exists in the enum, but the main window never renders it.

## Transitions

One `AnimatePresence mode="wait"` inside a `LayoutGroup` holds the routed screen,
so only one screen is on screen at a time. `splash` to `signin` uses a
fade-through. Every other change uses a 24px horizontal shared-axis slide in the
inferred direction. Enter takes 300ms and exit 150 to 200ms. Values and the
reduced-motion rules are in [`motion.md`](motion.md).

## Global chrome

- **Toasts.** `ToastProvider placement="bottom-right"`. See
  [`shared-ui.md`](shared-ui.md).
- **Command palette.** `mod+k` toggles it. It has two commands:
  **Export diagnostics bundle** saves a diagnostics bundle and toasts the path.
  **Reload UI** reloads the window.

## Theme

The gate screens use the dark `--hc-*` palette. No code sets `data-theme`, so the
dark `:root` block is always active. The connection screens and the lobby paint
the light tool-grade tokens directly. `Shell.tsx` also has a small local colour
map `C` for its loading fallback that mixes light literals with dark tokens.

## Accessibility

- The global `:focus-visible` ring applies on keyboard focus.
- Gate states pair colour with text. The fatal screen has a red headline and a
  detail line. The starting state has a label next to the spinner.
- Decorative glyphs are `aria-hidden`.
- The shell does not move focus on a route change. `mode="wait"` keeps one screen
  mounted, so focus stays inside the active screen.

## Known limitations

- `Shell` accepts an `onOpenPalette` prop that it does not use.
- The `console` route value is defined but never rendered in the main window.

## Related documentation

- [Design overview](overview.md)
- [Console window](console-popout.md)
- [Motion and animation](motion.md)
- [Shared UI primitives](shared-ui.md)
