# Design system and tokens

This page defines the token vocabulary every other design page uses: the two
themes, colour, type, radii, elevation, motion tokens and the global element
rules. Tokens live in [`packages/ui/src/tokens.css`](../../packages/ui/src/tokens.css)
and the reset in [`packages/ui/src/reset.css`](../../packages/ui/src/reset.css).
Both are imported once in `apps/desktop/src/renderer/main.tsx`.

## Two token families

| Family | Declared on | Used by |
| --- | --- | --- |
| `--hc-*` brand palette | `:root, [data-theme='dark']` | App shell, gate screens, console windows |
| Tool-grade `--sans`, `--mono`, `--blue`, `--ink`, ... | plain `:root`, always present | Lobby, endpoint list, add/edit, sign-in, cluster map |

TSX reaches the tool-grade tokens through the `T.*` aliases in
[`clusterTokens.ts`](../../apps/desktop/src/renderer/components/clusterTokens.ts).
Each alias is a `var(--token)` string.

The document is dark by default. The reset paints `html`, `body` and `#root` with
`--hc-bg-0`, `--hc-fg-0` and the sans face. The light screens do not switch theme.
They paint the tool-grade tokens directly with inline styles.

> [!NOTE]
> A `[data-theme='light']` block remaps part of `--hc-*`, but no code sets that
> attribute, so it is dormant. `T.canvas` points to `--canvas`, which
> `tokens.css` does not define.

## Dark theme colours (`--hc-*`)

| Token | Value | Role |
| --- | --- | --- |
| `--hc-bg-0` | `#0f1b2a` | Window background, deepest surface |
| `--hc-bg-1` | `#15263a` | Raised surface, cards |
| `--hc-bg-2` | `#1a3048` | Fields |
| `--hc-bg-3` | `#1f3955` | Tooltips, key caps |
| `--hc-bg-elev` | `rgba(255,255,255,0.04)` | White wash for elevation |
| `--hc-bg-glass` | `rgba(15,27,42,0.72)` | Panel fill under `backdrop-filter` |
| `--hc-fg-0` | `#e8f1f8` | Primary text, headings |
| `--hc-fg-1` | `#b8cfe0` | Secondary text |
| `--hc-fg-2` | `#94a3b8` | Muted text, labels |
| `--hc-fg-3` | `#64748b` | Faint or disabled text |
| `--hc-accent-50` / `-200` | `#e8f4fa` / `#b8d8e8` | Accent tints |
| `--hc-accent-400` | `#1788d8` | Focus ring, links |
| `--hc-accent-500` | `#0878c8` | Brand blue, primary buttons, selection |
| `--hc-accent-600` / `-700` | `#1668a5` / `#054d8a` | Deep accents |
| `--hc-success` | `#689838` | OK, live, reachable |
| `--hc-warning` | `#d68a1f` | Caution |
| `--hc-error` | `#c8102e` | Failure |
| `--hc-info` | `#5ab0e8` | Information |
| `--hc-border-0` | `rgba(255,255,255,0.06)` | Hairline |
| `--hc-border-1` | `rgba(255,255,255,0.12)` | Default control border |
| `--hc-border-strong` | `rgba(255,255,255,0.22)` | Hover and emphasis border |

The dormant light block flips surfaces to pale blues, text to dark ink, and
borders and shadows to alpha-ink. When the OS reports a dark scheme and no theme
is set, `tokens.css` re-asserts the four dark surfaces.

## Light tool-grade colour tokens

These tokens are flat and theme-independent. On these screens colour carries
status only, and data is always in the mono face.

| Token | Value | Role |
| --- | --- | --- |
| `--blue` / `--blueD` / `--blueTint` | `#0e76c4` / `#0a5ba0` / `#e7f1fb` | Primary action, pressed, pale fill |
| `--green` / `--greenD` / `--greenTint` | `#6aa442` / `#557f33` / `#eef4e6` | Reachable, healthy |
| `--amber` | `#cf8d1c` | Warning |
| `--red` | `#cf4036` | Offline, error |
| `--ink` / `--ink2` / `--ink3` / `--ink4` | `#14283c` / `#4d6076` / `#8194a8` / `#aebccb` | Text ramp, primary to faint |
| `--bg` / `--soft` | `#ffffff` / `#f5f8fb` | Surfaces |
| `--line` / `--line2` | `#e3eaf1` / `#d3dde7` | Rules and borders |

Lobby and console additions: `--blueDk #0a4d80`, `--blueL #4aa3dc`,
`--bluePale #cfe6f7`, `--blueGhost #f3f9fd`, `--greenL #8cc25e`,
`--paper #ffffff`, `--soft2 #edf3f9`, `--rule #e4ecf3`, `--rule2 #d5e1ec`.

Status always pairs a colour with a label. `statusMeta` in `clusterTokens.ts`
returns both:

| State | Colour | Label |
| --- | --- | --- |
| `healthy` | `T.green` | Reachable |
| `offline` | `T.red` | Offline |
| `checking` (default) | `T.ink4` | Checking… |

## Typography

| Token | Stack |
| --- | --- |
| `--hc-font-sans`, `--sans` | `'Jost', system-ui, -apple-system, sans-serif` |
| `--hc-font-mono`, `--mono` | `'JetBrains Mono', ui-monospace, 'SF Mono', Menlo, monospace` |

- Jost is the brand face for body, chrome and headings. Bundled weights are 400,
  500, 600 and 700.
- JetBrains Mono carries all data values: host:port, latency and byte counters.
  Bundled weights are 400, 500 and 600.
- The app loads latin-only subsets from `@fontsource`, which saves about 184 KB.
- The base is 14px with line-height 1.5, antialiased. Buttons and form controls
  inherit the body font.

There is no type-scale token set. Components set their own sizes, usually inline.

## Spacing, radii and elevation

There are no spacing tokens. Components set padding and gaps themselves.

| Token | Value | Use |
| --- | --- | --- |
| `--hc-radius-sm` | `6px` | Inputs, small chips, focus ring |
| `--hc-radius-md` | `10px` | Controls |
| `--hc-radius-lg` | `12px` | Cards, panels |
| `--hc-radius-xl` | `12px` | Same as `lg` |
| `--hc-shadow-soft` | `0 4px 12px rgba(15,34,53,0.3)` | Resting card |
| `--hc-shadow-deep` | `0 20px 60px -12px rgba(15,34,53,0.5)` | Modal, popover |
| `--hc-shadow-glow` | 1px accent ring plus a soft blue bloom | Accent, active |

## Motion tokens

| CSS token | TS token | Value | Use |
| --- | --- | --- | --- |
| `--dur-short3` | `duration.short3` | 150ms | Crossfade |
| `--dur-short4` | `duration.short4` | 200ms | Exit |
| `--dur-medium2` | `duration.medium2` | 300ms | Enter |
| `--dur-medium4` | `duration.medium4` | 400ms | Shared element |
| `--ease-standard` | `ease.standard` | `cubic-bezier(0.2,0,0,1)` | Default |
| `--ease-decel` | `ease.decel` | `cubic-bezier(0.05,0.7,0.1,1)` | Entering |
| `--ease-accel` | `ease.accel` | `cubic-bezier(0.3,0,0.8,0.15)` | Exiting |

Legacy tokens remain for older components: `--hc-motion-fast 120ms`,
`--hc-motion-base 220ms`, `--hc-motion-slow 360ms`,
`--hc-easing-emphasized cubic-bezier(0.2,0.8,0.2,1)` and
`--hc-easing-decelerated cubic-bezier(0.2,0.85,0.4,1)`. Under
`prefers-reduced-motion: reduce` the three legacy durations become `0ms`. Layers,
animations and reduced-motion rules are in [`motion.md`](motion.md).

## Global element rules

| Element | Rule |
| --- | --- |
| Box model | `box-sizing: border-box` on everything |
| Focus | `:focus-visible` draws `2px solid var(--hc-accent-400)`, offset 2px, radius `--hc-radius-sm` |
| Focus opt-out | `.no-focus-ring`, for components that draw their own ring |
| Links | `--hc-accent-400`, underline on hover only |
| Selection | `--hc-accent-500` background, white text |
| Media | `img` and `canvas` are `display: block; max-width: 100%` |
| Scrollbars | 10px, transparent track, thumb `--hc-border-1`, `--hc-border-strong` on hover |

The focus ring shows on keyboard focus only, not on mouse or touch.

## Boot splash

`index.html` paints a static splash before React and fonts load, so the window is
never a blank black frame. It uses hard-coded values on purpose: background
`#0f1b2a`, text `#94a3b8`, the system font, and a 28px ring spinner with an
`#1788d8` top edge that turns every 0.8s. Under reduced motion the spinner stops.
`main.tsx` fades the splash out after React paints its first frame (two
animation frames), then removes it after 300ms.

## Accessibility rules

- Status is never colour-only. Every status colour sits next to a text label.
- Every interactive element gets the global focus ring unless it opts out and
  draws its own.
- Glyphs come from the SVG `Icon` component, which is `aria-hidden`. The toast
  kind chips use a few text glyphs (`✓ ! ✕ i`). The UI has no emoji.
- Reduced motion is honoured in CSS, in the legacy tokens and in Framer Motion.

## Usage rules

1. Reference tokens, not raw hex. The boot splash is the only deliberate
   exception.
2. Use dark `--hc-*` for the shell and console. Use `T.*` for the light
   connection screens and lobby.
3. Put data in the mono face.
4. New components use the `--dur-*` and `--ease-*` tokens and the `atomic.css`
   class forms. Older components keep the legacy tokens until they are migrated.
5. Radii: `sm` for inputs and chips, `md` for controls, `lg` for cards and panels.
6. Elevation: `soft` at rest, `deep` for overlays, `glow` for accent or active.

## Related documentation

- [Motion and animation](motion.md)
- [Icon system](icons.md)
- [Shared UI primitives](shared-ui.md)
- [Design overview](overview.md)
