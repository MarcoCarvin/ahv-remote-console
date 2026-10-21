# Console view

This page describes the live VM screen: the canvas that paints the guest
framebuffer and the layers that float over it. It is for designers and front-end
contributors who change how a session looks or behaves.

The view lives in
[`apps/desktop/src/renderer/components/ConsoleView.tsx`](../../apps/desktop/src/renderer/components/ConsoleView.tsx)
and its `console/` helpers. The scaling math is in
[`canvasGeometry.ts`](../../apps/desktop/src/renderer/components/canvasGeometry.ts).

## Purpose

The console gives the operator the guest screen with as little app chrome as
possible. The guest owns the window below a fixed toolbar band. Everything else
is an overlay that appears only when the session needs attention.

The console always renders in the dark theme. It reuses the flat brand colour
tokens (`--blue`, `--green`, `--amber`, `--red`, `--mono`) from
[`packages/ui/src/tokens.css`](../../packages/ui/src/tokens.css).

## Layout

```text
+--------------------------------------------------------------+
|==============================================================| ambient line
|  [ spark vm-name  * 12ms  bars      buttons ...        | x ] | top rail
|                                                              |
|   +------------------------------------------------------+   |
|   |                                                      |   |
|   |   <canvas>  flex-centred, letterboxed               |   |
|   |                                                      |   |
|   +------------------------------------------------------+   |
|                                                              |
|        connection overlay (connecting / off / error)         |
|        keyboard-lock banner (while armed)                    |
|        install notice (near the bottom)                      |
+--------------------------------------------------------------+
```

- The root is a relative container with a near-black background (`#05090f`).
  That colour shows only in the letterbox bars.
- The viewport container is absolute, starts 44 px from the top, and centres
  the canvas with flexbox. The 44 px band clears the top rail.
- The band is constant because the top rail is always shown. The guest never
  re-scales when the pointer moves.
- There are no side or bottom rails. Bars at the edges are letterbox only.
- The rail is described in [Console chrome](console-chrome.md).

The viewport container uses `contain: strict`. This stops the canvas size from
feeding back into the size the resize observer measures.

## Fit modes

The fit mode is stored per VM by `useFitMode`. `computeCanvasGeometry` turns the
framebuffer size, container size and mode into a CSS display size.

| Mode | Display size | Effect |
| --- | --- | --- |
| `fit` | Largest size that keeps the aspect ratio | Letterbox bars |
| `resize-guest` | Same as `fit` | AHV ignores `SetDesktopSize` |
| `pixel` | Framebuffer size, 1:1 | Container scrolls if larger |
| `stretch` | Container size | Aspect ratio ignored |

The app never asks the guest to change resolution. The canvas backing store
always equals the framebuffer resolution, and the GPU compositor scales it.

The 2D context is opaque and low-latency (`alpha: false, desynchronized: true`).
Painting is coalesced to animation frames. Small updates (up to 64 KB) paint at
once while the window is visible. More than 4096 queued rectangles collapse into
one full repaint.

## Focus, capture and pointer

- The canvas has `tabIndex=0`. Focus means capture: keys and mouse go to the
  guest. Blur releases all held modifiers and ends capture.
- A click focuses the canvas.
- The bare **Ctrl+Alt** chord blurs the canvas and releases capture.
- A pop-out window focuses the canvas on connect and on window focus. An
  embedded view never takes focus by itself.
- Mouse moves are coalesced to a 7 ms floor, about 140 Hz. This sits under the
  QEMU limit of about 150 events per second. Coordinates clamp to the
  framebuffer.
- Button down and up are sent at once. The wheel is forwarded. The context menu
  is suppressed so right-click reaches the guest.
- While captured, the app menu accelerators are suspended. A blur and
  visibility watchdog releases stuck modifiers.

Cursor policy: if the guest sends a cursor shape, it becomes a CSS cursor that
the OS draws. Otherwise the host arrow shows, with the guest's own software
cursor visible in the framebuffer. AHV standard VGA does not send cursor shapes,
so the arrow is the common case.

Keyboard handling is detailed in [Capture and input](capture-and-input.md).

## States

The view renders from the `ConsoleStatus` union in `console/status.ts`. Each
state drives the ambient line, the rail status dot and the overlay.

| State | What the operator sees |
| --- | --- |
| `connecting` (no frame yet) | Centred spinner and "Connecting to VM console…" |
| `connected` | Canvas only. Green dot with live RTT, such as `12ms` |
| `reconnecting` | Frozen last frame under a dimmed overlay with spinner |
| `starting` | Same visual as reconnecting, without the attempt count |
| `poweredOff` | Opaque overlay, "This VM is powered off", **Power on VM** |
| `error` | Cause-specific title and detail with action buttons |

The reconnecting overlay waits 900 ms before it appears. The rail dot changes at
once. Its copy is "Connection lost — reconnecting · attempt N of M…". The
attempt suffix appears only when both numbers are known. The `starting` reason
text is kept in state but not shown.

Error actions depend on the cause:

| Cause | Buttons |
| --- | --- |
| `session_expired` | **Sign in**, **Close** |
| Retryable (no cause, `unreachable`, `session_closed`, `frame_too_large`) | **Reconnect**, **Close** |
| `rbac_denied` and other causes | **Close** |

The overlay is always mounted. It fades with opacity over 240 ms and is
`aria-hidden` and click-through while hidden.

### Notices and toasts

- **Keyboard-lock banner.** A pill near the top, shown only while keyboard
  lock is armed. Its copy depends on the OS. There is no "click to control"
  hint and no "captured" pill.
- **Install notice.** A centred card near the bottom with an `err`, `warn`,
  `info` or `ok` colour. It can carry **Restart now**, **Later** and
  **Dismiss**. Success notices close after 6 seconds. Warnings and errors stay
  until the operator acts.
- **Agent install progress** shows in the console task bar as
  "Installing agent…".
- **Toasts** go through the shared `ToastProvider`. The console window places
  them top centre. They close after 6 seconds.
- **Fullscreen hint.** See [Console controls](console-controls.md).

## Keyboard and accessibility

| Keys | Action |
| --- | --- |
| Ctrl+Alt | Release keyboard and mouse capture |
| Ctrl+Cmd+F (macOS), F11 (others) | Toggle fullscreen |
| `?` (canvas not captured) | Open the shortcuts overlay |
| Cmd+K or Ctrl+K | Command palette (app-wide) |

There is no keyboard shortcut to pin or unpin the rail.

- Status is never shown by colour alone. The rail dot has
  `role="img"` and an `aria-label` such as "Status: 12ms", next to visible text.
- A visually hidden `role="status"` region announces connection changes. It is
  assertive for errors and polite otherwise.
- The install notice and the fullscreen hint use `role="status"`.
- Decorative layers are `aria-hidden`: the ambient line, signal bars, the
  hidden overlay and the lock banner while not armed.
- The canvas has no focus outline. The rail dot and the lock banner signal
  capture instead.
- Overlays use the literal glyph `⏻` for powered off. Rail icons come from the
  SVG `Icon` component.

## Look

| Element | Value |
| --- | --- |
| Root and letterbox | `#05090f` |
| Toolbar band | 44 px |
| Overlay, reconnect and error | `rgba(0,0,0,0.55)`, blur 4 px, mono 14 px |
| Overlay, powered off | `rgba(10,18,28,0.86)`, blur 4 px, sans 14 px |
| Overlay title | 15 px, weight 600 |
| Error title colour | `var(--hc-error-400, #ef4444)` |
| Primary overlay button | `var(--hc-accent-500)`, radius 8 |
| Install notice | Radius 8, sans 12 px, 6 s auto-close for `ok` |
| Lock banner | Pill, amber border, sans 11.5 px |
| Spinner | `hc-spin`, 1 s linear |

Layer order, bottom to top: canvas, connection overlay (50), lock banner (56),
ambient line (60), install notice (61), top rail (100), fullscreen hint (9999).

## Known limitations

- `--hc-error-400` and `--hc-ok-400` are not defined in `tokens.css`. The
  console paints their inline fallbacks, `#ef4444` and `#22c55e`. These are
  brighter than `--hc-error` and `--hc-success`.
- The `hc-spin` spinner and the fullscreen hint fade do not respect reduced
  motion. The `cm-*` and `ec-*` animations do. See
  [Console motion and visuals](console-motion-visuals.md).
- Of the console motion layers, only the signal bars are mounted in this view.

## Related documentation

- [Console chrome](console-chrome.md)
- [Console controls](console-controls.md)
- [Console overlays](console-overlays.md)
- [Console pop-out](console-popout.md)
- [Capture and input](capture-and-input.md)
- [Design system](design-system.md)
