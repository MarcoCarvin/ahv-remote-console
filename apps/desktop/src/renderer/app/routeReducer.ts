/**
 * Route reducer for the v2 route-level transition system.
 *
 * Per the spec, AnimatePresence keys on a route enum (NOT pathname)
 * so query-param churn on the lobby doesn't trigger exit/enter
 * cycles. Direction is tracked here too — deep-link arrivals pass an
 * explicit direction so the variant set still gets the right slide
 * sign.
 *
 * Mapping from existing AppStage to Route:
 *   loading           → 'splash'
 *   no-endpoints      → 'no-endpoints'
 *   add-endpoint      → 'add-endpoint'
 *   select-endpoint   → 'endpoint'
 *   sign-in           → 'signin'
 *   ready (no tab)    → 'lobby'
 *   ready (tab open)  → 'console'
 */

export type Route =
  | 'splash'
  | 'no-endpoints'
  | 'add-endpoint'
  | 'endpoint'
  | 'signin'
  | 'lobby'
  | 'console';

export type Direction = 'forward' | 'back';

/**
 * Canonical linear ordering of routes for forward / back direction
 * inference. Routes earlier in the array are "lower" in the task
 * flow; moving to a later route is 'forward'.
 *
 * 'console' is intentionally past 'lobby' so opening a VM is forward
 * navigation and closing it is back.
 */
const ROUTE_ORDER: Route[] = [
  'splash',
  'no-endpoints',
  'add-endpoint',
  'endpoint',
  'signin',
  'lobby',
  'console',
];

export interface RouteState {
  route: Route;
  direction: Direction;
}

/**
 * Infer direction from the linear route order.
 * Forward if target is later than current; back otherwise. Same-
 * route navigations are treated as 'forward' (no-op visually).
 */
export function inferDirection(from: Route, to: Route): Direction {
  return ROUTE_ORDER.indexOf(to) >= ROUTE_ORDER.indexOf(from) ? 'forward' : 'back';
}
