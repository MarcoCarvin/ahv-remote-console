# Connection manager

The connection manager is the set of screens you see before you sign in to a
cluster. This page describes them for designers and front-end contributors: the
cluster list, the add and edit form, and the sign-in panel. The cluster map has
its own page, [Cluster map](cluster-map.md). The lobby and the console view are
also separate.

## Purpose

The operator picks a Prism Central endpoint, checks that it is reachable, and
starts a session. Two peer views show the same endpoints and share one
selection: the map (the default) and the list, an accessible list with a detail
pane in `EndpointPicker`. A **Map | List** toggle in the toolbar switches
between them.

> [!IMPORTANT]
> These screens render before authentication. They show only facts the desktop
> app knows locally: name, host and port, reachability, and last-connected time.
> The list never shows VM counts, latency or session names. The map shows VM
> count and round-trip time only after they are known locally.

## Look and tokens

These screens use a light skin. Its tokens sit under the unconditional `:root`
block in [`packages/ui/src/tokens.css`](../../packages/ui/src/tokens.css), so
they stay light whatever `data-theme` is set. Components read them through the
`T.*` aliases in
[`clusterTokens.ts`](../../apps/desktop/src/renderer/components/clusterTokens.ts).

Colour conveys status only. Data such as host and port, latency, counts,
fingerprints and versions is always in the mono face.

| Token | Value | Use |
| --- | --- | --- |
| `--sans` | Jost | Labels, headings, buttons |
| `--mono` | JetBrains Mono | All data |
| `--blue` | `#0e76c4` | Primary actions, selection |
| `--blueTint` | `#e7f1fb` | Selected row, selection glow |
| `--green` | `#6aa442` | Reachable |
| `--amber` | `#cf8d1c` | Degraded link, insecure-TLS warning |
| `--red` | `#cf4036` | Offline, destructive actions |
| `--ink` to `--ink4` | `#14283c` to `#aebccb` | Text from primary to quaternary |
| `--bg` / `--soft` / `--soft2` | `#ffffff` / `#f5f8fb` / `#edf3f9` | Cards, toolbars, list sidebar |
| `--line` / `--line2` | `#e3eaf1` / `#d3dde7` | Hairlines, input and card borders |

Status comes from one mapping, `statusMeta`:

| State | Colour | Label |
| --- | --- | --- |
| `healthy` | green | Reachable |
| `offline` | red | Offline |
| `checking` and anything else | ink4 | Checking… |

`checking` exists only in the UI. It means a probe is in flight.

`main.tsx` loads Jost weights 400 to 700 only. Headlines that ask for weight 300
render at 400. Radii and shadows are literal values in the code, not the
`--hc-radius-*` tokens of the dark app shell.

## List view

```text
+--------------------------------------------------------+
| [Spark] AHV Remote Console          [Map|List]  [+]    |  56px
+---------------+----------------------------------------+
| [search]      |  ● Label                   [Reachable] |
| v CUSTOMER  3 |  host · last connected 2h ago          |
|   ● env       |  [Connect] [Edit] [Diagnostics] [Del]  |
|     host:port |----------------------------------------|
|   ● env       |  ● NOT CONNECTED                       |
|               |        (SignalStage hero)              |
| [+ New endpt] |        Ready when you are.             |
+---------------+----------------------------------------+
| ● 2 reachable ● 1 unreachable     3 endpoints · v…     |  30px
+--------------------------------------------------------+
```

### Components

- **Toolbar**: Spark glyph, the "AHV Remote Console" wordmark, the view toggle
  and a filled blue **+** button. The map toolbar uses the Burst glyph and an
  outlined **+** instead.
- **Sidebar** (288 px): search field, endpoints grouped by customer, and a
  **New endpoint** button at the bottom. Group headers collapse and show a count.
  They appear only when there is more than one group or the single group is not
  `Ungrouped`. A filter with no hits shows "No matches."
- **Row**: status dot, environment name (or label) and host and port. Selected
  rows get a blue left bar and the tint fill. Single click selects, double click
  connects.
- **Detail, top section**: the endpoint label as a large light headline, a status
  pill, a sub-line with host and connection state, and the actions. The primary
  action is **Connect**, or **Retry** when offline; when offline a secondary
  **Connect** also appears. **Edit** and **Diagnostics** appear when the host
  provides them. **Delete** is a quiet ghost button that turns red on hover and
  swaps to an inline "Remove “{label}”?" confirm.
- **Detail, hero section**: a tinted panel with a caption, the `SignalStage`
  animation, a headline and one line of help.
- **Status bar** (30 px): reachable and unreachable counts, endpoint count,
  "not connected" and the version.

### States

| State | Hero mood | Caption | Pill | Headline |
| --- | --- | --- | --- | --- |
| Offline | error | OFFLINE (red) | Unreachable | Can’t reach {name}. |
| Checking | active | CHECKING (blue) | Checking… | Checking {name}… |
| Reachable | idle | NOT CONNECTED (grey) | Reachable | Ready when you are. |

A reachable endpoint shows a green pill but a grey caption dot. Green means
"reachable", never "connected".

With no endpoints at all, the list shows the toolbar and a dashed card: "Add your
first endpoint" over "Connect to a Prism Central cluster".

`SignalStage` shows the Spark glyph inside two rotating rings with a periodic
ping. The mood class alone changes its colour and speed. In `error` it turns
red, stops, and hides the ping. Hovering spins the spark up and lets it coast to
rest. The hover spin is off in `error` and under reduced motion.

## Add and edit endpoint form

The form opens from **+**, **New endpoint** or **Edit**. It is a 480 px card
centred on a tinted backdrop with the brand header on top.

| Field | Notes |
| --- | --- |
| **Prism Central URL** | Autofocused. Must start with `http://` or `https://`. |
| **Friendly name (optional)** | Defaults to the hostname. |
| **Customer** / **Environment** | Side by side. Group the endpoint in the list and map. |
| **Pinned leaf certificate (base64)** | Under **Advanced TLS**. Saving it empty clears an existing pin. |
| **Trust the certificate at first sign-in** | Under **Advanced TLS**. The label turns amber when checked. |

The **Advanced TLS** section starts collapsed. It opens on its own when you edit
an endpoint that already has a pin or the insecure flag.

The title reads "Connect to Prism" for a new endpoint and "Edit endpoint" when
editing. The submit button reads **Continue** or **Save**, and **Saving…** while
it works, when it is disabled. An invalid URL shows a warning toast and keeps the
form open. A successful save shows no toast.

## Sign-in panel

The sign-in panel opens after **Connect**. It is a 440 px card on the same
backdrop. The header shows "Sign in", the endpoint label and its URL. The body
has four exclusive states:

1. **Prompt**: "A Prism sign-in window will open…" and **Sign in with SSO**.
2. **Waiting**: a pulsing blue dot and "Waiting for sign-in…".
3. **Error**: "Sign-in did not complete", the message, and **Open Prism sign-in
   again**.
4. **First connection** (trust on first use): "First connection to {host}" and
   the SHA-256 fingerprint as colon-separated hex pairs in a mono block. You
   choose **Cancel sign-in** or **Trust and sign in**. **Trust and sign in** is
   disabled when no fingerprint is available. The session reaches the runtime
   only after you trust the certificate.

A **← Back to endpoints** link sits at the bottom. The SSO window opens on its
own only when the panel was reached by an explicit Connect or Add. A background
session expiry never opens it.

## Keyboard and accessibility

| Key | Where | Action |
| --- | --- | --- |
| Cmd+F (macOS) or Ctrl+F | List and map | Focus the search field |
| `/` | Map | Focus the search field |
| Up, Down | List | Move the selection, wrapping |
| Enter | List | Connect, or probe again when offline |
| Enter, Space | Focused row | Select it |
| Cmd+K or Ctrl+K | Anywhere | Open the command palette |

The list ignores these keys while a text field has focus, except Cmd/Ctrl+F.

- Status is never colour alone. `StatusDot` has `role="img"` and an
  `aria-label` of "Status: {label}". The detail pill has `role="status"`.
- Rows are `role="button"` with `tabIndex=0` and `aria-current`.
- Group headers and the **Advanced TLS** toggle carry `aria-expanded`. The view
  toggle uses `aria-pressed`.
- The `.ec-focus` class draws a 2 px blue `:focus-visible` outline with a 2 px
  offset.
- Toolbars are window drag regions. Controls inside them opt out. On macOS the
  left padding leaves 78 px for the traffic-light buttons.
- Under `prefers-reduced-motion: reduce`, all `ec-*` and `SignalStage`
  animations stop.

## Known gaps

- The list search field shows a `⌘K` badge on every platform, but the shortcut
  that focuses it is Cmd+F or Ctrl+F. Cmd/Ctrl+K opens the command palette.

Source: [`EndpointPicker.tsx`](../../apps/desktop/src/renderer/components/EndpointPicker.tsx),
[`AddEndpointPanel.tsx`](../../apps/desktop/src/renderer/components/AddEndpointPanel.tsx),
[`SignInPanel.tsx`](../../apps/desktop/src/renderer/components/SignInPanel.tsx),
[`clusterShared.tsx`](../../apps/desktop/src/renderer/components/clusterShared.tsx).

## Related documentation

- [Cluster map](cluster-map.md)
- [Lobby](lobby.md)
- [Design system](design-system.md)
- [Motion](motion.md)
- [Keyboard reference](../reference/keyboard.md)
