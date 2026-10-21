# Cluster map

The cluster map is the default view of the connection manager. It draws your
Prism Central endpoints as nodes on a pan-and-zoom canvas, with links you draw
between them and an inspector for the selected node or link. This page is for
designers and front-end contributors. The list view, the add and edit form and
the sign-in panel are in [Connection manager](connection-manager.md).

## Purpose

The map shows reachability at a glance and lets the operator record how clusters
relate, for example "Replication" or "DR pair". Links are operator metadata kept
in `localStorage`. The build reads no replication topology from the clusters, and
link health is derived from the reachability of the two endpoints.

## Layout

```text
+-------------------------------------------------------------+
| [Burst] AHV Remote Console   [search  ⌘F]  [Map|List] [+]   | 50px
+-------------------------------------------+-----------------+
|  . . . dot grid . . . (brand decoration)  | Inspector       |
|  +-- CUSTOMER · 3 -------------------+    | 320px, only     |
|  |  [node] ~~~ Replication ~~~ [node]|    | when a node or  |
|  |  [node]                           |    | link is         |
|  +-----------------------------------+    | selected        |
|  [+] [-] [fit]                            |                 |
+-------------------------------------------+-----------------+
| ● 2 healthy · ● 1 offline        (*) sync 30s  1 links  v… | 28px
+-------------------------------------------------------------+
```

The canvas has three layers, back to front:

1. A dot grid (34 px pitch) and the animated brand decoration. Both stay fixed
   while the content pans and zooms.
2. The world layer, which carries the pan and zoom transform. Inside it, in
   order: customer frames, link strokes, link labels, nodes.
3. Floating controls: the linking hint, the zoom buttons and the link-label form.

## Components

### Nodes

Each node is a 152 x 64 card with a status dot, the Burst glyph, the endpoint
name, and a mono meta line. The meta line shows "{n} VMs · {rtt}ms" when those
values are known locally, and host and port otherwise. On hover or selection a
small blue **+** appears on the right edge. Clicking it starts a link.

Endpoints without a saved position get an automatic layout. Each customer gets
a block, with four nodes per row. Dragging a node saves its position.

### Customer frames

A labelled box surrounds each customer's nodes and follows them when you drag.
The label is the customer name in uppercase mono with a count. Frames are not
drawn when every endpoint is in the single `Ungrouped` bucket.

### Links

Links are curved strokes that start and end at the node borders. A pill at the
midpoint shows a health dot and the label. Clicking the stroke or the pill
selects the link.

| Link health | Rule | Colour | Flow |
| --- | --- | --- | --- |
| Healthy | Both endpoints reachable | green | 2 s dash cycle |
| Degraded | Either status unknown | amber | 3.4 s |
| Down | Either endpoint offline | red, 70% opacity | 3.4 s |

Every link keeps moving. Health shows through colour, speed and opacity, never
by stopping. When motion is allowed, a small glowing pulse also travels along
each link.

### Inspector

The right rail opens when a node or link is selected and closes when the
selection clears. It has no transition.

- **Node inspector**: name, host and port, status, last-connected time, a
  **Links** list with **Unlink** per peer, then **Connect**, **Retry** (offline
  only), **Edit** and **Diagnostics**. **Delete endpoint** asks for inline
  confirmation: "Remove “{label}”? Its saved sign-in and links are cleared."
  When offline it adds "This cluster isn’t reachable. Check the network or VPN,
  then retry."
- **Link inspector**: label, derived health "(derived from endpoint status)",
  the two endpoint names, the note "Operator-defined link (this build exposes no
  cluster-reported replication topology).", and **Remove link**.

### Status bar

The footer counts healthy, offline and unknown endpoints. The unknown count
appears only when it is above zero. On the right it shows a sync indicator
("syncing…" with a spinning Burst, or "sync 30s"), the number of links and the
version.

## States and transitions

| Node state | Border | Other |
| --- | --- | --- |
| Default | 1.5 px, status colour | Light shadow |
| Selected | 2 px blue | 4 px blue-tint glow |
| Link target while linking | 1.5 px blue | Glow, pointer cursor |
| Link source while linking | Status colour | 60% opacity |
| Offline | Red | 70% opacity, grey Burst |
| Hidden by search | Unchanged | 35% opacity |
| Checking | Grey | Burst spins |

A node shows **Checking** only while a probe runs and no earlier result exists.

### Drawing a link

1. Click the **+** on a node. A blue dashed line follows the pointer, and a hint
   reads "Click another cluster to link · Esc to cancel".
2. Click another node. A popover opens at the click point and asks "What is this
   link?".
3. Type a label, or pick **Replication**, **DR pair** or **Backup**. **Link**
   stays disabled until the label is not empty.

There is no press-and-hold. Clicking the empty canvas or pressing Esc cancels.

### Pan and zoom

| Action | Result |
| --- | --- |
| Drag empty canvas | Pan. Also clears the selection. |
| Wheel | Zoom by 1.12 per step around the pointer |
| **+** / **-** buttons | Zoom by 1.2 |
| Fit button | Fit all nodes with 80 px padding |
| Enter in search | Centre the first match at the current zoom |

Zoom stays between 0.5 and 2. The map fits itself once, after the first layout.
A press on a node that does not move counts as a click and selects the node.

## Keyboard and accessibility

| Key | Action |
| --- | --- |
| Cmd+F (macOS) or Ctrl+F | Focus cluster search |
| `/` | Focus cluster search |
| Esc | Cancel linking, or clear the selection |
| Enter | Connect to the selected node |
| Arrow keys | Move the selected node 1 px, or 10 px with Shift |
| Cmd+K or Ctrl+K | Open the command palette (app-wide) |

Apart from Cmd/Ctrl+F, the map ignores these keys while a text field has focus.

- Nodes are `role="button"` with `tabIndex=0`, `aria-current` and an
  `aria-label` of "{label} — {status}".
- Status dots have `role="img"` and an `aria-label`. Footer counts pair each
  colour with a word.
- Search, the link handle, the zoom buttons and the close buttons have
  `aria-label` or `title`.
- The sync indicator is `aria-live="polite"`.
- The `.ec-focus` class draws a 2 px blue focus-visible outline.
- The brand decoration, the customer frames and the Burst glyph are hidden from
  assistive technology.
- Under reduced motion, CSS stops all `ec-*` animations and the link pulses are
  not rendered.

## Values

| Item | Value |
| --- | --- |
| Toolbar / footer height | 50 px / 28 px |
| Search box | 240 x 32 px, radius 8 |
| Node | 152 x 64 px, radius 12 |
| Customer frame | radius 16, 24 px padding |
| Link stroke | 2 px, 3 px selected |
| Inspector width | 320 px |
| Zoom button | 30 x 30 px, radius 8 |
| Link popover | 240 px wide, radius 10 |
| Dot grid | `#dce6f0`, 34 px pitch |
| Zoom range | 0.5 to 2, initial 1 |

Tokens and the status mapping are listed in
[Connection manager](connection-manager.md#look-and-tokens).

## Known gaps

- `T.canvas` points at `--canvas`, which `tokens.css` does not declare. The
  canvas colour comes from the dot grid and the surface behind it.
- The `cm-bg` class on the canvas has no CSS rule.
- The dot-grid colour, the frame fill and the shadows are literal values, not
  tokens.
- The empty **Links** text says "Hover the node and drag its handle", but the
  gesture is click, move, click.

Source: [`ClusterMap.tsx`](../../apps/desktop/src/renderer/components/ClusterMap.tsx)
and [`components/clustermap/`](../../apps/desktop/src/renderer/components/clustermap/).

## Related documentation

- [Connection manager](connection-manager.md)
- [Lobby](lobby.md)
- [Motion](motion.md)
- [Keyboard reference](../reference/keyboard.md)
