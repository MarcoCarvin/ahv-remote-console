# UI library

`@ahv-remote-console/ui` holds the shared React components, hooks and design tokens for the desktop app's renderer.

## Overview

The renderer gets its visual parts (buttons, dialogs, toasts, tabs) and its
console input hooks (pointer and keyboard capture, focus trap) from this package.
The only consumer is [`apps/desktop`](../../apps/desktop/README.md). The runtime
and the agent do not use it. See
[`docs/architecture/overview.md`](../../docs/architecture/overview.md) for the
whole system.

Everything is exported from [`src/index.ts`](src/index.ts). React 19 and
`framer-motion` 12 are peer dependencies, so the host app owns the single React
instance.

### Components

| Component | Purpose |
| --- | --- |
| `Button`, `IconButton` | Buttons with variants and sizes. |
| `Card`, `Surface` | Containers. `Surface` maps a `level` to a token background. |
| `Field` | Labelled text input. |
| `Spinner`, `StatusDot` | Loading and status. `StatusDot` kinds are `ok`, `warn`, `error`, `idle`, `pending`, with optional pulse. |
| `Tooltip`, `Kbd` | Hover label and keycap. |
| `Dialog`, `Tabs` | Modal (use with `useFocusTrap`) and the console tab strip. |
| `CommandPalette` | Fuzzy launcher over a `CommandItem[]`. |
| `SendKeysMenu` | Sends combos the browser cannot capture, such as Ctrl+Alt+Del. The renderer turns each `SendKeyCombo` into RFB key events. |
| `ToastProvider`, `useToast` | Toasts. See below. |

`useToast()` returns `{ push(kind, title, body?) }` and throws outside a `ToastProvider`. Kinds are `info`, `success`, `warn` and `error`. Identical consecutive toasts merge. Toasts close after 6 seconds and pause on hover. `placement` is set per surface; the console uses `top-center` because the bottom holds the rail and the guest taskbar.

### Hooks

| Hook | Purpose |
| --- | --- |
| `useResizeObserver()` | Returns `[ref, { width, height }]`, rounded. |
| `useFocusTrap(ref, active)` | Keeps Tab focus inside a container and restores focus on close. Use with `role="dialog"`. |
| `useKeyboardShortcut({ combo, handler })` | Global shortcut. `Mod` is Cmd on macOS and Ctrl elsewhere. |
| `useMouseCapture(opts)` | Click-to-capture with a release chord (default Ctrl+Alt). Pointer Lock is best effort in Electron. |
| `useKeyboardCapture({ enabled, onKeyDown, onKeyUp })` | Stops every key event in the page and forwards it to the VM, except the release chord. |

`ConsoleView` in the desktop app uses the capture hooks. See the [keyboard reference](../../docs/reference/keyboard.md).

### Design tokens

The app imports two stylesheets straight from `src/`. They are not built.

- `@ahv-remote-console/ui/tokens.css` defines the `--hc-*` custom properties: surfaces, text, accents, status, borders, radii, fonts and shadows. Dark is the default. Light values apply under `[data-theme="light"]`, and `prefers-color-scheme: dark` also selects dark. Every component styles itself with these variables.
- `@ahv-remote-console/ui/reset.css` is the base reset.

## Build and test

TypeScript compiled with `tsc` to `dist/`. The `.css` files ship as source. The package has no unit tests. From the repository root:

```bash
pnpm --filter @ahv-remote-console/ui build
pnpm --filter @ahv-remote-console/ui typecheck
pnpm --filter @ahv-remote-console/ui lint
```

Build the packages before type-aware ESLint, or it reports many `no-unsafe-*` warnings.

## Structure

| Path | Contents |
| --- | --- |
| `src/components/` | React components. |
| `src/hooks/` | React hooks. |
| `src/tokens.css`, `src/reset.css` | Design tokens and reset. |
| `src/index.ts` | Public exports. |

## Related documentation

- [Desktop app architecture](../../docs/architecture/desktop.md)
- [Keyboard reference](../../docs/reference/keyboard.md)
- [Building](../../docs/development/build.md)
- [Architecture overview](../../docs/architecture/overview.md)
