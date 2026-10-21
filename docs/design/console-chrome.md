# Console chrome

This page describes the two pieces of app chrome that sit over the guest screen:
the ambient latency line and the top rail. It is for designers and front-end
contributors who change the console toolbar.

Source:
[`apps/desktop/src/renderer/components/console/ConsoleRails.tsx`](../../apps/desktop/src/renderer/components/console/ConsoleRails.tsx)
(`AmbientLine`, `TopRail`). Buttons come from
[`FloatingCluster.tsx`](../../apps/desktop/src/renderer/components/FloatingCluster.tsx)
(`ClusterBtn`).

## Purpose

The guest framebuffer should own the window. The chrome gives the operator VM
identity, connection health and commands without covering guest pixels.

- **Ambient line.** A 3 px gradient strip on the top edge. Its colour tracks
  connection state and round-trip time.
- **Top rail.** A glass bar under the line with the VM name, status, signal
  bars and the command buttons.

There is no bottom rail. The bottom edge belongs to the guest, so its taskbar
stays reachable.

## Layout

```text
+------------------------------------------------------------------+
|################# 3 px ambient line ##############################|
|  +------------------------------------------------------------+  |
|  | spark vm-name  * 12ms  bars    [kbd][disc][globe] | [pwr]  |  |
|  |                  [snap][stats][fs] | [close]               |  |
|  +------------------------------------------------------------+  |
|                                                                  |
|                  guest framebuffer                               |
+------------------------------------------------------------------+
```

The rail is one row; the sketch wraps it to fit. It is a flex row with two
groups:

1. **Identity group.** Spark glyph, VM name, status dot, status text and,
   when enabled, signal bars.
2. **Button group.** Pushed right with `margin-left: auto`. Command buttons,
   an optional `extra` slot, a divider, then **Close session**.

The rail spans the window with a 10 px inset on each side and a fixed 34 px
height. It does not wrap or scroll; the buttons must fit. On macOS the left
padding is 78 px so the content clears the traffic lights. Elsewhere it is
12 px (`titleBarLeft` in `platform.ts`).

## Components

### Command buttons

Buttons render in this order. A divider is inserted before every button with
the danger tone.

| Button | Icon | Notes |
| --- | --- | --- |
| **Send Keys** | `keyboard` | Sectioned menu of key chords |
| **Prerequisites & media** | `disc` | Present only when the agent ISO or catalog applies |
| **Guest keyboard layout** | `globe` | US, UK, Italian, plus Win+Space cycle |
| **Power** | `power-dot` | Danger tone. Power on, restart, shutdown, force reset, force off |
| **Snapshots** | `layers` | Toggles the snapshots panel |
| **Live VM stats** | `info` | Toggles the stats panel |
| **Fullscreen** | `fullscreen` | Toggles fullscreen |
| **Close session** | `close` | Always last, after a divider |

Buttons the operator may not use are left out, not disabled. The `extra` slot
is not used by the console.

### ClusterBtn

Each button is a 28 by 28 px `<button>` with a 16 px icon and radius 9.

- A button with a `menu` toggles that menu on click.
- A button without a menu runs its `onClick`.
- A tooltip with the title shows below the button on hover, unless its menu is
  open.

Menus open as a centred `OverlayMenu` card, not as an anchored dropdown. The
card closes on a backdrop click or Esc. Its styles are in
[Capture and input](capture-and-input.md).

## States

### Ambient line

| State | Gradient | Sweep |
| --- | --- | --- |
| Connected, RTT up to 150 ms | blue to green | No |
| Connected, RTT over 150 ms | blue to amber | No |
| Connecting or starting | blue to light blue | Yes |
| Reconnecting or error | red to `#e8765f` | Yes |
| Powered off | flat amber | No |

### Status dot and text

The status text always travels with the dot colour.

| State | Colour | Text |
| --- | --- | --- |
| `connected` | `--greenL` | RTT, such as `12ms` |
| `connecting` | `--amber` | `connecting` |
| `reconnecting` | `--amber` | `reconnecting` |
| `starting` | `--amber` | `starting` |
| `poweredOff` | `--amber` | `powered off` |
| `error` | `--red` | `disconnected` |

The Spark glyph spins slowly while connecting, reconnecting or starting.

### Signal bars

| RTT | Colour | Lit bars |
| --- | --- | --- |
| Under 50 ms | `--cm-green` | 4 |
| 50 to 150 ms | `--cm-warn` | 3 |
| 150 to 300 ms | `--cm-warn` | 2 |
| Over 300 ms | `--cm-danger` | 1 |
| Not connected | `--cm-warn` | 0 |

### Rail visibility

The rail is always shown. `useChromeReveal` starts pinned, so its hover reveal,
1.5 s auto-hide and top-edge zone are inert. `togglePin` exists but nothing
calls it. When the hook is unpinned, the rail fades and slides 8 px over
160 ms.

### Button states

| State | Background | Icon |
| --- | --- | --- |
| Rest | transparent | `rgba(255,255,255,0.82)` |
| Hover or menu open | `rgba(255,255,255,0.08)` | `rgba(255,255,255,0.82)` |
| Danger tone | transparent | `#F2A65A` |
| Primary tone (unused) | `--blue` | white |

The rail background is the window drag region. The button group opts out with
`no-drag`, so clicks reach the buttons.

## Keyboard and accessibility

- Rail buttons are real `<button>` elements with `aria-label` and `title`.
  They use the global `:focus-visible` ring.
- Menu cards trap Tab while topmost. Esc closes the topmost card.
- The status dot has `role="img"` and an `aria-label` such as
  "Status: connecting".
- The ambient line, dividers, signal bars, Spark glyph and icon SVGs are
  `aria-hidden`.
- There is no keyboard shortcut to pin or show the rail. Cmd+K or Ctrl+K opens
  the app-wide command palette. The fullscreen keys are in
  [Console controls](console-controls.md).
- All icons are SVG. There are no emoji in the rail.

## Look

| Element | Value |
| --- | --- |
| Ambient line | 3 px, opacity 0.9, z-index 60 |
| Rail position | top 7 px, inset 10 px, height 34 px, z-index 100 |
| Rail surface | `rgba(11,18,28,.8)`, blur 14 px |
| Rail border | `1px solid rgba(74,163,220,.22)`, no bottom border |
| Rail radius | `6px 6px 0 0` |
| Group gaps | 12 px between groups, 8 px identity, 4 px buttons |
| Divider | 1 by 18 px, `rgba(255,255,255,.16)` |
| VM name | mono 13 px, weight 600, white |
| Status text | mono 11 px, status colour |
| Spark glyph | 17 px, `--blueL` |
| Tooltip | mono 11 px, `rgba(15,34,53,0.95)`, radius 4 |
| Sweep | 1.4 s linear loop |
| Spark spin | 8 s linear loop |

All rail text is JetBrains Mono. Surface colours are literal `rgba()` values,
not theme tokens. The console only renders dark, so this is consistent.

## Known limitations

- A code comment describes a 40 px hit area, but the button is 28 by 28 px with
  no padding.
- The 160 ms rail transition is not gated by reduced motion. The sweep, spin
  and signal-bar wave are.

## Related documentation

- [Console view](console-view.md)
- [Console controls](console-controls.md)
- [Console motion and visuals](console-motion-visuals.md)
- [Capture and input](capture-and-input.md)
- [Icons](icons.md)
