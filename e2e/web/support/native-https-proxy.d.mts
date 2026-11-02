import type { Server } from 'node:https';
export function createNativeHttpsProxy(options: { key: Buffer; cert: Buffer; upstreamPort: number; port?: number }): Promise<Server>;
