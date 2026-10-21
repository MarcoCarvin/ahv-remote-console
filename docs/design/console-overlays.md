# Console overlays and states

This page describes the layers that appear over the VM picture in a console
window while it connects, reconnects, waits for a powered-off VM or shows an
error. It is for designers and front-end contributors who change these states.

## Purpose

The console must always tell the operator what the connection is doing. Three
surfaces share that job:

- The **connecting placeholder**, shown before the first framebuffer arrives.
- The **connection-state overlay**, one always-mounted layer that fades in over
  the frozen last frame for reconnecting, starting, powered-off and error states.
- The **ambient line** and the **status chip** in the top rail, which report the
  same status continuously, so an overlay is never the only signal.

The connection-state overlay is one persistent element whose opacity changes.
It is not mounted and unmounted. A flapping connection therefore shows a calm
notice instead of a blank or white flash.

## Layout

```text
+----------------------------------------------------------+
|==================== ambient line (3 px) =================| z 60
|  [ spark  vm-name  (o) 12ms        [#][<>] | [x] ]       | top rail, z 100
|                                                          |
|            . . . . . . . . . . . . . . . . .             |
|            .   dim + blur over last frame  .             | overlay, z 50
|            .            (spinner)          .             | inset 0
|            .  Connection lost - reconnecting...          |
|            . . . . . . . . . . . . . . . . .             |
|                                                          |
|                 [ install notice ]                       | z 61
+----------------------------------------------------------+
```

The overlay fills the console root and centres a single vertical stack. It sits
below the ambient line and the top rail, so the status indicators stay readable
over the dimmed overlay.

## The status model

Every state is derived from one union, `ConsoleStatus`:

| `kind`         | Meaning                                  | What renders                       |
| -------------- | ---------------------------------------- | ---------------------------------- |
| `connecting`   | First dial, no framebuffer yet           | Placeholder, no backdrop           |
| `connected`    | Stream live                              | No overlay                         |
| `reconnecting` | Drop or realign                          | Reconnecting overlay, after 900 ms |
| `starting`     | VM is on, console not serving yet        | Reconnecting overlay, at once      |
| `poweredOff`   | VM reported off                          | Powered-off overlay                |
| `error`        | Terminal fault, with an optional `cause` | Error panel                        |

`ConsoleErrorCause` is one of `session_expired`, `rbac_denied`, `unreachable`,
`vm_not_found`, `config_error`, `session_closed` or `frame_too_large`.
`isRetryableCause` returns true for `unreachable`, `session_closed`,
`frame_too_large` and an undefined cause. Only those get a **Reconnect** button.

## States

### Connecting

A 24 px spinning ring and the label "Connecting to VM console…" in the muted
foreground colour. There is no backdrop because there is nothing to dim. As soon
as pixels or a `connected` frame arrive, the canvas replaces the placeholder.

### Reconnecting and starting

A 20 px spinner and the text "Connection lost — reconnecting…". When the attempt
count is known, the text adds " · attempt N of M". The overlay appears only
after `reconnecting` has lasted 900 ms, so a short flap never shows it. The
status chip changes immediately.

A booting VM (`starting`) reuses this visual without the delay and without the
attempt counter. The fixed reason "VM is starting…" is stored in state, but the
rendered line is the reconnecting text.

### Powered off

A deliberately calmer treatment, because power-off is expected. It uses a
darker, more opaque backdrop, the sans font and a wider gap. It shows the power
glyph, the title "This VM is powered off", the line "The console reconnects
automatically as soon as the VM is powered on." and a primary **Power on VM**
button. The button calls the VM power action and shows progress toasts. The
glyph is a Unicode character, not an SVG from the icon set.

### Error

The overlay hosts `ErrorOverlayBody`: a red title, a centred detail line and a
button row. **Close** is always present, so the operator is never stuck.

| `cause`                      | Title                             | Buttons             |
| ---------------------------- | --------------------------------- | ------------------- |
| `session_expired`            | Please sign in again              | Sign in, Close      |
| `rbac_denied`                | You don't have access to this VM  | Close               |
| `vm_not_found`               | This VM is no longer available    | Close               |
| `config_error`               | Can't connect securely to this VM | Close               |
| `session_closed`             | Connection to this VM dropped     | Reconnect, Close    |
| `frame_too_large`            | Display hiccup                    | Reconnect, Close    |
| `unreachable` or unknown     | Lost connection to the VM         | Reconnect, Close    |

- **Sign in** runs the embedded SSO flow and reconnects in place.
- **Reconnect** clears the give-up latch and sets the status back to
  `connecting`.
- **Close** closes the window when the console is a pop-out.

## Transitions

- The overlay fades with `opacity` over 240 ms. Its content is cleared 280 ms
  after hiding, so text and buttons stay visible through the fade.
- Powered-off, starting and reconnecting clear themselves. A later `connected`
  or `resumed` frame, or fresh pixels, sets the status to `connected`.
- Only `error` is terminal and needs a button.

Watchdogs turn a stuck state into an error:

| Condition                                           | Result                     |
| --------------------------------------------------- | -------------------------- |
| `connecting` with no connect or close for 45 s      | `error`, VM did not respond |
| `reconnecting` with no control frame for 60 s       | `error`, could not reconnect |
| 90 s since leaving `connected` without a recovery   | `error`, give-up latch set |

A sleep gap longer than 15 s resets the 90 s baseline instead of firing.

## Ambient line and status chip

The 3 px ambient line runs across the top of the console. Its gradient follows
the status, and it shows an indeterminate sweep while connecting, starting,
reconnecting or in error.

| Status                         | Line colours       | Chip colour | Chip text       |
| ------------------------------ | ------------------ | ----------- | --------------- |
| `connected`                    | blue, light green  | light green | `{latency}ms`   |
| `connected`, latency > 150 ms  | blue, amber        | light green | `{latency}ms`   |
| `connecting`, `starting`       | blue, light blue   | amber       | status word     |
| `reconnecting`                 | red                | amber       | `reconnecting`  |
| `poweredOff`                   | amber              | amber       | `powered off`   |
| `error`                        | red                | red         | `disconnected`  |

The chip always pairs a 6 px dot with a text label. Status is never shown by
colour alone. The brand spark in the top rail spins while the console connects.

## Keyboard and accessibility

- While the overlay is visible it takes pointer events, so its buttons are
  clickable. When faded out it passes clicks through to the guest.
- The faded overlay is `aria-hidden`. A visually hidden `role="status"` region
  announces connection changes, `assertive` for errors and `polite` otherwise.
- The canvas auto-focus stays inactive while an overlay is up, so overlay
  buttons remain reachable with Tab.
- Buttons are native `<button type="button">` elements.
- The ambient line is decorative and `aria-hidden`. The chip dot has
  `role="img"` and an `aria-label` such as "Status: reconnecting".

## Values that define the look

| Element                 | Value                                         |
| ----------------------- | --------------------------------------------- |
| Default backdrop        | `rgba(0,0,0,0.55)` with `blur(4px)`           |
| Powered-off backdrop    | `rgba(10,18,28,0.86)` with `blur(4px)`        |
| Overlay text            | `#fff`, 14 px                                 |
| Reconnecting and error  | `--hc-font-mono`, gap 8 px                    |
| Powered-off             | `--hc-font-sans`, gap 14 px, glyph 40 px      |
| Titles                  | 15 px, weight 600                             |
| Detail text             | 12.5 px, line height 1.5, max width 380–400   |
| Error title colour      | `var(--hc-error-400, #ef4444)`                |
| Primary button          | `--hc-accent-500` fill and border             |
| Secondary button        | transparent, `rgba(255,255,255,0.25)` border  |
| Buttons                 | 8px 18px padding, radius 8, sans 13 px / 600  |
| Spinner                 | `hc-spin`, 1 s linear, infinite               |
| Overlay elevation       | blur and dim only, no shadow                  |

Source: [`ConsoleView.tsx`](../../apps/desktop/src/renderer/components/ConsoleView.tsx),
[`ConsoleOverlays.tsx`](../../apps/desktop/src/renderer/components/console/ConsoleOverlays.tsx),
[`ConsoleRails.tsx`](../../apps/desktop/src/renderer/components/console/ConsoleRails.tsx),
[`status.ts`](../../apps/desktop/src/renderer/components/console/status.ts).

## Known limitations

- `--hc-error-400` is not defined in `packages/ui/src/tokens.css`, so the error
  title always uses the fallback `#ef4444`. This differs from the brand
  `--hc-error`.
- Most overlay colours are literals, not tokens. The 8 px button radius is not a
  `--hc-radius-*` token.
- Overlay buttons have no hover, active or custom focus styles. They rely on the
  browser default focus outline.
- The `hc-spin` spinners are not disabled under `prefers-reduced-motion`. The
  ambient sweep and spark spin are.
- The overlay container has no `role`. The connecting placeholder has none
  either, and the live region announces "Connecting to {vm name}" instead of
  the placeholder text.

## Related documentation

- [Console view](console-view.md)
- [Console chrome](console-chrome.md)
- [Console controls](console-controls.md)
- [Motion](motion.md)
