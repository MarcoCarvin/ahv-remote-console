import { createContext, useContext, useEffect, useMemo, useState } from 'react';

export type MotionLayer =
  | 'A1'
  | 'B1'
  | 'B2'
  | 'B3'
  | 'B4'
  | 'B5'
  | 'C1'
  | 'C2'
  | 'C3'
  | 'D1'
  | 'D2'
  | 'D3'
  | 'E1'
  | 'E2'
  | 'E3'
  | 'E4';

const DEFAULT_MOTION: Record<MotionLayer, boolean> = {
  A1: true,
  B1: true,
  B2: true,
  B3: true,
  B4: false,
  B5: false,
  C1: true,
  C2: false,
  C3: false,
  D1: true,
  D2: true,
  D3: true,
  E1: true,
  E2: true,
  E3: true,
  E4: true,
};

interface MotionContextValue {
  layers: Record<MotionLayer, boolean>;
  enabled: boolean;
  set: (layer: MotionLayer, on: boolean) => void;
}

const Ctx = createContext<MotionContextValue>({
  layers: DEFAULT_MOTION,
  enabled: true,
  set: () => {
    /* noop default; real impl provided by MotionProvider */
  },
});

export function MotionProvider({ children }: { children: React.ReactNode }) {
  const [layers, setLayers] = useState(DEFAULT_MOTION);
  const [reducedMotion, setReducedMotion] = useState(false);

  useEffect(() => {
    const mq = window.matchMedia('(prefers-reduced-motion: reduce)');
    setReducedMotion(mq.matches);
    const onChange = (e: MediaQueryListEvent) => setReducedMotion(e.matches);
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, []);

  const enabled = !reducedMotion;

  const value = useMemo<MotionContextValue>(
    () => ({
      layers: enabled
        ? layers
        : (Object.fromEntries(Object.keys(layers).map((k) => [k, false])) as Record<
            MotionLayer,
            boolean
          >),
      enabled,
      set: (layer, on) => setLayers((prev) => ({ ...prev, [layer]: on })),
    }),
    [layers, enabled],
  );

  useEffect(() => {
    const root = document.documentElement;
    for (const [k, v] of Object.entries(value.layers)) {
      root.setAttribute(`data-motion-${k.toLowerCase()}`, v ? 'on' : 'off');
    }
  }, [value.layers]);

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

// eslint-disable-next-line react-refresh/only-export-components -- intentional: shared hook co-located with provider
export function useMotion() {
  return useContext(Ctx);
}
