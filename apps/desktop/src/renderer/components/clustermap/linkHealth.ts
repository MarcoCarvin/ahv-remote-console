import { T } from '../clusterTokens';

/**
 * Operator-link health, DERIVED from the two endpoints' reachability.
 * Moved verbatim from ClusterMap (FE-06 decomposition) so the inspector
 * panels can share it without a circular import back into ClusterMap.
 */
export type LinkHealth = 'ok' | 'degraded' | 'down';

export function linkHealthMeta(h: LinkHealth): { color: string; label: string } {
  switch (h) {
    case 'ok':
      return { color: T.green, label: 'Healthy' };
    case 'degraded':
      return { color: T.amber, label: 'Degraded' };
    default:
      return { color: T.red, label: 'Down' };
  }
}
