# Console telemetry visuals and remote cursors

This page specifies three small visuals from the console motion layer: the
latency ring, the signal bars and the remote cursors. It is for designers and
front-end contributors who change how link health or shared-session presence
looks.

## Purpose

| Component       | What it shows                          | Mounted today |
| --------------- | -------------------------------------- | ------------- |
| `SignalBars`    | Link health as four bars in the top rail | Yes         |
| `LatencyRing`   | Round-trip time as a pulsing dot       | No            |
| `RemoteCursors` | Other operators' pointers in a shared session | No     |

`LatencyRing` and `RemoteCursors` are exported and styled, but no component
mounts them yet. `LatencyRing` is meant for the titlebar, next to the
millisecond readout. `RemoteCursors` waits for the shared-session feature, and
renders nothing when it has no peers, so it can be mounted unconditionally.

## Layout

```text
+- console window ----------------------------------------------+
| ( spark  vm-name  (o) connected  ._||  [buttons]      [x] )    |  top rail
|=============== ambient line =================================|
| +- canvas wrapper -----------------------------------------+ |
| |                                                          | |
| |        > Maria            > Devon                        | |  remote cursors
| |                                                          | |  inset 0, no
| |                 guest framebuffer                        | |  pointer events
| +----------------------------------------------------------+ |
+--------------------------------------------------------------+

  titlebar:   12 ms  (o)    <- latency ring beside the ms text
```

The signal bars and latency ring sit in the chrome, never over guest pixels.
The remote cursors cover the canvas but pass all input through.

## Shared foundations

The motion layer has its own palette, scoped under `.cm-root`, so the `--cm-*`
variables never leak into the rest of the app. The colours are brighter than
the global `--hc-*` status tokens because they must read on top of any guest
picture.

Every animation uses only `transform` and `opacity`. It runs on the compositor
and never forces layout or paint of the guest pixels. Animated elements set
`will-change` on the animated properties only.

Reduced motion is handled twice:

1. `useConsoleMotion` reads the app motion setting and turns off every animated
   layer flag, so callers can skip the work.
2. A `@media (prefers-reduced-motion: reduce)` block in the stylesheet freezes
   every animation at a static end state, in case an element still renders.

## Latency ring

A 6 px dot inside a 10 px box, with a 1 px ring that expands and fades. The
colour signals health and the pulse speed tracks round-trip time.

| RTT              | Colour               |
| ---------------- | -------------------- |
| Below 50 ms      | `--cm-green`         |
| 50 to 150 ms     | `--cm-warn` (amber)  |
| Above 150 ms     | `--cm-danger` (red)  |
| Unknown or `NaN` | red                  |

The pulse duration equals the RTT clamped to 800–3000 ms. A non-finite RTT uses
3000 ms. Higher latency therefore gives a slower pulse. The pulse keyframe
scales from 0.4 to 1.9 while opacity falls from 0.7 to 0, and holds empty for
the last 20 % of the cycle.

The ring is relevant only while the status is `connected`. Under reduced motion
the pulse becomes a static outline at 0.35 opacity, and the hook flag turns off.

## Signal bars

Four bars of rising height, 4, 7, 10 and 12 px, with a 2 px gap. Lit bars wave
in sequence. Unlit bars stay dim.

| Condition       | Lit bars | Colour |
| --------------- | -------- | ------ |
| Not connected   | 0        | amber  |
| Below 50 ms     | 4        | green  |
| 50 to 150 ms    | 3        | amber  |
| 151 to 300 ms   | 2        | amber  |
| Above 300 ms    | 1        | red    |

All bars share one colour. The lit count tells three bars from two within the
amber band. When not connected, no bar is lit, so the amber is barely visible.

The wave runs for 1.4 s, between 0.5 and 1 opacity and 0.85 and 1 vertical
scale, from the bottom edge. Each bar starts 0.12 s after the previous one.
Under reduced motion lit bars are static at 0.9 opacity and unlit at 0.3. Count
and colour still carry the health.

The bars render in the top rail's status group, after the VM name and status
text, while the status is `connected` or `reconnecting`.

## Remote cursors

One arrow and one name label per peer. A peer has `id`, `name`, `color`, `x`
and `y`. Coordinates use canvas-wrapper pixels, the same space the console
maps its own mouse into.

- The arrow is a 16 px SVG filled with the peer colour and outlined in
  `#0a0f17`, so it reads on light and dark guest pictures.
- The label sits 14 px below and 10 px right of the tip. It has the peer colour
  as background and white 10 px text.
- Each peer moves by a new `translate3d`, and CSS glides it there over 120 ms,
  linear.
- A new label fades in over 200 ms to 0.9 opacity.
- Peers are keyed by `id`, so adding or removing one mounts or unmounts only
  that peer.

The layer is relevant whenever the link is live. Under reduced motion the
cursors stay visible but jump instead of gliding, because presence is
information. The label shows at rest without the fade.

## States and data

`useConsoleMotion` derives a state and per-layer flags from the console status,
capture, fullscreen and backgrounded signals.

| Flag            | True when                                    |
| --------------- | -------------------------------------------- |
| `latencyRing`   | Status is `connected`                        |
| `signalBars`    | Status is `connected` or `reconnecting`      |
| `remoteCursors` | The link is live, including degraded or idle |

A connected link counts as degraded above 150 ms (`DEGRADED_RTT_MS`). The ring
and bar colours change at the same point.

## Accessibility

- The ring and the bars are `aria-hidden`. The numeric RTT and the status dot
  with its label carry the meaning.
- Colour is never the only channel. The ring also changes speed and the bars
  change count.
- The remote cursor layer is `aria-hidden` and has `pointer-events: none`, so
  it never takes input from the console.
- None of these components takes focus or responds to the keyboard.

## Values that define the look

| Element                 | Value                                       |
| ----------------------- | ------------------------------------------- |
| `--cm-blue`             | `#1f6fe5`                                   |
| `--cm-blue-light`       | `#4f93f0`                                   |
| `--cm-green`            | `#33b55a`                                   |
| `--cm-green-light`      | `#7fd398`                                   |
| `--cm-warn`             | `#e0911f`                                   |
| `--cm-danger`           | `#e5484d`                                   |
| `--cm-e-std`            | `cubic-bezier(0.2, 0, 0, 1)`, loops         |
| `--cm-e-dec`            | `cubic-bezier(0.05, 0.7, 0.1, 1)`, entries  |
| `--cm-e-acc`            | `cubic-bezier(0.3, 0, 0.8, 0.15)`, exits    |
| Ring                    | 10 px box, 6 px dot, pulse 800–3000 ms      |
| Bars                    | 3 px wide, radius 1, opacity 0.25 and 0.85  |
| Peer arrow              | 16 px, outline `#0a0f17` at 0.75 width      |
| Peer label              | 10 px sans, padding 1px 5px, radius 4       |
| Peer glide              | 120 ms linear                               |

Source: [`apps/desktop/src/renderer/components/console-motion/`](../../apps/desktop/src/renderer/components/console-motion/),
mainly `LatencyRing.tsx`, `SignalBars.tsx`, `RemoteCursors.tsx`, `signal.ts`,
`useConsoleMotion.ts` and `consoleMotion.css`.

## Known limitations

- `LatencyRing` and `RemoteCursors` are not mounted anywhere yet.
- Peer presence is visual only. Screen readers do not announce peers.
- A comment in `SignalBars.tsx` calls the disconnected colour neutral, but the
  code returns `--cm-warn`, which is amber.

## Related documentation

- [Console chrome](console-chrome.md)
- [Console overlays and states](console-overlays.md)
- [Console view](console-view.md)
- [Motion](motion.md)
