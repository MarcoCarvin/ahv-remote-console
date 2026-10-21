# Application icons

This folder holds the icon set that electron-builder packages and the desktop app loads at run time.

## Overview

The files are generated from the master icon
[`assets/icon/source/icon-master.svg`](../../assets/icon/source/icon-master.svg)
and checked in. They use the brand accent gradient on a transparent or
near-black background. The installers take their icons from here, and
`apps/desktop/electron-builder.yml` copies this folder into the app's `icons/`
resources for the dock, window and tray icons.

## Build and test

Run these from the repository root to regenerate and check the set:

```bash
pnpm build:icons
pnpm verify:icons
```

The generator is [`scripts/build-icons.mjs`](../../scripts/build-icons.mjs).

## Structure

| File | Use |
| --- | --- |
| `ahv-remote-console.ico` | Windows installer and window icon. |
| `ahv-remote-console.icns` | macOS app bundle. |
| `ahv-remote-console-dock.png` | macOS dock icon at run time. |
| `tray-iconTemplate.png` | macOS tray template image, 16x16. |
| `tray-icon.png` | Windows and Linux tray, 32x32. |
| `ahv-remote-console-512.png` | Linux AppImage and desktop file. |
| `<size>x<size>.png` | Linux icon sizes, 16 to 1024 px. |
| `icon.svg` | SVG copy of the icon. |

## Related documentation

- [App icon source and palette](../../assets/icon/README.md)
- [Building](../../docs/development/build.md)
