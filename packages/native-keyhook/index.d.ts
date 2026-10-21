/**
 * @ahv-remote-console/native-keyhook
 *
 * Per-OS native keyboard grab for VMware-style WINDOWED keyboard auto-grab.
 * Loaded ONLY in the Electron main process (never the sandboxed renderer).
 *
 * The grab installs an OS keyboard hook on a dedicated thread, SUPPRESSES a
 * FIXED set of host-reserved combos from the host, and forwards each as a
 * CLOSED enum value. Raw scancodes/keysyms never cross into JS. The combo set
 * is hard-coded in native code; it is never caller-supplied.
 *
 * Graceful by design: if the prebuilt binary is absent (no toolchain / unbuilt
 * dev box) the module still loads — `available()` returns false and `arm()`
 * returns false, so the caller falls back to the Send-Keys menu.
 */

/** The fixed set of host-reserved combos the native hook can suppress + forward. */
export type LockCombo = 'AltTab' | 'AltShiftTab' | 'AltEsc' | 'Super';

/**
 * Whether this platform can honor an active keyboard grab at all, WITHOUT
 * installing the hook. False on Wayland (no core active grab) and on macOS when
 * Accessibility (TCC) is not granted, and when the native binary is missing.
 */
export function available(): boolean;

/**
 * Install the per-OS hook on its own thread. `onCombo` is invoked (on the JS
 * thread, via a thread-safe function) with a CLOSED enum value for each
 * suppressed host-reserved combo. Returns false if the platform cannot grab
 * (Wayland / macOS TCC denied / binary absent) — the caller then keeps
 * Send-Keys as the path for those combos. Idempotent: a second arm() while
 * already armed returns true without re-installing.
 */
export function arm(onCombo: (combo: LockCombo) => void): boolean;

/** Remove the hook and stop its run-loop/message-pump thread. Idempotent. */
export function release(): void;
