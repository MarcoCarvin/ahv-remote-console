import { createContext, useContext } from 'react';
import type { AHVRemoteConsoleClient } from '@ahv-remote-console/sdk';

export const ClientContext = createContext<AHVRemoteConsoleClient | null>(null);

export function useClient(): AHVRemoteConsoleClient {
  const c = useContext(ClientContext);
  if (!c) throw new Error('ClientContext not initialized');
  return c;
}
