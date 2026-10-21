# Lobby

The lobby is the home screen after you sign in to an endpoint. You browse VMs,
reopen recent consoles, join live sessions and sort VMs into folders. This page
is for designers and front-end contributors.

## Purpose

The lobby answers two questions: what can I open, and what is already open.
`Lobby.tsx` owns presentation only. VM data, filtering, sorting and the connect,
refresh and favourite handlers come from `Shell.tsx`.

## Look

The lobby uses the same light token family as the
[connection manager](connection-manager.md#look-and-tokens), so it stays light
whatever `data-theme` is set. Two embedded primitives from `packages/ui` read the
dark `--hc-*` tokens instead:

- the search `Field`, which paints a dark input on the light canvas under the
  default theme;
- the loading `Spinner`.

## Layout

```text
+------+-----------------------------------------------------+
| Spark|  Lobby                                   [Refresh]  |
| ---  |  ● 2 SESSIONS LIVE · 40 VMS AVAILABLE   updated 1m  |
|  A   |                                                     |
|  H   |  RECENT ------------------------------------- < >   |
|  V   |  [card] [card] [card] ->                            |
|  ... |  live now                                           |
|      |  | ● name   host                           [Join]   |
| [env]|  all VMs                                            |
|  ↻   |  [Search VMs…]  starred  folders  on off all  + sort|
| (JD) |  [● vm  ▦ ☆] [● vm  ▦ ★] [● vm  ▦ ☆] [● vm  ▦ ☆]    |
+------+-----------------------------------------------------+
 86px spine          canvas, scrolls vertically
```

### Spine

An 86 px column on the left. It is the window drag handle. On macOS its top
padding leaves room for the traffic-light buttons. From top to bottom it holds
the Spark glyph, a vertical "AHV Remote Console" wordmark, the environment badge
(green, when the endpoint has an environment), a refresh button and the account
avatar.

The **account menu** opens from the avatar. It shows the identity and endpoint,
then **Switch cluster** and **Sign out** in red. The avatar shows the user's
initials and falls back to the endpoint's initials.

### Canvas

The canvas is white with two faint decorations, a dashed ring and a dot field.
It holds four sections.

1. **Header**: a large light "Lobby" headline, a status line with live sessions
   and available VMs, a dark **Refresh** button, and an "updated {time}" stamp
   ("never updated" before the first load).
2. **Recent**: a horizontal carousel of VMs opened on this endpoint in the last
   24 hours, newest first, at most six. VMs that are deleted or live now are
   left out. The first card is highlighted with a blue border, tint, shadow and
   Spark. Each card shows "opened {time}" and **Reconnect**. Arrow buttons
   scroll by one card.
3. **live now**: one slim row per VM with a live session, with a blue left
   stripe, the VM name and host, and **Join**.
4. **all VMs**: a toolbar and a grid of VM tiles. The grid uses
   `repeat(auto-fill, minmax(260px, 1fr))`, so it fits as many columns as the
   width allows.

The toolbar holds the search field, filter chips in the order starred, custom
folders, on, off, all, then a dashed **+** to create a folder, and a sort select
(name, power state, recent). There is no "recent" chip, because recents have
their own carousel.

## VM tile

From left to right a tile shows a status dot, the VM name, a secondary line
("ip · cluster"), an "off" word when not running, a folder button and a star.
Clicking the tile opens the console. Clicking the folder button or the star does
not.

| State | Look |
| --- | --- |
| Running | Green pulsing dot |
| Not running | Grey dot and the word "off" |
| Hover | Pale blue border, blue-tint fill |
| Starred | Filled green ★ (else grey ☆) |
| In any folder | Folder glyph turns blue |
| In a coloured folder | Border and a 3 px left stripe in the folder colour |
| Active session | Small green pip in the top-right corner |
| Dragging | 45% opacity, scaled to 0.97 |

The tile has two power states only. The test is `status === 'on'`. A starting
or stopping VM looks like an off VM.

## Folders

Folders are local only. They live in `localStorage` under `ahv-vm-folders`,
keyed per endpoint, and never reach the runtime. A folder has an id, a name, a
list of VM ids and an optional colour. Switching endpoints reloads the folders
and clears the folder selection.

You can file a VM in three ways:

- Drag a tile onto a folder chip. The chip shows a blue-ghost fill and a 2 px
  ring while you hover, then pops on drop. Dropping only adds; it never removes.
- Use the tile's folder button. The menu lists folders as checkboxes and has a
  "New folder…" field at the bottom.
- Create a folder with **+**. The popover asks for a name and one of five colours
  or none. **Create** is disabled while the name is empty.

Each folder chip has a **×** that deletes the folder.

## Screen states

| State | What shows |
| --- | --- |
| Loading | Centred spinner, "Loading VMs…" |
| Empty folder | "This folder is empty." |
| No matches | "No VMs match." |
| No recents | The Recent section is not rendered |
| No live sessions | The live now section is not rendered |

The lobby has no error overlay. `Shell.tsx` reports fetch errors as toasts. The
"updated" stamp is the only staleness signal on the screen.

## Motion

| Animation | Spec |
| --- | --- |
| Tile entrance | 280 ms, fade in and slide from 14 px left |
| Tile stagger | `(i % 4) * 60 + min(floor(i / 4), 14) * 8` ms |
| Category switch | The grid remounts, so the entrance plays again |
| Status pulse | Ring grows to 2.6x and fades, 2.4 s, repeating |
| Folder drop | Scale 1 to 1.13 and back, 340 ms |
| Route change | Shared axis X, 300 ms in and 200 ms out |

Switching filters replays the entrance rather than animating layout, which keeps
large grids smooth. Under `prefers-reduced-motion: reduce` the `ec-*` animations
stop and route transitions fade only. See [Motion](motion.md).

## Keyboard and accessibility

- Status is never colour alone. The dot has `role="img"` and "Status: On" or
  "Status: Off". Off tiles add the word "off". The header spells out the counts.
- The star and the active-session pip have labels that name what they mean.
- Filter chips use `aria-pressed`. Folder menu rows are `menuitemcheckbox` with
  `aria-checked`. The account menu uses `aria-haspopup`, `role="menu"` and
  `menuitem`. Colour swatches use `aria-pressed` and a label.
- The sort select is labelled "Sort VMs". The search field has a placeholder
  only.
- In the new-folder popover, Enter creates and Escape closes. All popovers close
  on a click outside.
- The `.ec-focus` class draws a 2 px blue focus-visible outline.
- Icons come from the SVG `Icon` set and are `aria-hidden`. There are no emoji.

## Values

| Item | Value |
| --- | --- |
| Spine width | 86 px |
| Canvas padding | 40 px by 46 px |
| Headline | Jost 300, 66 px |
| Section headings | Jost 500 italic, 17 px |
| Grid | min 260 px columns, 10 px gap |
| Tile | radius 11, padding 11 by 13 |
| Recent card | 244 px wide, radius 14 |
| Chips | pill, mono 12 px |
| Popovers | radius 11 to 12, shadow `0 12px 28px -10px rgba(15,34,53,.35)` |
| Folder colours | blue, green, amber, red, dark blue |

## Known limitations

- The tile is a `div`, not a button, and has no `tabIndex`. The star is a `span`
  with `role="button"` and no `tabIndex`. Opening a VM and starring it need a
  pointer. The folder menu is the click alternative to drag and drop.
- Comments in `Shell.tsx` describe a tile-to-console morph through `layoutId`
  with `containerSpring` (stiffness 380, damping 38). No component sets a
  `layoutId`, so the morph does not run.

Source: [`Lobby.tsx`](../../apps/desktop/src/renderer/components/Lobby.tsx),
[`foldersStore.ts`](../../apps/desktop/src/renderer/app/foldersStore.ts),
[`recentStore.ts`](../../apps/desktop/src/renderer/app/recentStore.ts).

## Related documentation

- [Connection manager](connection-manager.md)
- [Console view](console-view.md)
- [Motion](motion.md)
- [Design system](design-system.md)
