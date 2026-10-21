import { useCallback, useEffect, useRef, useState } from 'react';
import type { Endpoint } from '@ahv-remote-console/protocol';
import type { NodePosition } from '../../app/recentStore';
import { NODE_W, NODE_H, MIN_ZOOM, MAX_ZOOM } from './constants';

export interface MapView {
  x: number;
  y: number;
  zoom: number;
}

export interface UseMapViewportReturn {
  viewportRef: React.RefObject<HTMLDivElement | null>;
  view: MapView;
  setView: React.Dispatch<React.SetStateAction<MapView>>;
  fit: () => void;
  onWheel: (e: React.WheelEvent) => void;
  toWorld: (clientX: number, clientY: number) => { x: number; y: number };
}

/**
 * Viewport pan/zoom state machine for the cluster map. Moved verbatim
 * from ClusterMap (FE-06 decomposition): same state, same effect
 * (fit-once after first layout), same wheel-to-zoom-under-cursor maths,
 * same world-space projection. No behavior change — the hook is invoked
 * at the exact point in ClusterMap's render where this block previously
 * lived, so the React hook-call order is unchanged.
 */
export function useMapViewport(
  endpoints: Endpoint[],
  pos: Record<string, NodePosition>,
  defaults: Record<string, NodePosition>,
): UseMapViewportReturn {
  const viewportRef = useRef<HTMLDivElement>(null);
  const [view, setView] = useState<MapView>({ x: 40, y: 40, zoom: 1 });

  const fit = useCallback(() => {
    const vp = viewportRef.current;
    if (!vp || endpoints.length === 0) return;
    let minX = Infinity,
      minY = Infinity,
      maxX = -Infinity,
      maxY = -Infinity;
    for (const ep of endpoints) {
      const p = pos[ep.id] ?? defaults[ep.id]!;
      minX = Math.min(minX, p.x);
      minY = Math.min(minY, p.y);
      maxX = Math.max(maxX, p.x + NODE_W);
      maxY = Math.max(maxY, p.y + NODE_H);
    }
    const pad = 80;
    const w = maxX - minX + pad * 2;
    const h = maxY - minY + pad * 2;
    const zoom = Math.max(
      MIN_ZOOM,
      Math.min(MAX_ZOOM, Math.min(vp.clientWidth / w, vp.clientHeight / h)),
    );
    setView({
      zoom,
      x: vp.clientWidth / 2 - (minX - pad + w / 2) * zoom,
      y: vp.clientHeight / 2 - (minY - pad + h / 2) * zoom,
    });
  }, [endpoints, pos, defaults]);

  // Fit once after the first layout is known.
  const fittedRef = useRef(false);
  useEffect(() => {
    if (!fittedRef.current && endpoints.length > 0 && Object.keys(pos).length > 0) {
      fittedRef.current = true;
      fit();
    }
  }, [endpoints, pos, fit]);

  const onWheel = useCallback((e: React.WheelEvent) => {
    const vp = viewportRef.current;
    if (!vp) return;
    const rect = vp.getBoundingClientRect();
    const cx = e.clientX - rect.left;
    const cy = e.clientY - rect.top;
    setView((v) => {
      const factor = e.deltaY < 0 ? 1.12 : 1 / 1.12;
      const zoom = Math.max(MIN_ZOOM, Math.min(MAX_ZOOM, v.zoom * factor));
      const k = zoom / v.zoom;
      // keep the point under the cursor fixed
      return { zoom, x: cx - (cx - v.x) * k, y: cy - (cy - v.y) * k };
    });
  }, []);

  const toWorld = useCallback(
    (clientX: number, clientY: number) => {
      const vp = viewportRef.current!;
      const rect = vp.getBoundingClientRect();
      return {
        x: (clientX - rect.left - view.x) / view.zoom,
        y: (clientY - rect.top - view.y) / view.zoom,
      };
    },
    [view],
  );

  return { viewportRef, view, setView, fit, onWheel, toWorld };
}
