# Console controls and shortcuts

This page covers four controls in
[`apps/desktop/src/renderer/presence/`](../../apps/desktop/src/renderer/presence/)
that operators use during a live session, and the feature-flag hook that gates
console features. It is for designers and front-end contributors. All controls
use the dark theme and JetBrains Mono (`var(--mono)`).

| Control | File | Exports |
| --- | --- | --- |
| Display fit menu | `DisplayFitMenu.tsx` | `useFitMode`, `DisplayFitMenu` |
| Keyboard layout menu | `KeyboardLayoutMenu.tsx` | `useKeyboardLayout`, `KeyboardLayoutMenu` |
| Fullscreen toggle | `FullscreenToggle.tsx` | `useFullscreen`, `useWindowFullscreen`, `FullscreenToggle` |
| Shortcuts overlay | `ShortcutsOverlay.tsx` | `useShortcutsOverlay`, `ShortcutsOverlay` |
| Feature flags | `useFeatureFlags.ts` | `useFeatureFlags` |

> [!NOTE]
> `ConsoleView` uses the hooks for fit mode, keyboard layout and fullscreen,
> and renders the fullscreen hint and the shortcuts overlay. The
> `DisplayFitMenu`, `KeyboardLayoutMenu` and `FullscreenToggle` components are
> complete but not placed in the toolbar. The rail has its own fullscreen and
> layout buttons, described in [Console chrome](console-chrome.md).

## Display fit menu

The menu picks how the guest framebuffer scales into the canvas.

| Value | Label | Description |
| --- | --- | --- |
| `fit` | Fit | Scale to fill, maintain aspect ratio |
| `pixel` | 1 : 1 | Native pixels, scroll if needed |
| `stretch` | Stretch | Fill container, ignore aspect ratio |
| `resize-guest` | Resize guest | Same as Fit (guest resize unsupported on AHV) |

`useFitMode(vmId)` stores the choice in `localStorage` under
`ahv-console-fit-<vmId>`. With no stored value the default is `resize-guest`,
which looks the same as `fit`. Invalid values and storage errors fall back to
the default.

```text
 trigger              panel (anchored below, right-aligned)
+-------------+      +--------------------------------------+
| [] Fit  v   |      | | check Fit                          |
+-------------+      | |   Scale to fill, maintain aspect   |
                     |   1 : 1                              |
                     |     Native pixels, scroll if needed  |
                     |   Stretch / Resize guest ...         |
                     +--------------------------------------+
```

The trigger shows a monitor icon, the current label and a caret. Its tooltip is
`Display fit: <label>`. Each option row shows the label over a smaller
description. The selected row has a darker background, a 2 px accent bar on
the left, bold text and a `✓`.

## Keyboard layout menu

The menu declares which keyboard layout the guest uses. It offers ten layouts:
US, UK, Italian, German, French, Spanish, Portuguese BR, Swiss German, Swiss
French and Japanese.

`useKeyboardLayout(vmId)` stores the choice under `ahv-console-kbd-<vmId>`. The
default is Italian (`it`), to match the input engine's default physical layout.
An unknown stored id falls back to US when the label is looked up.

The trigger matches the fit menu without the leading icon. Its tooltip is
`Keyboard layout: <label>`. Rows are single-line and the panel is slightly
narrower.

## Shared menu behaviour

Both menus use the same popover logic.

- Clicking the trigger toggles the panel.
- Clicking a row selects it and closes the panel.
- A `mousedown` outside the menu closes it. Esc closes it.
- These listeners exist only while the panel is open.
- The panel appears and disappears without animation. Only hover colours fade.

## Fullscreen toggle

The button is a 28 by 28 px icon button. It shows an expand icon when windowed
and a collapse icon in fullscreen. Its `title` and `aria-label` include the
shortcut, for example `Enter fullscreen (⌃⌘F)` or `Exit fullscreen (F11)`.

`useFullscreen` works like this:

1. It tracks fullscreen from the DOM `fullscreenchange` event, whatever caused
   it.
2. `toggleFullscreen` calls `window.ahvRemoteConsole.toggleFullscreen()` over
   the preload bridge. It does not call the DOM Fullscreen API.
3. A capture-phase `keydown` listener on `window` toggles fullscreen. On macOS
   the keys are Ctrl+Cmd+F (`metaKey && ctrlKey`, no Alt, code `KeyF`).
   Elsewhere the key is F11.

`useWindowFullscreen` tracks native window fullscreen, such as the macOS green
button. That path does not fire `fullscreenchange`, so the main process
forwards it over the bridge with `onWindowFullscreen`.

### Exit hint

On entering fullscreen a hint appears at the top centre for 4 seconds. It
reads `Press ⌃⌘F to exit fullscreen` on macOS and `Press F11 to exit
fullscreen` elsewhere. It holds full opacity for 70% of the time, then fades
out. It is click-through (`pointer-events: none`) and has `role="status"`.

## Shortcuts overlay

A centred modal that lists the console shortcuts.

```text
+----------------------------------------+
| Keyboard shortcuts                   x |
+----------------------------------------+
| CONSOLE                                |
| Release keyboard / mouse   [Ctrl][Alt] |
| Toggle fullscreen          [⌃][⌘][F]  |
| Show this shortcut reference      [?]  |
| NAVIGATION                             |
| Open command palette          [⌘][K]  |
| CLUSTER MAP                            |
| Search clusters               [⌘][F]  |
+----------------------------------------+
|       Press [?] or [Esc] to close      |
+----------------------------------------+
```

Keys shown per platform:

| Action | macOS | Windows and Linux |
| --- | --- | --- |
| Release keyboard and mouse capture | Ctrl+Alt | Ctrl+Alt |
| Toggle fullscreen | Ctrl+Cmd+F | F11 |
| Show this shortcut reference | `?` | `?` |
| Open command palette | Cmd+K | Ctrl+K |
| Search clusters | Cmd+F | Ctrl+F |

The cluster map also focuses its search on a bare `/`. The overlay does not
list it. There is no global hotkey.

Behaviour:

- A bare `?` toggles the overlay. It works only while the console canvas is not
  captured, so `?` still reaches the guest during capture.
- Any held Cmd, Ctrl or Alt cancels the trigger.
- Esc, a backdrop click or the `×` button closes it.
- It appears without animation.

## Feature flags

`useFeatureFlags(client)` fetches a map of boolean flags from the runtime with
`client.getFeatureFlags()`. It returns `{}` until loaded and when the call
fails, so gated features stay off. It does nothing when `client` is `null` and
refetches when the client changes. `ConsoleView` reads `qemu_ext_key_event`
and `clipboard_large_paste`.

## Accessibility

| Control | Support |
| --- | --- |
| Fit and layout triggers | Native `title` tooltip |
| Fit and layout rows | Real `<button>` elements, Esc and click-outside close |
| Fullscreen button | `aria-label` and `title` with the shortcut |
| Fullscreen hint | `role="status"` |
| Shortcuts overlay | `role="dialog"`, `aria-label="Keyboard shortcuts"`, close button labelled "Close" |

## Look

| Element | Value |
| --- | --- |
| Trigger | Transparent, `1px solid var(--hc-bg-3)`, radius 6, mono 11 px |
| Trigger hover | Text white, border `var(--hc-fg-2)`, 120 ms |
| Menu panel | `var(--hc-bg-0)`, radius 8, shadow `0 8px 24px rgba(0,0,0,0.5)` |
| Selected row | `var(--hc-bg-2)`, 2 px `var(--hc-accent-500)` left bar |
| Row hover | `var(--hc-bg-1)`, 80 ms |
| Fullscreen hint | `var(--hc-bg-0)`, radius 8, mono 12 px, top 24 px |
| Overlay backdrop | `rgba(10,9,7,0.7)`, blur 4 px |
| Overlay panel | `#252219`, width 480 px, radius 12 |
| Overlay section header | Accent, 10 px uppercase |
| Key chip | `#2e2a23`, border `#4a4538`, height 22 px, radius 4 |

The shortcuts overlay uses warm charcoal literals, unlike the cool blue tokens
of the menus.

## Known limitations

- The fit and layout menus have no `role="menu"` or `role="listbox"`, no
  `aria-expanded` or `aria-haspopup`, and no focus management in the panel.
- The shortcuts dialog does not trap focus, move focus into the panel, or
  restore it on close.
- No transition here respects reduced motion. The durations are literals, not
  motion tokens.
- `FullscreenToggle` declares a `green` colour that it does not use.

## Related documentation

- [Console chrome](console-chrome.md)
- [Console view](console-view.md)
- [Capture and input](capture-and-input.md)
- [Keyboard reference](../reference/keyboard.md)
- [Design system](design-system.md)
