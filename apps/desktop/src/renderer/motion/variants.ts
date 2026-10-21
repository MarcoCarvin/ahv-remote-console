/**
 * Route-level Framer Motion variants for the v2 motion system.
 *
 * One <AnimatePresence mode="wait"> at the Shell level keyed on a
 * Route enum picks the right variant set based on:
 *   - source route (where we're exiting from)
 *   - target route (where we're entering)
 *   - navigation direction (forward / back)
 *
 * Patterns per the transitions design brief:
 *
 *   T1 Fade-through      — Splash → Sign-in. Unrelated contexts.
 *                          Out 150ms accel, in 250ms decel + 8px y.
 *   T2/T3 Shared Axis X  — same-task navigation (sign-in ↔ endpoint
 *                          ↔ lobby). Direction-aware translate.
 *   T4 Container Transform — Lobby tile → Console via layoutId.
 *                            Wired separately on the m.div elements.
 *
 * "Never overlap" semantics come from mode="wait" on AnimatePresence;
 * variants here only describe initial / animate / exit.
 */

import type { Variants } from 'framer-motion';
import { duration, ease } from './tokens';
import type { Direction } from '../app/routeReducer';

/**
 * T1: Splash → Sign-in. Used ONLY for that pair and on hard auth
 * reset (e.g., session-expired → forced sign-in). Per the brief:
 * a brief gap of pure background colour, no overlap.
 */
export const fadeThroughVariants: Variants = {
  initial: { opacity: 0, y: 8 },
  animate: {
    opacity: 1,
    y: 0,
    transition: { duration: duration.medium2, ease: ease.decel },
  },
  exit: {
    opacity: 0,
    transition: { duration: duration.short3, ease: ease.accel },
  },
};

/**
 * T2/T3: Shared Axis X — same-task forward / back navigation.
 * Direction is closure-captured: 'forward' means content slides in
 * from the right (+24px → 0); 'back' inverts the sign so the user
 * perceives a return motion.
 */
export const sharedAxisX = (dir: Direction): Variants => {
  const enterX = dir === 'forward' ? 24 : -24;
  const exitX = dir === 'forward' ? -24 : 24;
  return {
    initial: { opacity: 0, x: enterX },
    animate: {
      opacity: 1,
      x: 0,
      transition: { duration: duration.medium2, ease: ease.decel },
    },
    exit: {
      opacity: 0,
      x: exitX,
      transition: { duration: duration.short4, ease: ease.accel },
    },
  };
};

/**
 * Reduced-motion variant set — opacity only, near-zero duration. We
 * still need `initial/animate/exit` so AnimatePresence has something
 * to do (otherwise children unmount instantly with no class for the
 * a11y tree). MotionConfig reducedMotion="user" auto-swaps to this
 * for any element NOT using layoutId; we apply the same effect to
 * route-level variants too so behaviour is symmetric.
 */
export const reducedMotionVariants: Variants = {
  initial: { opacity: 0 },
  animate: { opacity: 1, transition: { duration: 0.01 } },
  exit: { opacity: 0, transition: { duration: 0.01 } },
};
