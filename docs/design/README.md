# UI design specifications

These pages describe the user interface of the AHV Remote Console desktop app as
it is built today. They are for designers and front-end contributors who want to
understand a surface before they change it.

Start with the [design overview](overview.md). It maps every window and screen and
links to the page that covers it.

## How to read these pages

- Each page explains the purpose of a surface, its layout, the components it uses,
  its states, and its accessibility rules.
- Exact pixel values live in the code. Each page links the source file once, so
  you can read the current value there.
- Design tokens and timing values that define the design system are kept in
  [`design-system.md`](design-system.md) and [`motion.md`](motion.md).
- When a page and the code disagree, the code is correct. Fix the page.

## Two visual themes

The renderer in `apps/desktop/src/renderer` uses two token families. Both are
declared in [`packages/ui/src/tokens.css`](../../packages/ui/src/tokens.css).

| Theme | Tokens | Used by |
| --- | --- | --- |
| Dark (default) | `--hc-*` | App shell, gate screens, every console window |
| Light, tool-grade | `--sans`, `--mono`, `--blue`, `--ink`, ... (as `T.*` in TSX) | Connection screens, cluster map, lobby |

On the light screens, colour carries status only, and data values such as
host:port, latency and byte counts use the mono face.

The sans face is Jost. The mono face is JetBrains Mono. The UI uses no
emoji. Every glyph comes from the SVG `Icon` component.

## Foundations

| Page | Covers |
| --- | --- |
| [`design-system.md`](design-system.md) | Token families, colour, type, radii, elevation, focus ring, scrollbars, boot splash |
| [`motion.md`](motion.md) | Duration and easing tokens, CSS animation layers, route transitions, reduced motion |
| [`icons.md`](icons.md) | The `Icon` component, its glyph set, sizing and colour rules |
| [`shared-ui.md`](shared-ui.md) | Shared primitives: toasts, notices, buttons, cards, fields, tooltips |

## Screens and windows

| Page | Covers |
| --- | --- |
| [`overview.md`](overview.md) | Window topology, navigation flow, full section inventory |
| [`app-shell.md`](app-shell.md) | Main window, runtime gate, route shell, command palette |
| [`sso-login-window.md`](sso-login-window.md) | The SSO window shell and the sign-in states around it |
| [`connection-manager.md`](connection-manager.md) | Endpoint list, add/edit form, sign-in panel |
| [`cluster-map.md`](cluster-map.md) | The pan/zoom endpoint map and its inspectors |
| [`lobby.md`](lobby.md) | The post-sign-in VM grid and session picker |
| [`console-view.md`](console-view.md) | The live VM viewport and its canvas |
| [`console-chrome.md`](console-chrome.md) | Ambient latency line and the top rail |
| [`console-overlays.md`](console-overlays.md) | Connecting, reconnecting, powered-off and error layers |
| [`capture-and-input.md`](capture-and-input.md) | Keyboard lock banner, Send Keys, guest layout, display fit |
| [`console-controls.md`](console-controls.md) | Display-fit menu, layout menu, fullscreen, shortcuts overlay |
| [`console-motion-visuals.md`](console-motion-visuals.md) | Latency ring, remote cursors, signal bars |
| [`console-popout.md`](console-popout.md) | The per-session console window and its boot screens |

## Conventions

- Use tokens, not raw hex. Use `--hc-*` on dark surfaces and `T.*` on the light
  connection screens. See [`design-system.md`](design-system.md).
- Status is never colour-only. Reachability and connection state always carry a
  text label next to the colour.
- Use the SVG `Icon` component for glyphs. Do not add emoji.
- Respect `prefers-reduced-motion`. See [`motion.md`](motion.md).

## Related documentation

- [Glossary](../reference/glossary.md)
- [Keyboard reference](../reference/keyboard.md)
