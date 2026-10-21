# Design overview

This page maps every window, screen and UI surface of the desktop app and links
each one to its detailed page. Read it first if you are new to the renderer.

## Themes and type

The renderer (`apps/desktop/src/renderer`) ships two themes from one stylesheet,
[`packages/ui/src/tokens.css`](../../packages/ui/src/tokens.css), imported once in
`main.tsx`.

| Theme | Where | Key values |
| --- | --- | --- |
| Dark `--hc-*` (default) | App shell, gate screens, console windows | canvas `#0f1b2a`, accent `#0878c8` |
| Light tool-grade `T.*` | Connection screens, cluster map, lobby | paper `#ffffff`, blue `#0e76c4` |

The sans face is Jost and the mono face is JetBrains Mono. Both are bundled with
the app. Details are in [`design-system.md`](design-system.md).

## Windows

The app runs three kinds of native `BrowserWindow`. The main process creates all of
them. One renderer bundle serves both the main window and the console windows. It
picks what to mount from the `?mode` query parameter.

| Window | Default size (min) | Background | Content | Trust |
| --- | --- | --- | --- | --- |
| Main window | 1440 x 900 (960 x 600) | `#0f1b2a` | `App` and `Shell` | Trusted, preload and IPC |
| Console window | 1280 x 800 (640 x 400) | `#0f1b2a` | `ConsoleWindowApp` (`?mode=console`) | Trusted, same bundle |
| SSO window | 980 x 720 | `#0a0c12` | Prism's own login page | Untrusted, no preload, sandboxed |

The SSO window never loads the app bundle. The main process opens it, loads
Prism directly and watches its cookies.

## Navigation flow

`App` runs a runtime phase machine (`starting`, `ready`, `error`). When the runtime
is ready, `Shell` runs an `AppStage` machine and swaps screens with one
`AnimatePresence`.

```text
runtime starting --> ready --> Shell
                 \-> error (fatal screen with Retry)

Shell (main window, light screens)
  no-endpoints / add-endpoint --> AddEndpointPanel
        | saved (new) --> sign-in      | saved (edit) --> select-endpoint
  select-endpoint
        |-- map view  --> ClusterMap (default)
        '-- list view --> EndpointPicker (accessible peer)
        | Connect
  sign-in --> SignInPanel --(Sign in with SSO)--> SSO window
        | cookies captured
  ready --> Lobby
        | open a VM tile
        v
  Console window (one per session, dark) --> ConsoleView
```

- On start, `Shell` loads endpoints and lands on `no-endpoints` or
  `select-endpoint`.
- **Connect** loads resources. An auth error routes to `sign-in`, otherwise to
  `ready`.
- After sign-in, `Shell` retries the load.
- Signing out returns to `select-endpoint` and closes open console windows.

## Section inventory

Paths are relative to `apps/desktop/src/renderer` unless they start with
`apps/` or `packages/`.

| Surface | Main files | Theme | Page |
| --- | --- | --- | --- |
| Window factories | `apps/desktop/src/main/main.ts` | dark | [app-shell](app-shell.md), [console-popout](console-popout.md) |
| SSO window | `apps/desktop/src/main/sso-login.ts` | Prism's | [sso-login-window](sso-login-window.md) |
| Renderer entry, mode split | `main.tsx` | both | [app-shell](app-shell.md) |
| Runtime gate, command palette | `app/App.tsx` | dark | [app-shell](app-shell.md) |
| Route shell | `app/Shell.tsx`, `app/routeReducer.ts` | both | [app-shell](app-shell.md) |
| Console window root | `app/ConsoleWindowApp.tsx` | dark | [console-popout](console-popout.md) |
| Endpoint list, add/edit, sign-in | `components/EndpointPicker.tsx`, `AddEndpointPanel.tsx`, `SignInPanel.tsx`, `clusterShared.tsx` | light | [connection-manager](connection-manager.md) |
| Cluster map and inspectors | `components/ClusterMap.tsx`, `components/clustermap/` | light | [cluster-map](cluster-map.md) |
| Lobby | `components/Lobby.tsx` | light | [lobby](lobby.md) |
| Live viewport | `components/ConsoleView.tsx`, `components/console/framebufferCanvas.ts` | dark | [console-view](console-view.md) |
| Rails and rail buttons | `components/console/ConsoleRails.tsx`, `components/FloatingCluster.tsx` | dark | [console-chrome](console-chrome.md) |
| State overlays | `components/console/ConsoleOverlays.tsx`, `status.ts` | dark | [console-overlays](console-overlays.md) |
| Lock banner, Send Keys, layout | `components/console/CaptureIndicator.tsx`, `ConsoleView.tsx` | dark | [capture-and-input](capture-and-input.md) |
| Fit, layout, fullscreen, shortcuts | `presence/` | dark | [console-controls](console-controls.md) |
| Telemetry visuals | `components/console-motion/` | dark | [console-motion-visuals](console-motion-visuals.md) |
| Tokens and reset | `packages/ui/src/tokens.css`, `reset.css`, `components/clusterTokens.ts` | both | [design-system](design-system.md) |
| Icons | `components/Icon.tsx` | inherits colour | [icons](icons.md) |
| Shared primitives | `packages/ui/src/` | both | [shared-ui](shared-ui.md) |
| Motion | `motion/` | n/a | [motion](motion.md) |

## Surfaces in brief

### Foundations

- Design system. Two token families, the type faces, radii, shadows and the
  global reset. See [`design-system.md`](design-system.md).
- Motion. Duration and easing tokens, route transitions with Framer Motion,
  CSS animation layers gated by `data-motion-*`, and three reduced-motion
  mechanisms. See [`motion.md`](motion.md).
- Icons. One SVG `Icon` component. Glyphs paint with `currentColor`, so they
  take the colour of their container. See [`icons.md`](icons.md).
- Shared UI. Toasts (bottom-right in the main window, top-centre in console
  windows), buttons, cards, fields, tooltips and the command palette on `mod+k`.
  See [`shared-ui.md`](shared-ui.md).

### Windows and shell

- App shell. The main window opens on a dark canvas and shows on
  `did-finish-load`, so you never see a white frame. On macOS the traffic lights
  sit over the content. See [`app-shell.md`](app-shell.md).
- SSO window. A separate, sandboxed window with an isolated session
  partition. Only its shell and the sign-in states around it are ours. See
  [`sso-login-window.md`](sso-login-window.md).
- Console window. One window per session, titled `<vm> — AHV Remote Console`.
  It shows a connecting screen and an error screen before the live console. See
  [`console-popout.md`](console-popout.md).

### Connection screens and lobby

- Connection manager. The pre-sign-in screens. The list view shows only local
  facts: name, host:port, reachability and last connection. It shows no VM
  inventory. See [`connection-manager.md`](connection-manager.md).
- Cluster map. The default endpoint view. Endpoints are draggable nodes on a
  pan/zoom canvas, with operator-drawn link-health edges and a right-hand
  inspector. See [`cluster-map.md`](cluster-map.md).
- Lobby. The VM browser after sign-in, with a blue brand spine, a recent
  carousel, a "live now" strip and the VM grid. Clicking a tile opens a console
  window. See [`lobby.md`](lobby.md).

### Console surfaces

- Console view. One `<canvas>` paints the guest framebuffer. All chrome floats
  over it. See [`console-view.md`](console-view.md).
- Console chrome. A 3px ambient latency line and a glass top rail that appears
  on hover and can be pinned. There is no bottom rail. See
  [`console-chrome.md`](console-chrome.md).
- Console overlays. Connecting, reconnecting, starting, powered-off and error
  layers. The error panel offers recovery by cause (`session_expired`,
  `rbac_denied`, `unreachable`). See [`console-overlays.md`](console-overlays.md).
- Capture and input. Capture is focus-based. The lock banner shows only while
  keyboard lock is armed. See [`capture-and-input.md`](capture-and-input.md).
- Console controls and telemetry. See
  [`console-controls.md`](console-controls.md) and
  [`console-motion-visuals.md`](console-motion-visuals.md).

## Related documentation

- [Design specifications index](README.md)
- [Design system](design-system.md)
- [Glossary](../reference/glossary.md)
