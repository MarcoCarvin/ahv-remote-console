# App icon

This folder holds the master icon for AHV Remote Console and the files generated from it.

## Overview

`source/icon-master.svg` (1024x1024) is the single source. Every raster here and
in [`build/icons`](../../build/icons/README.md) is generated from it. The files
in `build/icons` are the ones electron-builder and the desktop app use.

## Build and test

Run these from the repository root:

```bash
pnpm build:icons     # generate PNGs, .icns, .ico, macOS modes and the icon sprite
pnpm verify:icons    # check that every file exists at the right size
make icons           # same generation through the Makefile
```

The generator is [`scripts/build-icons.mjs`](../../scripts/build-icons.mjs). `make dev` regenerates the icons before it starts the app.

## Brand palette

| Token | Hex | Use |
| --- | --- | --- |
| blue-pale | `#5ab0e8` | Gradient top |
| blue-2 | `#1668a5` | Gradient bottom |
| blue | `#0878c8` | Primary brand |
| blue-3 | `#054d8a` | Deep accents |
| green (sage) | `#689838` | Presence dot, play icon |
| paper | `#ffffff` | Monitor fills |
| ink | `#0f2235` | Dark backgrounds |

## macOS rendering modes

These are the macOS Tahoe icon appearances, written to `modes/` at 1024x1024.

| Mode | Ground | Monitors | Accents |
| --- | --- | --- | --- |
| Default | `#5ab0e8` to `#1668a5` gradient | White fills | Sage green |
| Dark | `#054d8a` to `#0f2235` gradient | White fills | Sage green |
| Tinted Light | Flat `#DDE6EF` | Ink `#15263A` | Brand blue |
| Tinted Dark | Flat `#2A3B4D` | White 90% | Pale blue |
| Clear Light | Flat `#9FBEDC` | White outlines | Sage green 90% |
| Clear Dark | Flat `#23425E` | White outlines | Sage green 90% |

## Structure

| Path | Contents |
| --- | --- |
| `source/icon-master.svg` | Master icon. |
| `icon.icns`, `icon.ico` | macOS and Windows icons. |
| `icon-<size>.png` | PNGs from 16 to 1024 px. |
| `modes/icon-<mode>.png` | macOS rendering modes. |
| `preview/icon-<size>.png` | Previews for design review. |

## Related documentation

- [Build icons](../../build/icons/README.md)
- [Building](../../docs/development/build.md)
