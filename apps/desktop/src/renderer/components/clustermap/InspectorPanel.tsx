import type { ReactNode } from 'react';
import { T } from '../clusterTokens';

/**
 * Shared right-rail chrome for the cluster-map inspectors. Lifted from
 * ClusterMap (FE-06 decomposition): the `<aside>` wrapper was identical
 * around both the EdgeInspect and NodeInspect bodies, so the scrolling
 * panel shell now lives in one place. Behavior unchanged — the caller
 * still decides WHICH inspector body to render and passes it as
 * children; this only owns the panel container.
 */
export function InspectorPanel({ children }: { children: ReactNode }) {
  return (
    <aside
      style={{
        width: 320,
        flexShrink: 0,
        borderLeft: `1px solid ${T.line}`,
        background: T.bg,
        overflowY: 'auto',
      }}
    >
      {children}
    </aside>
  );
}
