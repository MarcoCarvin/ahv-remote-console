# Icon system

Every glyph in the UI comes from one hand-written SVG component, `Icon`, in
[`apps/desktop/src/renderer/components/Icon.tsx`](../../apps/desktop/src/renderer/components/Icon.tsx).
The app uses no emoji, no icon font and no image sprites. This page explains the
component, its glyph set and how to add an icon.

## Using the component

```tsx
import { Icon, type IconName } from './Icon';

<Icon name="search" size={14} style={{ color: 'var(--ink3)' }} />;
```

| Prop | Type | Default |
| --- | --- | --- |
| `name` | `IconName` | required |
| `size` | `number` | `16` |
| `className` | `string` | none |
| `style` | `React.CSSProperties` | none |

The component renders one `<svg>` with `width` and `height` set to `size`, a
`0 0 24 24` viewBox and `aria-hidden`. The body is a lookup into a
`Record<IconName, ReactNode>`. Only the TypeScript union guards against an unknown
name.

## Colour

Every path paints with `currentColor`, so an icon takes the `color` of its
container. There is no `color` prop. To recolour an icon, set `color` on the icon's
`style` or on the button around it.

Most glyphs are stroked outlines with `fill="none"`. Only small marks are solid:
the `power-dot` body, the `lock` and `unlock` keyholes, and the `info` dot.

## Size and stroke

- Geometry is drawn on a 24-unit grid. `size` sets width and height in px, so
  icons are always square.
- `size` is a raw number, not a token. Values in use run from 13 to 20px. The
  default is 16px.
- Every stroke is `1.75` user units with round caps. Round joins appear only where
  a glyph has corners.
- Strokes scale with the icon because they are not `non-scaling-stroke`. At 16px
  a stroke draws at about 1.17px, at 13px about 0.95px, at 20px about 1.46px.

## Glyph set

| Name | Shape |
| --- | --- |
| `power-dot` | Filled circle |
| `search` | Ring with a handle |
| `chevron-down` | Down-pointing V |
| `arrow-right` | Shaft with arrowhead |
| `plus` | Cross |
| `close` | X |
| `keyboard` | Rounded rectangle with three keys and a spacebar |
| `disc` | Outer ring with a hub |
| `clipboard` | Board, clip and an up arrow |
| `fit` | Rounded square with corner brackets |
| `globe` | Ring, equator and meridian |
| `fullscreen` | Four outward corner brackets |
| `endpoint-badge` | Ring with eight radiating ticks |
| `refresh-pulse` | Open circular arrow |
| `lock` | Body, closed shackle, filled keyhole |
| `unlock` | Body, open shackle swung out, filled keyhole |
| `info` | Ring with stem and dot |
| `layers` | Stacked diamond with two bands (snapshots) |
| `power-on` | Broken ring with a bar |
| `power-off` | Stop square |
| `power-cycle` | Near-closed circular arrow |

`lock` and `unlock` have clearly different shapes, so the state reads without
colour.

## Where icons render

These modules render `<Icon>` directly: `FloatingCluster.tsx`, `EndpointPicker.tsx`,
`Lobby.tsx`, `ClusterMap.tsx`, `SnapshotsPanel.tsx`, `TasksTray.tsx`,
`console/StatsPanel.tsx` and `console/Overlay.tsx`.

The console rail renders icons indirectly. `ConsoleView.tsx` builds rail items as
data with an `icon: IconName` field. `ConsoleRails.tsx` passes each item to
`ClusterBtn` in `FloatingCluster.tsx`, which renders the `<Icon>`.

| Rail item | Icon |
| --- | --- |
| Send Keys | `keyboard` |
| Prerequisites & media | `disc` |
| Guest keyboard layout | `globe` |
| Power | `power-dot` |
| Snapshots | `layers` |
| Live VM stats | `info` |
| Fullscreen | `fullscreen` |
| Close session | `close` |

## Colours an icon picks up

| Context | Colour |
| --- | --- |
| Light screens, primary affordance | `T.blue` (`#0e76c4`) |
| Light screens, search and refresh | `T.ink3` (`#8194a8`) |
| Filled blue button | `#fff` |
| Console rail, default | `rgba(255,255,255,0.82)` |
| Console rail, `danger` tone (Power) | `#F2A65A` |
| Console rail, `primary` tone | `#fff` on `var(--blue)` |

## States

`Icon` has no state of its own. It renders the same SVG whatever happens. The
container owns hover, active, focus, disabled and loading, and the icon follows
through inherited colour and opacity.

- Hover on a rail button changes the button background, not the icon.
- Keyboard focus draws a ring on the control, never on the SVG.
- No icon spins. Spinners in the UI are separate elements.

The SVG carries no transition or animation, so reduced motion has no direct effect
on it.

## Accessibility

- Icons are decorative. The accessible name comes from the control. Rail buttons
  set `aria-label` and `title`. Search inputs have their own `aria-label`.
- Where an icon or dot shows state, a text label or `aria-label` sits next to it.
- The rail button is 28 x 28px around a 16px icon. Some light-screen buttons use a
  44px minimum height. The icon size does not set the hit area.
- The search icon in `EndpointPicker` sets `pointer-events: none`, so clicks reach
  the input. The search icons in `Lobby` and `ClusterMap` do not.

## Adding an icon

1. Add the name to the `IconName` union in `Icon.tsx`.
2. Add a matching entry to the paths record. Draw it on the 24 x 24 grid with
   stroke `1.75`, `currentColor` paint, round caps, and round joins only at
   corners.
3. Use it through `<Icon name="...">` or as a rail item's `icon`. No other file
   needs to change.

> [!NOTE]
> The no-emoji rule is a review convention. No lint rule enforces it.

## Related documentation

- [Design system and tokens](design-system.md)
- [Shared UI primitives](shared-ui.md)
- [Capture and input](capture-and-input.md)
