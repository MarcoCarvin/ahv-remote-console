/**
 * v2 motion tokens — Material 3 durations + Apple-style emphasis
 * eases. Mirror of the CSS variables in packages/ui/src/tokens.css.
 *
 * Use these for Framer Motion variants (the array form). For CSS
 * animations, read from the `--dur-*` / `--ease-*` variables instead.
 */

export const duration = {
  short3: 0.15, // 150ms — crossfade
  short4: 0.2, // 200ms — exit
  medium2: 0.3, // 300ms — enter
  medium4: 0.4, // 400ms — shared element
} as const;

export const ease = {
  standard: [0.2, 0, 0, 1] as const, // default
  decel: [0.05, 0.7, 0.1, 1] as const, // entering (Linear-ish)
  accel: [0.3, 0, 0.8, 0.15] as const, // exiting
} as const;

/**
 * Spring for T4 container transform (Lobby tile → Console viewport).
 * Stiffness 380 + damping 38 lands in ~420ms — matches Material 3
 * `medium4` for shared element morphs.
 */
export const containerSpring = {
  type: 'spring' as const,
  stiffness: 380,
  damping: 38,
};
