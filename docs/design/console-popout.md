# Console pop-out window

A console pop-out is a separate native window that shows one VM session. This
page covers the window frame, its open-time fade, and the two screens it shows
before the live console takes over. It is for designers and front-end
contributors.

## Purpose

The main process opens one pop-out per session. The window loads the same
renderer bundle as the main window, with
`?mode=console&session=<id>&vm=<name>`, so it mounts `ConsoleWindowApp`
instead of the lobby.

`ConsoleWindowApp` shows exactly one of these, in priority order:

1. The **error screen**, when the runtime is unavailable or boot fails.
2. The **connecting screen**, while the WebSocket or SDK client is not ready.
3. The live console, `<ConsoleView popout active />`.

The live console and its rails are described in [Console view](console-view.md)
and [Console chrome](console-chrome.md). This page stops at that handoff.

## Window frame

```text
+- native title bar: "<vm name> — AHV Remote Console" ----[-][o][x]-+
|  macOS: inset traffic lights. Windows and Linux: default frame,   |
|  menu bar auto-hidden.                                            |
+-------------------------------------------------------------------+
|                                                                   |
|        ConsoleWindowApp: error | connecting | ConsoleView         |
|                                                                   |
|              default 1280 x 800, minimum 640 x 400                |
+-------------------------------------------------------------------+
```

| Property          | Value                                         |
| ----------------- | --------------------------------------------- |
| Initial size      | 1280 x 800                                    |
| Minimum size      | 640 x 400, no maximum                         |
| Frame             | Native frame with minimise, maximise, close   |
| Title bar style   | `hiddenInset` on macOS, `default` elsewhere   |
| Menu bar          | `autoHideMenuBar: true`                       |
| Window background | `#0f1b2a`, the same as `--hc-bg-0`            |
| Icon              | `ahv-remote-console-512.png`                  |
| Title             | `<vm name> — AHV Remote Console`              |

Design decisions:

- The in-window menu bar is hidden because the console has its own top rail.
  Hiding it also stops a resize flicker on Windows, where releasing keyboard
  capture would otherwise bring the menu row back and reflow the console.
  macOS keeps its menu in the system bar.
- There is one window per session. Opening the same session again shows and
  focuses the existing window.
- The window is a trusted app window with navigation guards and the shared
  hardened web preferences. The renderer is sandboxed, and the preload script
  mediates all privileged IPC.
- Native fullscreen, from the green button, F11 or the toggle IPC, is sent to
  the renderer on `ahv-remote-console:window-fullscreen`. The console then
  drops its rail insets and goes full-bleed. The state is re-sent after a
  reload.

## Connecting screen

```text
+--------------------- 100vh, background #000 ----------------------+
|                                                                   |
|                    Connecting to <vm name>…                       |
|                                                                   |
+-------------------------------------------------------------------+
```

A single centred line in `--hc-fg-2`, with inherited font and size. It has no
spinner, icon or controls. It shows on first boot until both the client and
the stream exist. It shows again briefly after a runtime restart, while the
client and stream are rebuilt on a new loopback port.

## Error screen

```text
+--------------- 100vh, background #000, padding 24 ----------------+
|                                                                   |
|                   Can’t open this VM console                      |
|                                                                   |
|    The local helper that connects to your VMs isn’t responding.   |
|    Click Try again in a moment. If it keeps happening, close      |
|    this window and open the VM again from the main app.           |
|                                                                   |
|                   [ Try again ]   [ Close ]                       |
+-------------------------------------------------------------------+
```

This screen appears when the runtime state machine reports `error` or when
boot throws. It means the renderer never reached a runtime. Typed errors inside
a live console, such as `session_expired`, use the overlay described in
[Console overlays and states](console-overlays.md).

The copy avoids technical terms. "The local helper" is the runtime.

- **Try again** is the primary button. It reloads the renderer and reruns boot.
- **Close** is the secondary button. It closes the window. The main process
  then forgets the window and clears its capture flag.

Both are the shared `OverlayButton`, with the same recipe as the in-console
overlay buttons.

## Transitions and motion

- Screens swap by plain conditional render, with no animation.
- The only motion is a native window fade on first load. The window is created
  hidden. On `did-finish-load` the main process sets opacity to 0, shows the
  window and steps opacity to 1 over 200 ms in 16 ms ticks.
- The fade is linear. It replaces a shared-element transition from the lobby
  tile, which cannot cross native windows. It also hides the seam between the
  `#0f1b2a` window background and the `#000` page.
- `did-finish-load` is used instead of `ready-to-show`, because on macOS a
  hidden window can stall `ready-to-show` for a long time.
- Status toasts dock top-centre, 52 px from the top, because the bottom edge
  holds the console rail and guest taskbar. A reconnect shows no toast, because
  the overlay already reports it.

## Session lifecycle

The window closes its runtime session on React unmount and on `beforeunload`.
It marks the close as deliberate first, so self-healing does not re-dial during
teardown.

## Keyboard and accessibility

- The two buttons are native `<button type="button">` elements. Tab order is
  **Try again**, then **Close**.
- Fullscreen toggles with Ctrl+Cmd+F on macOS and F11 elsewhere. See
  [Console controls](console-controls.md).
- Both screens state their meaning in text, not by colour alone.
- Neither screen uses icons or emoji.

## Values that define the look

| Element               | Value                                         |
| --------------------- | --------------------------------------------- |
| Screen background     | `#000`, to match guest letterboxing           |
| Connecting text       | `--hc-fg-2`                                   |
| Error column          | Flex column, centred, gap 8, padding 24       |
| Error title           | 16 px, weight 600, `--hc-error`               |
| Error detail          | 13 px, line height 1.5, opacity 0.9, max 480  |
| Button row            | Gap 10, margin top 8                          |
| Primary button        | `--hc-accent-500` fill and border, white text |
| Secondary button      | Transparent, `rgba(255,255,255,0.25)` border  |
| Buttons               | 8px 18px padding, radius 8, sans 13 px / 600  |
| Font                  | `--hc-font-sans` throughout, no mono          |
| Elevation             | None. No borders, shadows or blur             |
| Window fade           | 200 ms, linear, 16 ms steps                   |

Source: [`ConsoleWindowApp.tsx`](../../apps/desktop/src/renderer/app/ConsoleWindowApp.tsx),
[`main.ts`](../../apps/desktop/src/main/main.ts) (`createConsoleWindow`),
[`ConsoleOverlays.tsx`](../../apps/desktop/src/renderer/components/console/ConsoleOverlays.tsx),
[`tokens.css`](../../packages/ui/src/tokens.css).

## Known limitations

- The window fade has no `prefers-reduced-motion` check. It always runs.
- The fade duration matches the `--dur-short4` token but is a literal value.
- Neither screen sets `role` or `aria-live`. The error text is not announced,
  and the connecting text has no `role="status"`.
- No focus is set when either screen mounts.
- The buttons have no hover, active or custom focus styles. They rely on the
  browser default focus outline. The 8 px radius is not a radius token.

## Related documentation

- [Console view](console-view.md)
- [Console chrome](console-chrome.md)
- [Console overlays and states](console-overlays.md)
- [App shell](app-shell.md)
- [Motion](motion.md)
