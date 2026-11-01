import { createVoiceMusicAdapter } from '../../../packages/client-core/src/voice/music';
import type { CoreClient } from '../../../packages/client-core/src/bridge';

const base = process.argv[2]!;
const config = await (await fetch(`${base}/fixture`)).json();
let startedAt: number | null = null;
const client = {
  get(key: string) {
    if (key === 'account') return { phase: 'ready', user: { id: config.user }, deviceId: config.device, instance: { id: config.instance } };
    if (key === 'communities') return [{ id: config.community }];
    return null;
  },
  subscribe() { return () => {}; },
  async call(input: Record<string, unknown>) {
    const response = await fetch(`${base}/core`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ...input, startedAt }) });
    if (!response.ok) throw new Error(await response.text());
    const result = await response.json();
    if (input.m === 'readBotInteractionResponses') {
      for (const row of result.responses) row.data = new Uint8Array(row.data);
    }
    return result;
  },
} as unknown as CoreClient;
const adapter = createVoiceMusicAdapter(client, config.community);
await adapter.refresh(config.channel);
await fetch(`${base}/arm`, { method: 'POST' });
startedAt = Date.now();
if (config.operation === 'pause') await adapter.pause(config.channel, true);
else await adapter.skip(config.channel);
process.stdout.write('control confirmed\n');
