import { useEffect, useState } from 'react';
import type { AHVRemoteConsoleClient } from '@ahv-remote-console/sdk';

export function useFeatureFlags(client: AHVRemoteConsoleClient | null): Record<string, boolean> {
  const [flags, setFlags] = useState<Record<string, boolean>>({});

  useEffect(() => {
    if (!client) return;
    let cancelled = false;
    client
      .getFeatureFlags()
      .then((f) => {
        if (!cancelled) setFlags(f);
      })
      .catch(() => {
        /* runtime may not have the endpoint yet */
      });
    return () => {
      cancelled = true;
    };
  }, [client]);

  return flags;
}
