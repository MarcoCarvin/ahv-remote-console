# Native keyboard hook

`@ahv-remote-console/native-keyhook` grabs host-reserved key combos such as Alt+Tab while a console window has focus, like VMware-style windowed keyboard grab.

## Overview

The module loads only in the desktop app's main process, never in the renderer.
When armed, it installs an OS keyboard hook on its own thread. It hides a fixed
set of combos from the host and passes each one to JavaScript as a closed enum
value. Raw scancodes and keysyms never reach JavaScript. The combo set is
hard-coded in native code, not supplied by the caller. See
[`docs/architecture/overview.md`](../../docs/architecture/overview.md) for where
the desktop app sits in the system.

The module always loads, even without a compiled binary. If no prebuilt `.node`
file is present, `available()` and `arm()` return `false` and the app falls back
to the **Send Keys** menu. Nothing throws.

| OS | Source | Can grab | Send Keys only |
| --- | --- | --- | --- |
| Windows | `src/win.cc` (`WH_KEYBOARD_LL`) | Alt+Tab, Alt+Shift+Tab, Alt+Esc, Win | Ctrl+Alt+Del, Win+L |
| Linux | `src/linux.cc` (stub) | none | everything |
| macOS | `src/mac.mm` (stub) | none | everything; Cmd+Tab, Cmd+Space and Mission Control are reserved by WindowServer |

Only Windows has an active grab. On Linux, a keyboard grab on the root window
would starve the Electron renderer of keystrokes. The right approach is passive
per-combo `XGrabKey` grabs, which need a real X server to test. On macOS, WindowServer reserves the useful combos
and a working build needs a Developer ID certificate. The header comments in the
stub files explain both.

### Integrity check

The addon runs native code in the main process, which holds the IPC secret. So
[`integrity.js`](integrity.js) checks the binary's SHA-256 before loading it.

- `build:native` writes `integrity.json`, which maps `<platform>-<arch>` to the
  SHA-256 of `node.napi.node`.
- `integrity.json` stays packed inside the asar. The binaries it pins live in
  `app.asar.unpacked`, so replacing a `.node` file does not also replace the pin.
- In a shipped build, only the pinned prebuild loads. A missing pin, missing file
  or hash mismatch disables the hook. The loader never falls back to an unpinned
  binary.
- In a dev build with no `integrity.json`, the loader accepts a prebuild or the
  local `node-gyp` output without a check.

> [!NOTE]
> This is defense in depth, not a sandbox. An attacker who can rewrite the whole
> asar can bypass the pin. On macOS, Library Validation and the ASAR integrity
> fuse add protection.

## Usage

```js
const keyhook = require('@ahv-remote-console/native-keyhook');
if (keyhook.available()) {
  keyhook.arm((combo) => {
    // combo is 'AltTab' | 'AltShiftTab' | 'AltEsc' | 'Super'
  });
}
keyhook.release();
```

## Build and test

The package ships prebuilt binaries. `pnpm install` does not compile it, so installs need no C++ toolchain.

```bash
pnpm --filter @ahv-remote-console/native-keyhook build:native   # prebuildify + integrity.json
pnpm --filter @ahv-remote-console/native-keyhook build:debug    # local node-gyp debug build
pnpm --filter @ahv-remote-console/native-keyhook test           # node --test
```

There is no `build` script on purpose, so workspace-wide `pnpm -r build` skips this package on runners without a toolchain. CI produces the prebuild with `build:native`. Prebuilds go to `prebuilds/<platform>-<arch>/node.napi.node`, and electron-builder unpacks them with `asarUnpack: '**/*.node'`. The native hooks have no unit tests; they need a real Win32 message pump or X11 server and Electron.

## Structure

| Path | Contents |
| --- | --- |
| `index.js`, `index.d.ts` | Loader and API: `available()`, `arm()`, `release()`. |
| `integrity.js` | SHA-256 check before loading the binary. |
| `src/` | Native code per OS and the shared `keyhook.cc`. |
| `scripts/gen-integrity.js` | Writes `integrity.json`. |
| `test/` | Tests for the integrity check. |
| `binding.gyp` | `node-gyp` build configuration. |

## Related documentation

- [Keyboard reference](../../docs/reference/keyboard.md)
- [Desktop app architecture](../../docs/architecture/desktop.md)
- [Architecture overview](../../docs/architecture/overview.md)
