# Shared UI primitives

This page covers the UI kit in [`packages/ui/src`](../../packages/ui/src) and the
few bespoke notice and button surfaces the renderer draws itself. Read it before
you add a toast, button, card or form field. Token values are in
[`design-system.md`](design-system.md).

The kit is dark-themed (`--hc-*`). The light connection screens do not use the
kit buttons. They draw flat buttons with `T.*` tokens instead.

## Toasts

There is one toast system, `ToastProvider` and `useToast` in
[`Toast.tsx`](../../packages/ui/src/components/Toast.tsx). Both the main window and
console windows use it.

```text
            +--------------------------------------+
            | [✓]  Title                       ✕  |
            |      Optional body text              |
            |======================               |  <- progress bar
            +--------------------------------------+
```

- Kinds are `info`, `success`, `warn` and `error`. Call `push(kind, title, body?)`.
- `useToast()` throws outside a provider.
- Placement is a prop. The main window uses `bottom-right`, where nothing else
  sits. Console windows use `top-center`, below the top rail.
- New toasts stack below older ones in a column.

| Kind | Chip colour | Chip glyph | Role |
| --- | --- | --- | --- |
| success | `--hc-success` | `✓` | `status` |
| warn | `--hc-warning` | `!` | `status` |
| error | `--hc-error` | `✕` | `alert` |
| info | `--hc-info` | `i` | `status` |

The card is glass: `--hc-bg-glass` with a 14px backdrop blur, a `--hc-border-1`
border, radius `--hc-radius-lg` and `--hc-shadow-deep`. The kind chip is a tinted
square at 18% of the kind colour, not a full-card tint.

Behaviour:

- A toast dismisses itself after 6 seconds. A 2px progress bar in the kind colour
  shrinks over that time.
- Hovering pauses the timer and the bar, and the border turns
  `--hc-border-strong`. Leaving resumes with the remaining time.
- An identical toast pushed right after the same one is dropped.
- The close button removes the toast at once.
- Toasts enter with a spring (stiffness 380, damping 30, mass 0.8) from 16px
  above or below and scale 0.96. They exit in 160ms.

`ConsoleView` has no toast stack of its own. Its `toast(text, kind)` helper maps
`ok`, `err` and `info` to `success`, `error` and `info` on the shared provider.

## Console notices

The console draws one bespoke notice over the canvas: the install and
serial-prerequisite notice. It sits centred near the bottom of the viewport, has
`role="status"` and comes in four kinds: `ok`, `info`, `warn` and `err`.

- A non-sticky notice disappears after 6 seconds. A sticky notice stays until you
  dismiss it.
- A notice that needs a power cycle shows **Restart now** and **Later**. Restart
  now is the only trigger of the power cycle. Other sticky notices show
  **Dismiss**.
- While an ISO mount is running, the notice is hidden and the tasks bar shows
  "Installing agent…" instead.

> [!NOTE]
> The notice uses hard-coded dark backgrounds and fallback border colours
> (`--hc-error-400`, `--hc-ok-400`) that `tokens.css` does not define. It
> therefore shows `#ef4444`, `#f59e0b`, `#1668a5` and `#22c55e`, not the brand
> status tokens.

The console shows no clipboard-failure notice. A clipboard failure only logs a
console warning.

## Buttons

### Kit `Button`

[`Button.tsx`](../../packages/ui/src/components/Button.tsx) is an `m.button` with
`variant`, `size`, `loading`, `iconLeft` and `iconRight`.

| Variant | Look |
| --- | --- |
| `primary` (default) | `--hc-accent-500` fill, white text, blue shadow |
| `secondary` | `--hc-bg-elev` fill, `--hc-border-1` border |
| `ghost` | Transparent, `--hc-fg-1` text |
| `danger` | `--hc-error` fill, white text |

Sizes are `sm`, `md` (default) and `lg`. `sm` uses `--hc-radius-sm`, the others
`--hc-radius-md`.

States: hover lifts by 1px, press scales to 0.97, and disabled or loading sets
opacity 0.6 and turns both motions off. Loading puts a 14px spinner in the leading
slot. Focus uses the global ring.

### `IconButton`

A square `m.button`, 32px by default, radius `--hc-radius-md`, `--hc-fg-1` icon.
Hover fills `--hc-bg-elev`. Press scales to 0.92. The `label` prop is required and
becomes both `aria-label` and `title`.

### Light-screen buttons

The connection screens draw flat buttons inline and share the `.ec-focus` class, a
2px `--blue` outline. Sources: [`EndpointPicker.tsx`](../../apps/desktop/src/renderer/components/EndpointPicker.tsx),
[`AddEndpointPanel.tsx`](../../apps/desktop/src/renderer/components/AddEndpointPanel.tsx),
[`clusterShared.tsx`](../../apps/desktop/src/renderer/components/clusterShared.tsx).

| Button | Look |
| --- | --- |
| Primary | `T.blue` fill, white 13px semibold text, radius 9, 42px tall |
| Secondary | White fill, `T.line2` border, `T.ink2` text |
| Toolbar add | 32px square, `T.line2` border, a text `+` |
| Form submit | `T.blue` fill. While saving it reads "Saving…" at opacity 0.7 |
| Map / List toggle | Segmented control. The selected item is white with `T.blue` text and `aria-pressed=true` |

## Cards and surfaces

| Component | Look |
| --- | --- |
| `Card` | `--hc-bg-1`, `--hc-border-0`, radius `lg`, `--hc-shadow-soft`. `glass` switches to `--hc-bg-glass` with blur. `elevated` uses `--hc-shadow-deep`. No padding of its own |
| `Surface` | Background only. `level` 0 to 3 maps to `--hc-bg-0` to `--hc-bg-3`. Default 1 |
| Add/Edit card (light) | 480px wide, white, radius 16, `T.line2` border, soft shadow |

## Supporting primitives

| Component | Purpose and behaviour |
| --- | --- |
| `Field` | Labelled input on `--hc-bg-2`. An error turns the border `--hc-error` and replaces the hint with the error text |
| `Spinner` | Ring in `--hc-accent-400`, 0.9s per turn. `role="status"`, `aria-label` defaults to "Loading" |
| `StatusDot` | `ok`, `warn`, `error`, `idle`, `pending` mapped to status tokens. Optional pulse ring and text label |
| `Tooltip` | Opens on hover and on focus. Sets `aria-describedby` while open. Fades and slides 4px in 0.18s |
| `Kbd` | Key cap in the mono face on `--hc-bg-3` |
| `useResizeObserver` | Returns a ref and the rounded content size. The console uses it to size the canvas |
| `CommandPalette` | Opened with `mod+k` in the main window. See [`app-shell.md`](app-shell.md) |

## Motion

Kit components animate with Framer Motion. `MotionConfig reducedMotion="user"`
turns those animations off when the OS asks for reduced motion. The toast progress
bar is a CSS animation, and the toast stylesheet hides it under reduced motion.
Class-based motion is described in [`motion.md`](motion.md).

## Accessibility

- The toast region is `role="region"` with `aria-label="Notifications"` and
  `aria-live="polite"`. Error toasts are `role="alert"`, others `role="status"`.
- The chip and progress bar are `aria-hidden`, so readers hear the title and body.
  Severity comes from the role and the text, not from colour.
- The toast close button has `aria-label="Dismiss notification"`.
- Icon-only controls always carry a label. Status dots pair colour with text or
  `role="img"` and an `aria-label`.
- A visually hidden live region in `ConsoleView` announces connection state.
- The text glyphs in toast chips and close buttons are `aria-hidden` or paired
  with a label. There are no emoji.

## Related documentation

- [Design system and tokens](design-system.md)
- [Icon system](icons.md)
- [Motion and animation](motion.md)
- [Console view](console-view.md)
