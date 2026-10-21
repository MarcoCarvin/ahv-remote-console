# Keyboard mapping

This page explains how keys you type on the host reach the guest VM, why some
characters need help, and how the console translates them. The first sections are
for operators. The later sections describe the wire format and the input engine for
developers who maintain it.

## Contents

- [Two keyboard layouts](#two-keyboard-layouts)
- [How the console solves it](#how-the-console-solves-it)
- [Choosing the guest layout](#choosing-the-guest-layout)
- [Send Keys menu](#send-keys-menu)
- [Keyboard lock](#keyboard-lock)
- [Key events on the wire](#key-events-on-the-wire)
- [Character translation](#character-translation)
- [Modifiers](#modifiers)
- [Auto-repeat](#auto-repeat)
- [Host operating systems](#host-operating-systems)
- [IME and composed text](#ime-and-composed-text)
- [Decision path for each key press](#decision-path-for-each-key-press)
- [Known limitations](#known-limitations)

## Two keyboard layouts

Two keyboard layouts are involved in every key press:

| Layout | Where it is set | What it decides |
| --- | --- | --- |
| Host layout | The operating system on your desk | Which character your physical key produces on the host |
| Guest layout | The operating system inside the VM | Which character the VM shows for the key it receives |

The console sends keys to the VM the way a physical keyboard would, and the guest
applies its own layout. When the two layouts differ, symbols come out wrong. For
example, if the VM is set to Italian and you type on a US keyboard, symbols follow the
Italian layout.

AHV adds a further constraint. AHV starts QEMU with a fixed keyboard layout,
`-k en-us`. With `-k` set, QEMU maps each incoming key symbol back to a key position
on a US keyboard and ignores any scan code. It also drops the Shift state that its own
keymap entry implies, and adds nothing. Two examples:

- A bare `@` key symbol (`0x40`) arrives in the guest as `2`.
- An Italian `/`, typed as Shift+7, arrives as `?`.

So the console controls only two things on the wire: which US key position is
pressed, and the Shift and AltGr state sent around it.

## How the console solves it

You declare the guest's layout. The console then types each character the way that
layout produces it.

1. You pick the guest layout from the globe menu in the console toolbar.
2. For each printable character you type, the input engine looks up which key and
   which Shift or AltGr level the guest layout uses for it.
3. It sends that key position as its US key symbol, with the matching Shift and AltGr
   state around it.
4. The guest's layout turns that key into the character you meant.

For an Italian guest:

| You type | The guest layout has it on | The console sends |
| --- | --- | --- |
| `@` | AltGr + `ò`; `ò` is on the US `;` position | AltGr + `;` |
| `£` | Shift + `3` | Shift + `3` |
| `è` | Unshifted, on the US `[` position | `[` |

The choice is a declaration only. It changes nothing inside the VM and types nothing
into it. VNC cannot read or set the guest's actual layout.

Characters that the declared layout cannot produce are sent as their own key symbol,
as a best effort. Most guests drop them. Producing any character regardless of layout
is a job for the agent, not the VNC keyboard channel. Accented and non-Latin input,
dead keys on a guest that does not expect them, and CJK input are best effort for the
same reason.

## Choosing the guest layout

The globe button in the console toolbar shows the current choice in its tooltip,
**Guest keyboard layout: \<name\>**. Its menu has these entries:

| Entry | Effect |
| --- | --- |
| **US** | Declares a US guest layout |
| **UK** | Declares a UK guest layout |
| **Italian** | Declares an Italian guest layout |
| **Cycle guest input language (Win+Space)** | Sends Win+Space, which switches between the guest's installed input languages. Use it when the guest's active language differs from the one you declared, then check the VM's tray. |

The default is **Italian**, because this deployment targets Italian operators. The
choice is saved per VM in browser `localStorage` and restored when you open that VM
again.

Source: [`apps/desktop/src/renderer/components/ConsoleView.tsx`](../../apps/desktop/src/renderer/components/ConsoleView.tsx),
[`apps/desktop/src/renderer/presence/KeyboardLayoutMenu.tsx`](../../apps/desktop/src/renderer/presence/KeyboardLayoutMenu.tsx).

## Send Keys menu

Some keys cannot be typed into the console directly. The host takes them (Alt+Tab,
Win), the host OS protects them (Ctrl+Alt+Del), or a Mac keyboard does not have them.
The **Send Keys** menu, behind the keyboard button in the toolbar, sends these.
Ordinary guest shortcuts are typed directly while the keyboard is captured.

Each entry sends a fixed chord of X11 key symbols with `sendKeySequence`. It presses
all keys, then releases all keys, so the guest sees one combination. A toast confirms
"Sent \<entry\>". The chords skip the guest-layout translation, because they are
commands, not typed characters.

| Group | Entry | Keys sent |
| --- | --- | --- |
| System | Ctrl + Alt + Del | Ctrl, Alt, Delete |
| System | Task Manager — Ctrl+Shift+Esc | Ctrl, Shift, Esc |
| Symbols (Mac Option layer) | `@`, `#`, `{`, `}`, `[`, `]`, `\`, `\|`, `~`, `` ` ``, `€` | The character's key symbol. Shown only on a Mac host. |
| Windows | Win — Start | Super |
| Windows | Ctrl + Esc — Start menu (no Win key) | Ctrl, Esc |
| Windows | Alt + Tab | Alt, Tab |
| Windows | Alt + Shift + Tab | Alt, Shift, Tab |
| Windows | Alt + Esc — Cycle windows | Alt, Esc |
| Windows | Alt + F4 — Close window | Alt, F4 |
| Single keys | PrintScreen, Insert, Pause / Break | The single key |
| Single keys | Num Lock, Scroll Lock | The single key; a Mac keyboard has neither |
| Function keys | F1 to F12 | The single key. F11 is labelled "host fullscreen key". |
| Linux virtual terminals | Ctrl + Alt + F1 to Ctrl + Alt + F6 | Ctrl, Alt, F*n* |
| Linux virtual terminals | Ctrl + Alt + F7 — back to GUI | Ctrl, Alt, F7 |
| Linux virtual terminals | Ctrl + Alt + Backspace — restart X | Ctrl, Alt, Backspace. Shown as a dangerous action. |

Notes:

- The Mac symbol group exists because the Mac Option layer cannot pass through as a
  layout layer. Mac and Windows layouts put these symbols on different keys. Other
  hosts type them directly, because AltGr passes through.
- The function keys are needed in BIOS, UEFI and boot menus. F11 is otherwise taken
  by the console's own fullscreen toggle, and a Mac maps several function keys to
  media and brightness controls.
- Win, Alt+Tab, Alt+Shift+Tab and Alt+Esc send the same bytes as the keyboard lock
  below. The menu is their fallback where the lock is not available.

Key symbols used by the menu:

| Key | Symbol | Key | Symbol |
| --- | --- | --- | --- |
| Ctrl | `0xffe3` | Print | `0xff61` |
| Shift | `0xffe1` | Insert | `0xff63` |
| Alt | `0xffe9` | Pause | `0xff13` |
| Win / Super | `0xffeb` | Num Lock | `0xff7f` |
| Delete | `0xffff` | Scroll Lock | `0xff14` |
| Esc | `0xff1b` | F1 to F12 | `0xffbe` to `0xffc9` |
| Tab | `0xff09` | Space | `0x20` |
| Backspace | `0xff08` | Euro | `0x20ac` |

## Keyboard lock

Clicking the console canvas captures the keyboard. Keys then go to the VM. Press
Ctrl+Alt on its own to release capture. The canvas loses focus and keys return to the
host.

Where the platform allows it, the console also arms a native keyboard grab for its
window while the keyboard is captured. The main process installs an OS keyboard hook,
stops these combinations from reaching the host, and sends them to the guest instead:

| Combination | Keys sent to the guest |
| --- | --- |
| Alt+Tab | Alt, Tab |
| Alt+Shift+Tab | Alt, Shift, Tab |
| Alt+Esc | Alt, Esc |
| Win (Super) | Super |

Only a closed list of combination names crosses the IPC bridge. The renderer maps each
name to its key symbols (`LOCK_COMBO_KEYSYMS`). The grab follows focus; there is no
button for it.

The native add-on (`packages/native-keyhook`) has a working backend on Windows only.
Its Linux and macOS backends are stubs, so the grab does not start there, or where the
add-on is missing. In those cases the console stays unlocked and these combinations stay on the Send Keys
menu. OS-protected combinations, such as Ctrl+Alt+Del, Win+L, Cmd+Tab and virtual
terminal switches, can never be grabbed and are always on the Send Keys menu.

Source: [`apps/desktop/src/renderer/input/lockCombos.ts`](../../apps/desktop/src/renderer/input/lockCombos.ts).

## Key events on the wire

The renderer speaks RFB to the runtime's loopback proxy, which forwards to AHV's QEMU
VNC server. Two key messages exist.

| Message | Size | Contents |
| --- | --- | --- |
| `KeyEvent` (RFC 6143 section 7.5.4), sent by `sendKey` | 8 bytes | Message type, down flag, padding, 32-bit X11 key symbol. No physical key information. |
| QEMU Extended Key Event (pseudo-encoding `-258`), sent by `sendExtendedKey` | 12 bytes | `CLI_MSG_QEMU` (255), subtype 0, down flag, 32-bit key symbol, 32-bit AT set-1 scan code |

Key symbols:

| Character | Key symbol |
| --- | --- |
| Latin-1 and BMP characters below `0x100` | The Unicode code point, for example `a` is `0x61` |
| Code points `0x100` and above | X.Org direct Unicode, `0x01000000 + codepoint` (`unicodeKeysym`) |

The scan code comes from `KeyboardEvent.code`, the physical key, through a
hand-written table (`eventCodeToKeycode`). The table holds the final wire bytes, with
the `0xE0` extended prefix folded into the high bit. For example, ArrowLeft
`0xE0 0x4B` is stored as `0xCB`.

### When each message is used

The renderer always advertises `-258` in `SetEncodings`. AHV's QEMU acknowledges it
with one empty pseudo-rectangle, and the client sets `extKeyAcked`. From then on
`extendedKeyEnabled()` returns true and `InputEngine.send` uses `sendExtendedKey` for
ordinary physical keys. The `qemu_ext_key_event` feature flag forces this on even
without the acknowledgement.

So in normal AHV operation, ordinary key presses carry both the key symbol and the scan
code. Synthetic keys, such as modifiers the engine sends itself, IME text and layout
translation, use plain `sendKey`.

Because AHV runs QEMU with `-k en-us`, the key symbol decides the result in both
message types, not the scan code. The scan code is there for a QEMU without `-k`, for
other VNC servers, and for firmware and UEFI screens that read raw scan codes before
any layout loads. The layout translation is therefore applied whether or not `-258`
is active.

In extended-key mode, AHV's QEMU drops an extended key event whose scan code is `0`.
It reads the scan code first and does not fall back to the key symbol. Synthetic keys
therefore look up a real AT set-1 scan code for their key symbol (`SYNTHETIC_KEYCODE`)
instead of sending `0`.

Source: [`apps/desktop/src/renderer/rfb/client.ts`](../../apps/desktop/src/renderer/rfb/client.ts),
[`apps/desktop/src/renderer/rfb/keysyms.ts`](../../apps/desktop/src/renderer/rfb/keysyms.ts),
[`apps/desktop/src/renderer/input/Keycodes.ts`](../../apps/desktop/src/renderer/input/Keycodes.ts).

## Character translation

`ConsoleView` passes the declared layout to the engine with `setGuestLayout` when the
console opens and whenever the choice changes. The engine's own default is `us`, but
the menu's default `it` is passed at mount, so Italian is the effective default.

For each printable character (`keysym !== 0` and `e.key.length === 1`), the engine
calls `guestKeyFor`. Each layout table maps a character to
`{ base, shift?, altgr? }`: the US key symbol of the key that carries it, plus the
level.

`typeGuestKey` then sends a complete sequence as plain `sendKey` events:

1. AltGr (right Alt) down, if needed.
2. Shift set to the state the layout needs.
3. Press and release the base key symbol.
4. Shift restored.
5. AltGr up, if it was pressed.

The host's matching key-up is swallowed by its physical `code`, because the key-down
already sent the whole sequence.

### Passthrough

When the host key already matches what the sequence would send, with the same base
position, the same Shift state and no AltGr, the engine sends it unchanged as a real
key-down and key-up (`enUsBaseOf`). This keeps real key-down and key-up behaviour, so
guest auto-repeat works.

Letters whose case comes from the host's CapsLock are also sent unchanged. Forcing
Shift on top of an engaged guest CapsLock would invert the case.

### Layout tables

| ID | Name | Offered in the menu | Status |
| --- | --- | --- | --- |
| `us` | US | Yes | Complete for its printable set |
| `uk` | UK | Yes | Complete for its printable set |
| `it` | Italian | Yes | Complete for its printable set |
| `de` | German | No | Complete for its printable set. Swaps `y` and `z`. |
| `fr` | French | No | Best effort, not yet checked on a live guest. AZERTY: swaps `a`/`q`, `z`/`w`, `m`/`;`. |
| `es` | Spanish | No | Best effort, not yet checked on a live guest |

The `de`, `fr` and `es` tables stay in the code so a choice saved earlier keeps
working. They are not offered for new choices. An unknown layout ID becomes `us`
(`asGuestLayoutId`). Letter swaps are handled by `letterSwaps`.

Dead keys (accent composition) are not in the layout tables. A dead-key sequence
cannot be driven blindly from one key symbol. A key that arrives as `e.key === 'Dead'`
is sent as an X11 dead key symbol chosen by its physical `code`.

Source: [`apps/desktop/src/renderer/input/GuestLayouts.ts`](../../apps/desktop/src/renderer/input/GuestLayouts.ts),
[`apps/desktop/src/renderer/input/InputEngine.ts`](../../apps/desktop/src/renderer/input/InputEngine.ts).

## Modifiers

The engine tracks modifier state as its belief about what the guest sees. It cannot
read the guest's state, so every decision is local.

| Behaviour | Detail |
| --- | --- |
| Logical tracking | State is kept per logical modifier (control, shift, alt, meta). Left and right halves are merged, so pressing one side and releasing the other cannot leave a stuck modifier. |
| Release | The engine always releases the left variant. The guest holds modifiers as a set, so releasing the left half clears Shift even if the right half was pressed. |
| AltGr | `AltGraph` is not mapped to Alt. Mapping it would make AltGr+key send `Alt_L` and trigger guest menu shortcuts. AltGr characters go through the layout translation with the right AltGr state and no command Alt. |
| Windows AltGr | Windows sends a phantom left Ctrl press just before right Alt when AltGr is pressed. The engine holds a new left Ctrl back. If right Alt follows within 100 ms, it is AltGr: one `ISO_Level3_Shift`, no Ctrl. Otherwise the Ctrl was real and is sent (`interruptAltGr`). Linux reports a real `AltGraph`, so this applies to Windows only. |
| Stuck modifiers | Every key event carries the host's current modifier flags. `reconcileHeldModifiers` releases any modifier the guest still holds that the host has already released. It only releases, never presses, so it cannot fight a modifier that is really held. |
| Focus loss | On window blur or `visibilitychange`, `releaseAllModifiers` releases every held modifier, the synthetic Cmd-to-Ctrl, and every key still in `downKeys`, with real scan codes. Cmd+Tab or Alt+Tab away cannot leave a key stuck in the guest. |
| CapsLock and NumLock | On every key press, `syncLockKeys` compares `getModifierState` with the last state sent and sends a toggle if they differ. macOS and some Linux desktops do not send reliable events for these keys. NumLock sync is skipped on a Mac, which has no NumLock key. |

Source: [`apps/desktop/src/renderer/input/Modifiers.ts`](../../apps/desktop/src/renderer/input/Modifiers.ts).

## Auto-repeat

QEMU's emulated keyboard does not repeat keys by itself. A held key repeats in the
guest only if the renderer sends each browser repeat as another key-down, followed by
one key-up on release, as a hardware keyboard does.

| Key type | Browser repeats |
| --- | --- |
| Printable, navigation and editing keys | Sent as further key-downs |
| Modifiers and `AltGraph` | Dropped; a held modifier is one key-down |
| Any key, when `suppressRepeat` is set | Dropped. The option defaults to off. |

Repeat works only for keys sent as real key-down and key-up, that is the passthrough
path. A character sent by `typeGuestKey` is a complete press and release on every
key-down, so it repeats only as often as the host sends repeat events.

## Host operating systems

The host OS comes from Electron's `process.platform` through the contextBridge, then
`navigator.userAgentData.platform`, then `navigator.platform`. The engine reads it
once when it starts.

Source: [`apps/desktop/src/renderer/input/HostOS.ts`](../../apps/desktop/src/renderer/input/HostOS.ts).

### Mac host

| Input | Result |
| --- | --- |
| Cmd alone | Suppressed. Use **Send Keys** for the Win key. |
| Option alone | Suppressed. On a Mac, Option selects symbols in the host layout; sending it as Alt put a Windows guest into menu mode. |
| Cmd + printable key | Ctrl + the same key. A synthetic `Control_L` is held (`fakeCtrl`). Case follows `e.key`, so Cmd+Shift+Z sends Ctrl+Shift+Z. Cmd+C, Cmd+V, Cmd+X and Cmd+Z work in a PC guest. |
| Cmd + Left / Right | Home / End |
| Cmd + Up / Down | Ctrl+Home / Ctrl+End |
| Option + Left / Right | Ctrl+Left / Ctrl+Right |
| Option + Backspace / Delete | Ctrl+Backspace / Ctrl+Delete |
| Shift with any of the navigation keys above | Extends the selection |

The navigation chords (`macNavChord`) are sent as complete sequences with
`sendKeySequence`, and the key-up is swallowed by `code`.

macOS does not deliver key-up events for other keys while Cmd is held. When Cmd is
released, the engine goes through `downKeys` and releases every key still down, except
modifiers, whose key-up macOS does deliver. This prevents a key that keeps repeating in
the guest.

### Windows host

AltGr handling applies, as described under [Modifiers](#modifiers). There is no
Cmd-to-Ctrl change and no Option suppression. Ctrl and Alt are sent as themselves.

### Linux host

Linux is detected but has no separate branch in the engine. AltGr arrives as a real
`AltGraph`, so no phantom-Ctrl handling is needed. There is no Cmd-to-Ctrl change.

The Ctrl+Alt release chord applies on every host. `isReleaseChord` lets a real
Ctrl+Alt+T style shortcut through and does not treat an AltGr sequence as the release
chord.

## IME and composed text

While the OS input method or a dead-key sequence is composing, raw key-down and key-up
events are suppressed. The engine also catches `keyCode === 229`. When the browser
fires `compositionend`, the engine types the final string in one go.

`typeLiteral` sends composed text one code point at a time as plain key events.
Latin-1 characters go as themselves, code points `0x100` and above use the X.Org
direct Unicode offset, and newline and Tab use their X11 key symbols. Whether the guest
shows a given character depends on its keymap. Over VNC this is best effort.

A bare shifted-symbol key symbol is not shifted automatically by Prism's QEMU: `@`
(`0x40`) arrived as `2`. For US shifted symbols, `typeLiteral` therefore sends Shift
plus the base key (`US_SHIFTED_BASE`).

## Decision path for each key press

`handleKeyDown` in the input engine checks these in order:

1. Composing, or `keyCode` 229: suppress.
2. `e.repeat` on a modifier, or `suppressRepeat` set: drop.
3. Windows AltGr: hold or resolve the pending left Ctrl.
4. Ctrl+Alt alone: release capture.
5. Sync lock keys, record this key's scan code, release stuck modifiers.
6. Mac Cmd or Option with an arrow or delete key: send the Windows navigation chord.
7. Mac Cmd or Option alone: suppress. Mac Cmd with a printable key: send Ctrl with it.
8. Compute the key symbol. If the declared layout types this printable character
   differently: `typeGuestKey`, then swallow the key-up.
9. Otherwise send the key unchanged, with the scan code in extended-key mode, and
   record it in `downKeys` for the matching release.

## Known limitations

- Host-layout recovery in `HostOS.ts` is unused. `detectHostKeyboardLayout`,
  `hostKeyboardLayout` and the referenced `recoverLayoutKeysym` describe correcting
  Italian host symbol keys against the guest's US keymap. No renderer code outside
  `HostOS.ts` calls them, and the `InputEngine` header states there is no host-layout
  remapping. Character fidelity comes from the declared guest layout only.
- There are two layout lists. `KEYBOARD_LAYOUTS` in `KeyboardLayoutMenu.tsx` lists ten
  IDs, and the `KeyboardLayoutMenu` component shows all ten. That component is not
  mounted anywhere; only its `useKeyboardLayout` hook is used. The live picker is the
  globe menu with `us`, `uk` and `it`. IDs outside the six layout tables, such as
  `pt-br`, `ch-de` or `jp`, would become `us` if selected.
- Some code comments, in the `-258` acknowledgement handler and in `Keycodes.ts`, say
  the scan code lets the guest apply its own keymap. Under AHV's `-k en-us` that does
  not happen; the key symbol decides the result.
- The `qemu_ext_key_event` comment in `ConsoleView.tsx` describes extended key events
  as off by default. The RFB client enables them whenever AHV's QEMU acknowledges
  `-258`; the flag only forces them on.

## Related documentation

- [Wire protocol reference](protocol.md)
- [Connecting and using the clipboard](../operations/connect-and-clipboard.md)
- [Capture and input design](../design/capture-and-input.md)
- [Console controls design](../design/console-controls.md)
- [Glossary](glossary.md)
