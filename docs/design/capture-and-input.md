# Capture and input

This page covers the controls that send input to the guest VM: keyboard capture
and the lock banner, the fullscreen exit hint, the rail buttons and their menus,
the **Send Keys** menu, the guest keyboard layout picker and the display fit
model. All of them live in the console window over the full-bleed guest
framebuffer, in the dark theme.

| Part | Source |
| --- | --- |
| Rail items, canvas, fit plumbing | [`ConsoleView.tsx`](../../apps/desktop/src/renderer/components/ConsoleView.tsx) |
| Lock banner | [`console/CaptureIndicator.tsx`](../../apps/desktop/src/renderer/components/console/CaptureIndicator.tsx) |
| Rail button and tooltip | [`FloatingCluster.tsx`](../../apps/desktop/src/renderer/components/FloatingCluster.tsx) |
| Menu overlay | [`console/Overlay.tsx`](../../apps/desktop/src/renderer/components/console/Overlay.tsx) |
| Rail layout | [`console/ConsoleRails.tsx`](../../apps/desktop/src/renderer/components/console/ConsoleRails.tsx) |
| Fullscreen hook and hint | [`presence/FullscreenToggle.tsx`](../../apps/desktop/src/renderer/presence/FullscreenToggle.tsx) |
| Fit and layout hooks | [`presence/DisplayFitMenu.tsx`](../../apps/desktop/src/renderer/presence/DisplayFitMenu.tsx), [`presence/KeyboardLayoutMenu.tsx`](../../apps/desktop/src/renderer/presence/KeyboardLayoutMenu.tsx) |

## Capture

Capture is focus-based, not pointer lock. The canvas has `tabIndex={0}`. Click it
to give it focus, and keystrokes go to the VM. **Ctrl+Alt** releases focus.
Electron's renderer rejects pointer lock, so the app does not use it.

The canvas draws no focus ring, because a ring around a full-bleed guest view is
noise. It is the only control on this page that opts out of the global ring. Its
`aria-label` is `<vm> console — keyboard captured; press Ctrl+Alt to release`.

Opening a console shows no capture hint. The release chord is listed in the
shortcuts overlay instead.

## Keyboard lock banner

When keyboard lock is armed, the native grab sends host-reserved combos to the VM,
and a banner says so. The banner shows only when the native grab engages. It stays
hidden on Wayland, on macOS and when the native addon is missing. Lock implies
capture.

```text
        +--------------------------------------------------------+
        | o  Keyboard locked — host keys go to the VM · ...     |  amber pill
        +--------------------------------------------------------+
```

The text names the combos that stay with the host:

| Host | Banner text |
| --- | --- |
| Windows | Keyboard locked — host keys go to the VM · Ctrl+Alt or click away to release · Ctrl+Alt+Del and Win+L stay local (use Send Keys) |
| macOS | Keyboard locked — Ctrl+Alt or click away to release · Cmd+Tab, Cmd+Space and Mission Control stay on your Mac (use Send Keys / Option+Tab in the guest) |
| Other | Keyboard locked — host keys go to the VM · Ctrl+Alt or click away to release · Ctrl+Alt+Del and the VT switches stay local (use Send Keys) |

The banner is a top-centred pill on dark amber glass, `rgba(40,26,8,.9)`, with a
`rgba(242,166,90,.5)` border, an `#F2A65A` dot and white 11.5px Jost text. It
ignores the pointer and fades in and out over 200ms. While locked it has
`role="status"`. Otherwise it is `aria-hidden`.

## Fullscreen exit hint

Entering element fullscreen shows a short hint at the top centre: "Press ⌃⌘F to
exit fullscreen" on macOS, "Press F11 to exit fullscreen" elsewhere.

- It shows for 4 seconds. A keyframe holds full opacity for 70% of that time,
  then fades out. Leaving fullscreen hides it at once.
- It ignores the pointer, so it never blocks the guest's own controls. It has no
  dismiss action.
- It has `role="status"` and uses `--hc-bg-0`, a `--hc-bg-3` border and 12px mono
  text.
- **⌃⌘F** on macOS and **F11** elsewhere toggle fullscreen.

Native window fullscreen, from the macOS green button, shows no hint.

## Rail buttons

The top rail holds the input controls as `ClusterBtn` entries, in this order:

| Button | Icon | Action |
| --- | --- | --- |
| Send Keys | `keyboard` | Menu |
| Prerequisites & media | `disc` | Menu. Shown only when you may install prerequisites or read the image catalog |
| Guest keyboard layout: `<label>` | `globe` | Menu |
| Power | `power-dot` | Menu. `danger` tone, with a divider before it |
| Snapshots | `layers` | Toggles the snapshots panel |
| Live VM stats | `info` | Toggles the stats panel |
| Fullscreen | `fullscreen` | Toggles fullscreen. No toast |
| Close session | `close` | Rail chrome, see [`console-chrome.md`](console-chrome.md) |

Each button is 28 x 28px with radius 9 and a 16px icon at
`rgba(255,255,255,0.82)`. Hover and an open menu add a `rgba(255,255,255,0.08)`
fill. The `danger` tone turns the icon `#F2A65A`. The `primary` tone, a filled
`--blue` button, exists but no button uses it. Keyboard focus shows the global
ring. Each button sets `aria-label` and `title` to its title.

On hover, a small mono tooltip with the title appears below the button. It hides
while the button's menu is open.

### Menu overlay

A menu opens as a centred overlay card (`OverlayMenu`), not as an anchored
dropdown. A blurred dark backdrop covers the console.

- The card is 340px wide on `--hc-bg-0`, radius 12, with an uppercase title and a
  close button.
- Items are 12px mono buttons. `danger` items use `--hc-error`. Hover adds a
  light fill.
- Section headers are muted 9.5px uppercase labels with a divider above.
- Choosing an item runs it and closes the menu.
- **Esc** or a click on the backdrop closes the topmost overlay only.
- On open, focus moves into the card and away from the canvas, so keys stop
  reaching the guest. **Tab** is trapped inside. On close, focus returns to the
  button.
- The card is `role="dialog"` with `aria-modal="true"`. Items are plain buttons,
  reached with **Tab**. There is no `menu` role or roving focus.

## Send Keys menu

Send Keys delivers keys the host swallows, cannot type, or lacks. Each item sends
one pre-composed chord: all keys are pressed, then all are released. The guest
sees one combo. Each send raises a success toast, `Sent <label>`.

| Section | Items |
| --- | --- |
| System | Ctrl + Alt + Del · Task Manager — Ctrl+Shift+Esc |
| Symbols (Mac Option layer) | macOS hosts only: @ at · # hash · { brace left · } brace right · [ bracket left · ] bracket right · \\ backslash · \| pipe · ~ tilde · \` backtick · € euro |
| Windows | Win — Start · Ctrl + Esc — Start menu (no Win key) · Alt + Tab · Alt + Shift + Tab · Alt + Esc — Cycle windows · Alt + F4 — Close window |
| Single keys | PrintScreen · Insert · Pause / Break · Num Lock · Scroll Lock |
| Function keys | F1 to F10 · F11 — host fullscreen key · F12 |
| Linux virtual terminals | Ctrl + Alt + F1 to F6 · Ctrl + Alt + F7 — back to GUI · Ctrl + Alt + Backspace — restart X |

- The Mac symbols section exists because the Mac Option layer cannot pass through
  in scancode mode. Other hosts type these symbols directly.
- Win, Alt + Tab, Alt + Shift + Tab and Alt + Esc match the combos the native
  keyboard grab forwards. The menu is their fallback when the grab is unavailable.
- F11 is the host's own fullscreen key, so this menu is the only way to send it.
- Ctrl + Alt + Backspace is the only `danger` item. Its red colour comes with a
  label that names the effect.
- Labels are text only. Ordinary guest shortcuts are typed directly while the
  keyboard is captured.

The full key reference is in [`keyboard.md`](../reference/keyboard.md).

## Guest keyboard layout

The globe button's title shows the current layout, for example "Guest keyboard
layout: Italian". The choice is a declaration. It tells the input engine which
layout the guest uses, so typed characters turn into that layout's key sequences.
Picking a layout types nothing into the guest.

- The menu offers **US**, **UK** and **Italian**. The current layout has a
  leading `●`, the others a space. The engine knows more layouts, so an earlier
  pick of another layout keeps working.
- A pick is stored per VM in `localStorage` and toasts `Typing as a <label> guest
  layout`. With nothing stored, the default is Italian.
- **Cycle guest input language (Win+Space)** sends Super + Space, the only in-band
  way to change the guest's installed layout. It raises an info toast.

## Power menu

Items: Power ON · Restart (Guest) · Shutdown (Guest) · Force reset · Force off.
**Force reset** and **Force off** are `danger` items.

## Display fit

The console stores a fit mode per VM and uses it to size the canvas. The live
rail has no fit control, so the mode changes only through the stored value.

| Mode | Behaviour |
| --- | --- |
| `fit` | Scale to the container, keep aspect ratio |
| `pixel` | 1:1 pixels. The container scrolls |
| `stretch` | Fill the container, ignore aspect ratio |
| `resize-guest` | Looks like `fit`, because AHV does not support SetDesktopSize. The default |

The canvas is centred in a flex container. Its CSS size is recomputed when the
framebuffer size, the container size or the mode changes.

## Accessibility

- The lock banner and fullscreen hint are `role="status"`. Decorative dots are
  `aria-hidden`.
- All glyphs are SVG `Icon`s. There are no emoji.
- Danger items and states are named in text, never by colour alone.
- The layout picker marks the current choice with a `●` in the label text.
  Screen readers read the bullet, not an `aria-checked` state.

## Known limitations

- `DisplayFitMenu` and `KeyboardLayoutMenu` are built but not rendered. Only
  their hooks are live. The rail's `extra` slot that would host them is never
  passed. They offer a richer design, with a two-line fit list and all ten
  layouts. See [`console-controls.md`](console-controls.md).
- Under reduced motion the fullscreen hint does not fade. It stays visible until
  its 4-second timer hides it.

## Related documentation

- [Console chrome](console-chrome.md)
- [Console controls](console-controls.md)
- [Keyboard reference](../reference/keyboard.md)
- [Icon system](icons.md)
